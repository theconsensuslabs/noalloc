package noalloc

import (
	"fmt"
	"go/constant"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// An append to reused memory must be shown not to grow it: a comparison the
// append's block runs only after, on the branch where it holds, says the
// slice has room for what is appended. The comparison and the room the
// append needs are read as sums of len, cap, constants, and other values, so
// len(s)+n <= cap(s), n <= cap(s)-len(s), len(s) < cap(s) for one element,
// and the negation of each on the other branch all prove it.

// linear is a sum of terms and a constant.
type linear struct {
	k     int64
	terms map[string]int64
}

func (l linear) plus(m linear, sign int64) linear {
	out := linear{k: l.k + sign*m.k, terms: map[string]int64{}}
	for t, n := range l.terms {
		out.terms[t] += n
	}
	for t, n := range m.terms {
		if out.terms[t] += sign * n; out.terms[t] == 0 {
			delete(out.terms, t)
		}
	}
	return out
}

func constLinear(k int64) linear { return linear{k: k, terms: map[string]int64{}} }

func termLinear(t string) linear { return linear{terms: map[string]int64{t: 1}} }

// valueKey names v so that two loads of the same field, reached the same way,
// name the same.
func valueKey(v ssa.Value) string {
	switch v := v.(type) {
	case *ssa.UnOp:
		if v.Op == token.MUL {
			return "*" + valueKey(v.X)
		}
	case *ssa.FieldAddr:
		return valueKey(v.X) + "." + fmt.Sprint(v.Field)
	case *ssa.Global:
		return v.RelString(nil)
	}
	return fmt.Sprintf("%p", v)
}

// linearOf reads an integer value as a sum.
func linearOf(v ssa.Value) linear {
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.Int {
			if k, ok := constant.Int64Val(v.Value); ok {
				return constLinear(k)
			}
		}
	case *ssa.BinOp:
		switch v.Op {
		case token.ADD:
			return linearOf(v.X).plus(linearOf(v.Y), 1)
		case token.SUB:
			return linearOf(v.X).plus(linearOf(v.Y), -1)
		}
	case *ssa.Convert:
		if isInteger(v.Type()) && isInteger(v.X.Type()) {
			return linearOf(v.X)
		}
	case *ssa.Call:
		if b, ok := v.Call.Value.(*ssa.Builtin); ok && len(v.Call.Args) == 1 {
			switch b.Name() {
			case "len":
				return lenOf(v.Call.Args[0])
			case "cap":
				return capOf(v.Call.Args[0])
			}
		}
	}
	return termLinear("v:" + valueKey(v))
}

func isInteger(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsInteger != 0
}

// arrayLen is the length of the array p points to, if it does.
func arrayLen(p ssa.Value) (int64, bool) {
	if ptr, ok := p.Type().Underlying().(*types.Pointer); ok {
		if a, ok := ptr.Elem().Underlying().(*types.Array); ok {
			return a.Len(), true
		}
	}
	return 0, false
}

func lenOf(v ssa.Value) linear {
	switch v := v.(type) {
	case *ssa.Slice:
		var high linear
		if v.High != nil {
			high = linearOf(v.High)
		} else {
			high = lenOf(v.X)
		}
		if v.Low != nil {
			return high.plus(linearOf(v.Low), -1)
		}
		return high
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.String {
			return constLinear(int64(len(constant.StringVal(v.Value))))
		}
	}
	if n, ok := arrayLen(v); ok {
		return constLinear(n)
	}
	return termLinear("len:" + valueKey(v))
}

func capOf(v ssa.Value) linear {
	if s, ok := v.(*ssa.Slice); ok {
		var max linear
		if s.Max != nil {
			max = linearOf(s.Max)
		} else {
			max = capOf(s.X)
		}
		if s.Low != nil {
			return max.plus(linearOf(s.Low), -1)
		}
		return max
	}
	if n, ok := arrayLen(v); ok {
		return constLinear(n)
	}
	return termLinear("cap:" + valueKey(v))
}

// holds is what an If's condition says on the branch to succ, as a sum that
// is at least zero there.
func holds(cond ssa.Value, succ int) (linear, bool) {
	if u, ok := cond.(*ssa.UnOp); ok && u.Op == token.NOT {
		return holds(u.X, 1-succ)
	}
	bin, ok := cond.(*ssa.BinOp)
	if !ok || !isInteger(bin.X.Type()) {
		return linear{}, false
	}
	l, r := linearOf(bin.X), linearOf(bin.Y)
	op := bin.Op
	if succ == 1 {
		switch op {
		case token.LEQ:
			op = token.GTR
		case token.LSS:
			op = token.GEQ
		case token.GEQ:
			op = token.LSS
		case token.GTR:
			op = token.LEQ
		default:
			return linear{}, false
		}
	}
	switch op {
	case token.LEQ:
		return r.plus(l, -1), true
	case token.LSS:
		return r.plus(l, -1).plus(constLinear(1), -1), true
	case token.GEQ:
		return l.plus(r, -1), true
	case token.GTR:
		return l.plus(r, -1).plus(constLinear(1), -1), true
	}
	return linear{}, false
}

// fits reports whether appending to s grows it by no more than its capacity
// allows: the room the append needs is a constant no less than zero, or some
// comparison the call's block runs only after, on the branch it holds on, is
// at least that room, with nothing stored to s or called between them.
func fits(call *ssa.Call) bool {
	args := call.Call.Args
	if len(args) != 2 {
		return false
	}
	s := args[0]
	need := capOf(s).plus(lenOf(s), -1).plus(lenOf(args[1]), -1)
	// A slice of an array, or of a known length, needs no comparison.
	if len(need.terms) == 0 {
		return need.k >= 0
	}
	b := call.Block()
	for at := b; at != nil; at = at.Idom() {
		for _, pred := range at.Preds {
			guard, ok := pred.Instrs[len(pred.Instrs)-1].(*ssa.If)
			if !ok || pred != at.Idom() {
				continue
			}
			succ := 0
			if pred.Succs[1] == at {
				succ = 1
			}
			have, ok := holds(guard.Cond, succ)
			if !ok {
				continue
			}
			if d := have.plus(need, -1); len(d.terms) == 0 && d.k <= 0 && untouched(s, at, b, call) {
				return true
			}
		}
		if len(at.Preds) != 1 {
			return false
		}
	}
	return false
}

// untouched reports whether nothing from the start of block from to call, a
// path from has to take, stores to the memory s is read from or calls a
// function, which could.
func untouched(s ssa.Value, from, to *ssa.BasicBlock, call ssa.Instruction) bool {
	root := s
	for {
		if sl, ok := root.(*ssa.Slice); ok {
			root = sl.X
			continue
		}
		break
	}
	var addr ssa.Value
	if u, ok := root.(*ssa.UnOp); ok && u.Op == token.MUL {
		addr = u.X
	}
	var chain []*ssa.BasicBlock
	for at := to; ; at = at.Idom() {
		chain = append(chain, at)
		if at == from {
			break
		}
	}
	for _, blk := range chain {
		for _, in := range blk.Instrs {
			if in == call {
				break
			}
			switch in := in.(type) {
			case *ssa.Store:
				if addr != nil && sameAddr(in.Addr, addr) {
					return false
				}
			case ssa.CallInstruction:
				if b, ok := in.Common().Value.(*ssa.Builtin); ok && (b.Name() == "len" || b.Name() == "cap") {
					continue
				}
				return false
			}
		}
	}
	return true
}

// ownAppend reports whether the append call grows the first parameter of
// the function marked //noalloc:append it is in, which its caller answers
// for.
func (c *checker) ownAppend(call *ssa.Call) bool {
	return c.ownAppendCall(call, call.Call.Args[0])
}

// ownAppendCall reports whether call, in a function marked //noalloc:append,
// appends to dst, that function's first parameter or what appending to it
// returned.
func (c *checker) ownAppendCall(call ssa.CallInstruction, dst ssa.Value) bool {
	fn := call.Parent()
	if !c.appendRoot(fn) {
		return false
	}
	return c.fromFirstParam(fn, dst, map[ssa.Value]bool{})
}

func (c *checker) fromFirstParam(fn *ssa.Function, v ssa.Value, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return true
	}
	seen[v] = true
	switch v := v.(type) {
	case *ssa.Parameter:
		return v == fn.Params[len(fn.Params)-fn.Signature.Params().Len()]
	case *ssa.Slice:
		return c.fromFirstParam(fn, v.X, seen)
	case *ssa.Phi:
		for _, e := range v.Edges {
			if !c.fromFirstParam(fn, e, seen) {
				return false
			}
		}
		return true
	case *ssa.Call:
		cm := v.Common()
		if b, ok := cm.Value.(*ssa.Builtin); ok {
			return b.Name() == "append" && len(cm.Args) > 0 && c.fromFirstParam(fn, cm.Args[0], seen)
		}
		if callee := cm.StaticCallee(); callee != nil && (c.appendRoot(callee) || c.allow.appends[funcName(callee)]) {
			if dst := appendedTo(cm, callee); dst != nil {
				return c.fromFirstParam(fn, dst, seen)
			}
		}
	case *ssa.Extract:
		if v.Index == 0 {
			return c.fromFirstParam(fn, v.Tuple, seen)
		}
	}
	return false
}
