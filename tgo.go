
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
	"llvm.org/llvm/bindings/go/llvm"
)

func init() {
	llvm.InitializeAllTargets()
	llvm.InitializeAllTargetMCs()
	llvm.InitializeAllTargetInfos()
	llvm.InitializeAllAsmParsers()
	llvm.InitializeAllAsmPrinters()
}

type Compiler struct {
	triple          string
	mod             llvm.Module
	ctx             llvm.Context
	builder         llvm.Builder
	machine         llvm.TargetMachine
	intType         llvm.Type
	stringLenType   llvm.Type
	stringType      llvm.Type
	printstringFunc llvm.Value
	printintFunc    llvm.Value
	printspaceFunc  llvm.Value
	printnlFunc     llvm.Value
}

type Frame struct {
	pkgPrefix string
	name      string                   // full name, including package
	params    map[*ssa.Parameter]int   // arguments to the function
	locals    map[ssa.Value]llvm.Value // local variables
	blocks    map[*ssa.BasicBlock]llvm.BasicBlock
	phis      []Phi
}

type Phi struct {
	ssa  *ssa.Phi
	llvm llvm.Value
}

func NewCompiler(pkgName, triple string) (*Compiler, error) {
	c := &Compiler{
		triple: triple,
	}

	target, err := llvm.GetTargetFromTriple(triple)
	if err != nil {
		return nil, err
	}
	c.machine = target.CreateTargetMachine(triple, "", "", llvm.CodeGenLevelDefault, llvm.RelocDefault, llvm.CodeModelDefault)

	c.mod = llvm.NewModule(pkgName)
	c.ctx = c.mod.Context()
	c.builder = c.ctx.NewBuilder()

	// Depends on platform (32bit or 64bit), but fix it here for now.
	c.intType = llvm.Int32Type()
	c.stringLenType = llvm.Int32Type()

	// Length-prefixed string.
	c.stringType = llvm.StructType([]llvm.Type{c.stringLenType, llvm.PointerType(llvm.Int8Type(), 0)}, false)

	printstringType := llvm.FunctionType(llvm.VoidType(), []llvm.Type{c.stringType}, false)
	c.printstringFunc = llvm.AddFunction(c.mod, "__go_printstring", printstringType)
	printintType := llvm.FunctionType(llvm.VoidType(), []llvm.Type{c.intType}, false)
	c.printintFunc = llvm.AddFunction(c.mod, "__go_printint", printintType)
	printspaceType := llvm.FunctionType(llvm.VoidType(), nil, false)
	c.printspaceFunc = llvm.AddFunction(c.mod, "__go_printspace", printspaceType)
	printnlType := llvm.FunctionType(llvm.VoidType(), nil, false)
	c.printnlFunc = llvm.AddFunction(c.mod, "__go_printnl", printnlType)

	return c, nil
}

func (c *Compiler) Parse(pkgName string) error {
	tripleSplit := strings.Split(c.triple, "-")

	config := loader.Config {
		// TODO: TypeChecker.Sizes
		Build: &build.Context {
			GOARCH:      tripleSplit[0],
			GOOS:        tripleSplit[2],
			GOROOT:      ".",
			CgoEnabled:  true,
			UseAllFiles: false,
			Compiler:    "gc", // must be one of the recognized compilers
			BuildTags:   []string{"tgo"},
		},
		AllowErrors: true,
	}
	config.Import(pkgName)
	lprogram, err := config.Load()
	if err != nil {
		return err
	}

	// TODO: pick the error of the first package, not a random package
	for _, pkgInfo := range lprogram.AllPackages {
		fmt.Println("package:", pkgInfo.Pkg.Name())
		if len(pkgInfo.Errors) != 0 {
			return pkgInfo.Errors[0]
		}
	}

	program := ssautil.CreateProgram(lprogram, ssa.SanityCheckFunctions | ssa.BareInits)
	program.Build()
	// TODO: order of packages is random
	for _, pkg := range program.AllPackages() {
		fmt.Println("package:", pkg.Pkg.Path())

		// Make sure we're walking through all members in a constant order every
		// run.
		memberNames := make([]string, 0)
		for name := range pkg.Members {
			memberNames = append(memberNames, name)
		}
		sort.Strings(memberNames)

		frames := make(map[*ssa.Function]*Frame)

		// First, build all function declarations.
		for _, name := range memberNames {
			member := pkg.Members[name]

			pkgPrefix := pkg.Pkg.Path()
			if pkg.Pkg.Name() == "main" {
				pkgPrefix = "main"
			}

			switch member := member.(type) {
			case *ssa.Function:
				frame, err := c.parseFuncDecl(pkgPrefix, member)
				if err != nil {
					return err
				}
				frames[member] = frame
			case *ssa.NamedConst:
				val, err := c.parseConst(member.Value)
				if err != nil {
					return err
				}
				global := llvm.AddGlobal(c.mod, val.Type(), pkgPrefix + "." +  member.Name())
				global.SetInitializer(val)
				global.SetGlobalConstant(true)
				if ast.IsExported(member.Name()) {
					global.SetLinkage(llvm.PrivateLinkage)
				}
			case *ssa.Global:
				typ, err := c.getLLVMType(member.Type())
				if err != nil {
					return err
				}
				global := llvm.AddGlobal(c.mod, typ, pkgPrefix + "." +  member.Name())
				if ast.IsExported(member.Name()) {
					global.SetLinkage(llvm.PrivateLinkage)
				}
			case *ssa.Type:
				// TODO
			default:
				return errors.New("todo: member: " + fmt.Sprintf("%#v", member))
			}
		}

		// Now, add definitions to those declarations.
		for _, name := range memberNames {
			member := pkg.Members[name]
			fmt.Println("member:", member.Token(), member)

			if member, ok := member.(*ssa.Function); ok {
				if member.Blocks == nil {
					continue // external function
				}
				err := c.parseFunc(frames[member], member)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (c *Compiler) getLLVMType(goType types.Type) (llvm.Type, error) {
	fmt.Println("        type:", goType)
	switch typ := goType.(type) {
	case *types.Basic:
		switch typ.Kind() {
		case types.Bool:
			return llvm.Int1Type(), nil
		case types.Int:
			return c.intType, nil
		case types.Int32:
			return llvm.Int32Type(), nil
		case types.String:
			return c.stringType, nil
		case types.UnsafePointer:
			return llvm.PointerType(llvm.Int8Type(), 0), nil
		default:
			return llvm.Type{}, errors.New("todo: unknown basic type: " + fmt.Sprintf("%#v", typ))
		}
	case *types.Named:
		return c.getLLVMType(typ.Underlying())
	case *types.Pointer:
		ptrTo, err := c.getLLVMType(typ.Elem())
		if err != nil {
			return llvm.Type{}, err
		}
		return llvm.PointerType(ptrTo, 0), nil
	case *types.Struct:
		members := make([]llvm.Type, typ.NumFields())
		for i := 0; i < typ.NumFields(); i++ {
			member, err := c.getLLVMType(typ.Field(i).Type())
			if err != nil {
				return llvm.Type{}, err
			}
			members[i] = member
		}
		return llvm.StructType(members, false), nil
	default:
		return llvm.Type{}, errors.New("todo: unknown type: " + fmt.Sprintf("%#v", goType))
	}
}

func (c *Compiler) getPackageRelativeName(frame *Frame, name string) string {
	if strings.IndexByte(name, '.') == -1 {
		name = frame.pkgPrefix + "." + name
	}
	return name
}

func (c *Compiler) parseFuncDecl(pkgPrefix string, f *ssa.Function) (*Frame, error) {
	name := pkgPrefix + "." + f.Name()
	frame := &Frame{
		pkgPrefix: pkgPrefix,
		name:      name,
		params:    make(map[*ssa.Parameter]int),
		locals:    make(map[ssa.Value]llvm.Value),
		blocks:    make(map[*ssa.BasicBlock]llvm.BasicBlock),
	}

	var retType llvm.Type
	if f.Signature.Results() == nil {
		retType = llvm.VoidType()
	} else if f.Signature.Results().Len() == 1 {
		var err error
		retType, err = c.getLLVMType(f.Signature.Results().At(0).Type())
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("todo: return values")
	}

	var paramTypes []llvm.Type
	for i, param := range f.Params {
		paramType, err := c.getLLVMType(param.Type())
		if err != nil {
			return nil, err
		}
		paramTypes = append(paramTypes, paramType)
		frame.params[param] = i
	}

	fnType := llvm.FunctionType(retType, paramTypes, false)
	llvm.AddFunction(c.mod, name, fnType)
	return frame, nil
}

func (c *Compiler) parseFunc(frame *Frame, f *ssa.Function) error {
	// Pre-create all basic blocks in the function.
	llvmFn := c.mod.NamedFunction(frame.name)
	for _, block := range f.DomPreorder() {
		llvmBlock := c.ctx.AddBasicBlock(llvmFn, block.Comment)
		frame.blocks[block] = llvmBlock
	}

	// Fill those blocks with instructions.
	for _, block := range f.DomPreorder() {
		c.builder.SetInsertPointAtEnd(frame.blocks[block])
		for _, instr := range block.Instrs {
			fmt.Printf("  instr: %v\n", instr)
			err := c.parseInstr(frame, instr)
			if err != nil {
				return err
			}
		}
	}

	// Resolve phi nodes
	for _, phi := range frame.phis {
		block := phi.ssa.Block()
		for i, edge := range phi.ssa.Edges {
			llvmVal, err := c.parseExpr(frame, edge)
			if err != nil {
				return err
			}
			llvmBlock := frame.blocks[block.Preds[i]]
			phi.llvm.AddIncoming([]llvm.Value{llvmVal}, []llvm.BasicBlock{llvmBlock})
		}
	}

	return nil
}

func (c *Compiler) parseInstr(frame *Frame, instr ssa.Instruction) error {
	switch instr := instr.(type) {
	case ssa.Value:
		value, err := c.parseExpr(frame, instr)
		frame.locals[instr] = value
		return err
	case *ssa.If:
		cond, err := c.parseExpr(frame, instr.Cond)
		if err != nil {
			return err
		}
		block := instr.Block()
		blockThen := frame.blocks[block.Succs[0]]
		blockElse := frame.blocks[block.Succs[1]]
		c.builder.CreateCondBr(cond, blockThen, blockElse)
		return nil
	case *ssa.Jump:
		blockJump := frame.blocks[instr.Block().Succs[0]]
		c.builder.CreateBr(blockJump)
		return nil
	case *ssa.Return:
		if len(instr.Results) == 0 {
			c.builder.CreateRetVoid()
			return nil
		} else if len(instr.Results) == 1 {
			val, err := c.parseExpr(frame, instr.Results[0])
			if err != nil {
				return err
			}
			c.builder.CreateRet(val)
			return nil
		} else {
			return errors.New("todo: return value")
		}
	case *ssa.Store:
		addr, err := c.parseExpr(frame, instr.Addr)
		if err != nil {
			return err
		}
		val, err := c.parseExpr(frame, instr.Val)
		if err != nil {
			return err
		}
		c.builder.CreateStore(val, addr)
		return nil
	default:
		return errors.New("unknown instruction: " + fmt.Sprintf("%#v", instr))
	}
}

func (c *Compiler) parseBuiltin(frame *Frame, instr *ssa.CallCommon, call *ssa.Builtin) (llvm.Value, error) {
	fmt.Printf("    builtin: %v\n", call)
	name := call.Name()

	switch name {
	case "print", "println":
		for i, arg := range instr.Args {
			if i >= 1 {
				c.builder.CreateCall(c.printspaceFunc, nil, "")
			}
			fmt.Printf("    arg: %s\n", arg);
			value, err := c.parseExpr(frame, arg)
			if err != nil {
				return llvm.Value{}, err
			}
			switch typ := arg.Type().(type) {
			case *types.Basic:
				switch typ.Kind() {
				case types.Int, types.Int32: // TODO: assumes a 32-bit int type
					c.builder.CreateCall(c.printintFunc, []llvm.Value{value}, "")
				case types.String:
					c.builder.CreateCall(c.printstringFunc, []llvm.Value{value}, "")
				default:
					return llvm.Value{}, errors.New("unknown basic arg type: " + fmt.Sprintf("%#v", typ))
				}
			default:
				return llvm.Value{}, errors.New("unknown arg type: " + fmt.Sprintf("%#v", typ))
			}
		}
		if name == "println" {
			c.builder.CreateCall(c.printnlFunc, nil, "")
		}
		return llvm.Value{}, nil // print() or println() returns void
	default:
		return llvm.Value{}, errors.New("todo: builtin: " + name)
	}
}

func (c *Compiler) parseFunctionCall(frame *Frame, call *ssa.CallCommon, fn *ssa.Function) (llvm.Value, error) {
	fmt.Printf("    function: %s\n", fn)

	name := c.getPackageRelativeName(frame, fn.Name())
	target := c.mod.NamedFunction(name)
	if target.IsNil() {
		return llvm.Value{}, errors.New("undefined function: " + name)
	}

	var params []llvm.Value
	for _, param := range call.Args {
		val, err := c.parseExpr(frame, param)
		if err != nil {
			return llvm.Value{}, err
		}
		params = append(params, val)
	}

	return c.builder.CreateCall(target, params, ""), nil
}

func (c *Compiler) parseCall(frame *Frame, instr *ssa.Call) (llvm.Value, error) {
	fmt.Printf("    call: %s\n", instr)

	switch call := instr.Common().Value.(type) {
	case *ssa.Builtin:
		return c.parseBuiltin(frame, instr.Common(), call)
	case *ssa.Function:
		return c.parseFunctionCall(frame, instr.Common(), call)
	default:
		return llvm.Value{}, errors.New("todo: unknown call type: " + fmt.Sprintf("%#v", call))
	}
}

func (c *Compiler) parseExpr(frame *Frame, expr ssa.Value) (llvm.Value, error) {
	fmt.Printf("      expr: %v\n", expr)

	if frame != nil {
		if value, ok := frame.locals[expr]; ok {
			// Value is a local variable that has already been computed.
			fmt.Println("        from local var")
			return value, nil
		}
	}

	switch expr := expr.(type) {
	case *ssa.Alloc:
		typ, err := c.getLLVMType(expr.Type().Underlying().(*types.Pointer).Elem())
		if err != nil {
			return llvm.Value{}, err
		}
		if expr.Heap {
			// TODO: escape analysis
			return c.builder.CreateMalloc(typ, expr.Comment), nil
		} else {
			return c.builder.CreateAlloca(typ, expr.Comment), nil
		}
	case *ssa.Const:
		return c.parseConst(expr)
	case *ssa.BinOp:
		return c.parseBinOp(frame, expr)
	case *ssa.Call:
		return c.parseCall(frame, expr)
	case *ssa.FieldAddr:
		val, err := c.parseExpr(frame, expr.X)
		if err != nil {
			return llvm.Value{}, err
		}
		indices := []llvm.Value{
			llvm.ConstInt(llvm.Int32Type(), 0, false),
			llvm.ConstInt(llvm.Int32Type(), uint64(expr.Field), false),
		}
		return c.builder.CreateGEP(val, indices, ""), nil
	case *ssa.Global:
		return c.mod.NamedGlobal(c.getPackageRelativeName(frame, expr.Name())), nil
	case *ssa.Parameter:
		llvmFn := c.mod.NamedFunction(frame.name)
		return llvmFn.Param(frame.params[expr]), nil
	case *ssa.Phi:
		t, err := c.getLLVMType(expr.Type())
		if err != nil {
			return llvm.Value{}, err
		}
		phi := c.builder.CreatePHI(t, "")
		frame.phis = append(frame.phis, Phi{expr, phi})
		return phi, nil
	case *ssa.UnOp:
		return c.parseUnOp(frame, expr)
	default:
		return llvm.Value{}, errors.New("todo: unknown expression: " + fmt.Sprintf("%#v", expr))
	}
}

func (c *Compiler) parseBinOp(frame *Frame, binop *ssa.BinOp) (llvm.Value, error) {
	x, err := c.parseExpr(frame, binop.X)
	if err != nil {
		return llvm.Value{}, err
	}
	y, err := c.parseExpr(frame, binop.Y)
	if err != nil {
		return llvm.Value{}, err
	}
	switch binop.Op {
	case token.ADD: // +
		return c.builder.CreateAdd(x, y, ""), nil
	case token.SUB: // -
		return c.builder.CreateSub(x, y, ""), nil
	case token.MUL: // *
		return c.builder.CreateMul(x, y, ""), nil
	case token.QUO: // /
		return c.builder.CreateSDiv(x, y, ""), nil // TODO: UDiv (unsigned)
	case token.REM: // %
		return c.builder.CreateSRem(x, y, ""), nil // TODO: URem (unsigned)
	case token.AND: // &
		return c.builder.CreateAnd(x, y, ""), nil
	case token.OR:  // |
		return c.builder.CreateOr(x, y, ""), nil
	case token.XOR: // ^
		return c.builder.CreateXor(x, y, ""), nil
	case token.SHL: // <<
		return c.builder.CreateShl(x, y, ""), nil
	case token.SHR: // >>
		return c.builder.CreateAShr(x, y, ""), nil // TODO: LShr (unsigned)
	case token.AND_NOT: // &^
		// Go specific. Calculate "and not" with x & (~y)
		inv := c.builder.CreateNot(y, "") // ~y
		return c.builder.CreateAnd(x, inv, ""), nil
	case token.EQL: // ==
		return c.builder.CreateICmp(llvm.IntEQ, x, y, ""), nil
	case token.NEQ: // !=
		return c.builder.CreateICmp(llvm.IntNE, x, y, ""), nil
	case token.LSS: // <
		return c.builder.CreateICmp(llvm.IntSLT, x, y, ""), nil // TODO: ULT
	case token.LEQ: // <=
		return c.builder.CreateICmp(llvm.IntSLE, x, y, ""), nil // TODO: ULE
	case token.GTR: // >
		return c.builder.CreateICmp(llvm.IntSGT, x, y, ""), nil // TODO: UGT
	case token.GEQ: // >=
		return c.builder.CreateICmp(llvm.IntSGE, x, y, ""), nil // TODO: UGE
	default:
		return llvm.Value{}, errors.New("unknown binop")
	}
}

func (c *Compiler) parseConst(expr *ssa.Const) (llvm.Value, error) {
	switch expr.Value.Kind() {
	case constant.String:
		str := constant.StringVal(expr.Value)
		strLen := llvm.ConstInt(c.stringLenType, uint64(len(str)), false)
		strPtr := c.builder.CreateGlobalStringPtr(str, ".str")
		strObj := llvm.ConstStruct([]llvm.Value{strLen, strPtr}, false)
		return strObj, nil
	case constant.Int:
		n, _ := constant.Int64Val(expr.Value) // TODO: do something with the 'exact' return value?
		return llvm.ConstInt(c.intType, uint64(n), true), nil
	default:
		return llvm.Value{}, errors.New("todo: unknown constant")
	}
}

func (c *Compiler) parseUnOp(frame *Frame, unop *ssa.UnOp) (llvm.Value, error) {
	x, err := c.parseExpr(frame, unop.X)
	if err != nil {
		return llvm.Value{}, err
	}
	switch unop.Op {
	case token.NOT: // !
		return c.builder.CreateNot(x, ""), nil
	case token.MUL: // *ptr, dereference pointer
		return c.builder.CreateLoad(x, ""), nil
	default:
		return llvm.Value{}, errors.New("todo: unknown unop")
	}
}

// IR returns the whole IR as a human-readable string.
func (c *Compiler) IR() string {
	return c.mod.String()
}

func (c *Compiler) Verify() error {
	return llvm.VerifyModule(c.mod, 0)
}

func (c *Compiler) Optimize(optLevel int) {
	builder := llvm.NewPassManagerBuilder()
	defer builder.Dispose()
	builder.SetOptLevel(optLevel)
	builder.UseInlinerWithThreshold(200) // TODO depend on opt level, and -Os

	funcPasses := llvm.NewFunctionPassManagerForModule(c.mod)
	defer funcPasses.Dispose()
	builder.PopulateFunc(funcPasses)

	modPasses := llvm.NewPassManager()
	defer modPasses.Dispose()
	builder.Populate(modPasses)

	modPasses.Run(c.mod)
}

func (c *Compiler) EmitObject(path string) error {
	buf, err := c.machine.EmitToMemoryBuffer(c.mod, llvm.ObjectFile)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	f.Write(buf.Bytes())
	f.Close()
	return nil
}

// Helper function for Compiler object.
func Compile(pkgName, outpath, target string, printIR bool) error {
	c, err := NewCompiler(pkgName, target)
	if err != nil {
		return err
	}

	parseErr := c.Parse(pkgName)
	if printIR {
		fmt.Println(c.IR())
	}
	if parseErr != nil {
		return parseErr
	}

	if err := c.Verify(); err != nil {
		return err
	}
	c.Optimize(2)
	if err := c.Verify(); err != nil {
		return err
	}

	err = c.EmitObject(outpath)
	if err != nil {
		return err
	}

	return nil
}


func main() {
	outpath := flag.String("o", "", "output filename")
	target := flag.String("target", llvm.DefaultTargetTriple(), "LLVM target")
	printIR := flag.Bool("printir", false, "print LLVM IR after optimizing")

	flag.Parse()

	if *outpath == "" || flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s [-printir] [-target=<target>] -o <output> <input>", os.Args[0])
		flag.PrintDefaults()
		return
	}

	err := Compile(flag.Args()[0], *outpath, *target, *printIR)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}