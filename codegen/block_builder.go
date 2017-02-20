package codegen

import (
	"fmt"
	"github.com/rhysd/gocaml/gcil"
	"github.com/rhysd/gocaml/typing"
	"llvm.org/llvm/bindings/go/llvm"
)

type blockBuilder struct {
	*moduleBuilder
	registers map[string]llvm.Value
}

func newBlockBuilder(b *moduleBuilder) *blockBuilder {
	return &blockBuilder{b, map[string]llvm.Value{}}
}

func (b *blockBuilder) resolve(ident string) llvm.Value {
	if glob, ok := b.globalTable[ident]; ok {
		return b.builder.CreateLoad(glob, ident)
	}
	if reg, ok := b.registers[ident]; ok {
		return reg
	}
	panic("No value was found for identifier: " + ident)
}

func (b *blockBuilder) typeOf(ident string) typing.Type {
	if t, ok := b.env.Table[ident]; ok {
		for {
			v, ok := t.(*typing.Var)
			if !ok {
				return t
			}
			if v.Ref == nil {
				panic("Empty type variable while searching variable: " + ident)
			}
			t = v.Ref
		}
	}
	if t, ok := b.env.Externals[ident]; ok {
		for {
			v, ok := t.(*typing.Var)
			if !ok {
				return t
			}
			if v.Ref == nil {
				panic("Empty type variable while searching external variable: " + ident)
			}
			t = v.Ref
		}
	}
	panic("Type was not found for ident: " + ident)
}

func (b *blockBuilder) buildIndexLoop(name string, until llvm.Value, pred func(index llvm.Value)) {
	idxPtr := b.builder.CreateAlloca(b.typeBuilder.intT, name+".index")
	b.builder.CreateStore(llvm.ConstInt(b.typeBuilder.intT, 0, false), idxPtr)

	parent := b.builder.GetInsertBlock().Parent()
	loopBlock := llvm.AddBasicBlock(parent, name+".loop")
	endBlock := llvm.AddBasicBlock(parent, name+".end")

	b.builder.CreateBr(loopBlock)
	b.builder.SetInsertPointAtEnd(loopBlock)

	idxVal := b.builder.CreateLoad(idxPtr, "")
	pred(idxVal)

	idxVal = b.builder.CreateAdd(idxVal, llvm.ConstInt(b.typeBuilder.intT, 1, false), name+".inc")
	b.builder.CreateStore(idxVal, idxPtr)
	compVal := b.builder.CreateICmp(llvm.IntEQ, idxVal, until, "")
	b.builder.CreateCondBr(compVal, endBlock, loopBlock)
	b.builder.SetInsertPointAtEnd(endBlock)
}

func (b *blockBuilder) buildEq(ty typing.Type, lhs, rhs llvm.Value) llvm.Value {
	switch ty := ty.(type) {
	case *typing.Unit:
		// `() = ()` is always true.
		return llvm.ConstInt(b.typeBuilder.boolT, 1, false /*sign extend*/)
	case *typing.Bool, *typing.Int:
		return b.builder.CreateICmp(llvm.IntEQ, lhs, rhs, "eql")
	case *typing.Float:
		return b.builder.CreateFCmp(llvm.FloatOEQ, lhs, rhs, "eql")
	case *typing.Tuple:
		cmp := llvm.Value{}
		for i, elemTy := range ty.Elems {
			l := b.builder.CreateLoad(b.builder.CreateStructGEP(lhs, i, "tpl.left"), "")
			r := b.builder.CreateLoad(b.builder.CreateStructGEP(rhs, i, "tpl.right"), "")
			elemCmp := b.buildEq(elemTy, l, r)
			if cmp.C == nil {
				cmp = elemCmp
			} else {
				cmp = b.builder.CreateAnd(cmp, elemCmp, "")
			}
		}
		cmp.SetName("eql.tpl")
		return cmp
	case *typing.Array:
		prevBlock := b.builder.GetInsertBlock()
		parent := prevBlock.Parent()
		elemsBlock := llvm.AddBasicBlock(parent, "cmp.arr.elems")
		endBlock := llvm.AddBasicBlock(parent, "cmp.arr.end")

		// Check size is equivalent
		lSize := b.builder.CreateLoad(b.builder.CreateStructGEP(lhs, 1, ""), "arr.left.size")
		rSize := b.builder.CreateLoad(b.builder.CreateStructGEP(rhs, 1, ""), "arr.right.size")

		cmpSize := b.builder.CreateICmp(llvm.IntNE, lSize, rSize, "")
		b.builder.CreateCondBr(cmpSize, endBlock, elemsBlock)

		// Check all elements are equivalent
		b.builder.SetInsertPointAtEnd(elemsBlock)
		lArr := b.builder.CreateLoad(b.builder.CreateStructGEP(lhs, 0, ""), "arr.left")
		rArr := b.builder.CreateLoad(b.builder.CreateStructGEP(lhs, 0, ""), "arr.right")
		cmp := cmpSize
		b.buildIndexLoop("cmp.arr.elems", lSize, func(idxVal llvm.Value) {
			l := b.builder.CreateLoad(b.builder.CreateInBoundsGEP(lArr, []llvm.Value{idxVal}, ""), "arr.elem.left")
			r := b.builder.CreateLoad(b.builder.CreateInBoundsGEP(rArr, []llvm.Value{idxVal}, ""), "arr.elem.right")
			elemCmp := b.buildEq(ty.Elem, l, r)
			cmp = b.builder.CreateAnd(cmp, elemCmp, "")
		})
		b.builder.CreateBr(endBlock)

		// Merge size check and elems check
		b.builder.SetInsertPointAtEnd(endBlock)
		phi := b.builder.CreatePHI(b.typeBuilder.boolT, "eql.arr")
		phi.AddIncoming([]llvm.Value{cmpSize, cmp}, []llvm.BasicBlock{prevBlock, elemsBlock})

		return phi
	default:
		panic("unreachable")
	}
}

func (b *blockBuilder) buildVal(ident string, val gcil.Val) llvm.Value {
	switch val := val.(type) {
	case *gcil.Unit:
		return llvm.ConstStruct([]llvm.Value{}, false /*packed*/)
	case *gcil.Bool:
		c := uint64(1)
		if !val.Const {
			c = 0
		}
		return llvm.ConstInt(b.typeBuilder.boolT, c, false /*sign extend*/)
	case *gcil.Int:
		return llvm.ConstInt(b.typeBuilder.intT, uint64(val.Const), true /*sign extend*/)
	case *gcil.Float:
		return llvm.ConstFloat(b.typeBuilder.floatT, val.Const)
	case *gcil.Unary:
		child := b.resolve(val.Child)
		switch val.Op {
		case gcil.NEG:
			return b.builder.CreateNeg(child, "neg")
		case gcil.FNEG:
			return b.builder.CreateFNeg(child, "fneg")
		case gcil.NOT:
			return b.builder.CreateNot(child, "not")
		default:
			panic("unreachable")
		}
	case *gcil.Binary:
		lhs := b.resolve(val.Lhs)
		rhs := b.resolve(val.Rhs)
		switch val.Op {
		case gcil.ADD:
			return b.builder.CreateAdd(lhs, rhs, "add")
		case gcil.SUB:
			return b.builder.CreateSub(lhs, rhs, "sub")
		case gcil.FADD:
			return b.builder.CreateFAdd(lhs, rhs, "fadd")
		case gcil.FSUB:
			return b.builder.CreateFSub(lhs, rhs, "fsub")
		case gcil.FMUL:
			return b.builder.CreateFMul(lhs, rhs, "fmul")
		case gcil.FDIV:
			return b.builder.CreateFDiv(lhs, rhs, "fdiv")
		case gcil.LESS:
			lty := b.typeOf(val.Lhs)
			switch lty.(type) {
			case *typing.Int:
				return b.builder.CreateICmp(llvm.IntSLT /*Signed Less Than*/, lhs, rhs, "less")
			case *typing.Float:
				return b.builder.CreateFCmp(llvm.FloatOLT /*Ordered and Less Than*/, lhs, rhs, "less")
			default:
				panic("Invalid type for '<' operator: " + lty.String())
			}
		case gcil.EQ:
			return b.buildEq(b.typeOf(val.Lhs), lhs, rhs)
		default:
			panic("unreachable")
		}
	case *gcil.Ref:
		reg, ok := b.registers[val.Ident]
		if !ok {
			panic("Value not found for ref: " + val.Ident)
		}
		return reg
	case *gcil.If:
		parent := b.builder.GetInsertBlock().Parent()
		thenBlock := llvm.AddBasicBlock(parent, "if.then")
		elseBlock := llvm.AddBasicBlock(parent, "if.else")
		endBlock := llvm.AddBasicBlock(parent, "if.end")

		ty := b.typeBuilder.convertGCIL(b.typeOf(ident))
		cond := b.resolve(val.Cond)
		b.builder.CreateCondBr(cond, thenBlock, elseBlock)

		b.builder.SetInsertPointAtEnd(thenBlock)
		thenVal := b.build(val.Then)
		b.builder.CreateBr(endBlock)

		b.builder.SetInsertPointAtEnd(elseBlock)
		elseVal := b.build(val.Else)
		b.builder.CreateBr(endBlock)

		b.builder.SetInsertPointAtEnd(endBlock)
		phi := b.builder.CreatePHI(ty, "if.merge")
		phi.AddIncoming([]llvm.Value{thenVal, elseVal}, []llvm.BasicBlock{thenBlock, elseBlock})
		return phi
	case *gcil.Fun:
		panic("unreachable because IR was closure-transformed")
	case *gcil.App:
		argsLen := len(val.Args)
		if val.Kind == gcil.CLOSURE_CALL {
			argsLen++
		}
		argVals := make([]llvm.Value, 0, argsLen)

		if val.Kind == gcil.CLOSURE_CALL {
			// Add pointer to closure captures
			argVals = append(argVals, b.resolve(val.Callee))
		}
		for _, a := range val.Args {
			argVals = append(argVals, b.resolve(a))
		}

		table := b.funcTable
		if val.Kind == gcil.EXTERNAL_CALL {
			table = b.globalTable
		}
		funVal, ok := table[val.Callee]
		if !ok {
			if val.Kind != gcil.CLOSURE_CALL {
				panic("Value for function is not found in table: " + val.Callee)
			}
			// If callee is a function variable and not well-known, we need to fetch the function pointer
			// to call from closure value.
			ptr := b.builder.CreateStructGEP(argVals[0], 0, "")
			funVal = b.builder.CreateLoad(ptr, "funptr")
		}

		// Note:
		// Call inst cannot have a name when the return type is void.
		return b.builder.CreateCall(funVal, argVals, "")
	case *gcil.Tuple:
		// Note:
		// Type of tuple is a pointer to struct. To obtain the value for tuple, we need underlying
		// struct type because 'alloca' instruction returns the pointer to allocated memory.
		ptrTy := b.typeBuilder.convertGCIL(b.typeOf(ident))
		allocTy := ptrTy.ElementType()

		ptr := b.builder.CreateAlloca(allocTy, ident)
		for i, e := range val.Elems {
			v := b.resolve(e)
			p := b.builder.CreateStructGEP(ptr, i, fmt.Sprintf("%s.%d", ident, i))
			b.builder.CreateStore(v, p)
		}
		return ptr
	case *gcil.Array:
		t, ok := b.typeOf(ident).(*typing.Array)
		if !ok {
			panic("Type of array literal is not array")
		}

		ty := b.typeBuilder.convertGCIL(t)
		elemTy := b.typeBuilder.convertGCIL(t.Elem)
		ptr := b.builder.CreateAlloca(ty, ident)

		sizeVal := b.resolve(val.Size)

		// XXX:
		// Arrays are allocated on stack. So returning array value from function
		// now breaks the array value.
		arrVal := b.builder.CreateArrayAlloca(elemTy, sizeVal, "array.ptr")
		b.builder.CreateStore(arrVal, b.builder.CreateStructGEP(ptr, 0, ""))

		// Copy second argument to all elements of allocated array
		elemVal := b.resolve(val.Elem)
		b.buildIndexLoop("arr.init", sizeVal, func(idxVal llvm.Value) {
			elemPtr := b.builder.CreateInBoundsGEP(arrVal, []llvm.Value{idxVal}, "")
			b.builder.CreateStore(elemVal, elemPtr)
		})

		// Set size value
		sizePtr := b.builder.CreateStructGEP(ptr, 1, "")
		b.builder.CreateStore(sizeVal, sizePtr)

		return ptr
	case *gcil.TplLoad:
		from := b.resolve(val.From)
		p := b.builder.CreateStructGEP(from, val.Index, "")
		return b.builder.CreateLoad(p, "tplload")
	case *gcil.ArrLoad:
		fromVal := b.resolve(val.From)
		idxVal := b.resolve(val.Index)
		arrPtr := b.builder.CreateLoad(b.builder.CreateStructGEP(fromVal, 0, ""), "")
		elemPtr := b.builder.CreateInBoundsGEP(arrPtr, []llvm.Value{idxVal}, "")
		return b.builder.CreateLoad(elemPtr, "arrload")
	case *gcil.ArrStore:
		toVal := b.resolve(val.To)
		idxVal := b.resolve(val.Index)
		rhsVal := b.resolve(val.Rhs)
		arrPtr := b.builder.CreateStructGEP(toVal, 0, "")
		elemPtr := b.builder.CreateInBoundsGEP(arrPtr, []llvm.Value{idxVal}, "")
		return b.builder.CreateStore(rhsVal, elemPtr)
	case *gcil.XRef:
		x, ok := b.globalTable[val.Ident]
		if !ok {
			panic("Value for external value not found: " + val.Ident)
		}
		return b.builder.CreateLoad(x, val.Ident)
	case *gcil.MakeCls:
		closure, ok := b.closures[val.Fun]
		if !ok {
			panic("Closure for function not found: " + val.Fun)
		}
		closureTy := b.typeBuilder.buildCapturesStruct(val.Fun, closure)
		alloca := b.builder.CreateAlloca(closureTy, "")

		// Set function pointer to first field of closure
		funVal, ok := b.funcTable[val.Fun]
		if !ok {
			panic("Value for function not found: " + val.Fun)
		}
		b.builder.CreateStore(funVal, b.builder.CreateStructGEP(alloca, 0, ""))

		// Set captures to rest of struct
		for i, v := range val.Vars {
			ptr := b.builder.CreateStructGEP(alloca, i+1, "")
			freevar := b.resolve(v)
			b.builder.CreateStore(freevar, ptr)
		}

		ptr := b.builder.CreateBitCast(alloca, b.typeBuilder.voidPtrT, fmt.Sprintf("closure.%s", val.Fun))
		return ptr
	case *gcil.NOP:
		panic("unreachable")
	default:
		panic("unreachable")
	}
}

func (b *blockBuilder) buildInsn(insn *gcil.Insn) llvm.Value {
	v := b.buildVal(insn.Ident, insn.Val)
	b.registers[insn.Ident] = v
	return v
}

func (b *blockBuilder) build(block *gcil.Block) llvm.Value {
	i := block.Top.Next
	for {
		v := b.buildInsn(i)
		i = i.Next
		if i.Next == nil {
			return v
		}
	}
}