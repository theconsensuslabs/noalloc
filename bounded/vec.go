package bounded

import (
	"iter"
	"math/bits"
)

// vecChunks is the most chunks a Vec spans.
const vecChunks = 32

// A Vec is a list of fixed capacity: one slice it is given, or chunks it
// borrows from a Region as it fills, each a power of two long so finding an
// item is a shift and a mask. A Push that does not fit adds nothing and
// leaves the Vec full until it is truncated or reset.
//
// A Vec in chunks is not contiguous: read it with At, or a chunk at a time
// with Chunks, whose slices a loop indexes as fast as any. A Slice is
// contiguous, and moves to a larger chunk as it grows.
//
//noalloc:fixed
type Vec[T any] struct {
	chunks [vecChunks][]T
	// k is how many chunks the Vec holds, and at the one Push fills.
	k, at int
	n     int
	full  bool
	// r and p are where a Vec reset from a Region borrows its chunks, and gen
	// r's releases when it was reset.
	r   *Region
	p   *Pool[T]
	gen uint64
	// shift and mask find item i of a Vec in chunks: chunk i>>shift, place
	// i&mask.
	shift uint8
	mask  int
}

// Reset makes mem the Vec's memory, emptied. cap(mem) is the budget.
//
//noalloc:root
func (v *Vec[T]) Reset(mem []T) {
	//noalloc:exempt setup: the memory the Vec is given, which is its own from here on.
	*v = Vec[T]{k: 1}
	v.chunks[0] = mem[:0]
}

// ResetFrom empties the Vec, which borrows each chunk it fills from p for r.
// What it holds is valid until r is released. p's chunks must be a power of
// two long.
//
//noalloc:root
func (v *Vec[T]) ResetFrom(r *Region, p *Pool[T]) {
	l := p.ChunkLen()
	if l&(l-1) != 0 {
		panic("bounded: a Vec borrows chunks a power of two long")
	}
	*v = Vec[T]{r: r, p: p, gen: r.gen, shift: uint8(bits.TrailingZeros(uint(l))), mask: l - 1}
}

// live panics when the Region a Vec borrows from was released since its reset.
func (v *Vec[T]) live() {
	if v.r != nil && v.r.gen != v.gen {
		panic("bounded: a Vec used after its region was released")
	}
}

// Push adds x, and reports whether it fit.
//
//noalloc:root
func (v *Vec[T]) Push(x T) bool {
	v.live()
	for v.at < v.k {
		if c := v.chunks[v.at]; len(c) < cap(c) {
			c = c[:len(c)+1]
			c[len(c)-1] = x
			v.chunks[v.at] = c
			v.n++
			return true
		}
		if v.at+1 == v.k {
			break
		}
		v.at++
	}
	if v.r != nil && v.k < vecChunks {
		if c := v.r.Take(v.p); c != nil {
			c = c[:1]
			c[0] = x
			v.chunks[v.k] = c
			v.at = v.k
			v.k++
			v.n++
			return true
		}
	}
	v.full = true
	return false
}

// At is item i, which Len counts.
//
//noalloc:root
func (v *Vec[T]) At(i int) T {
	v.live()
	if i < 0 || i >= v.n {
		panic("bounded: index out of range")
	}
	if v.r == nil {
		return v.chunks[0][i]
	}
	return v.chunks[i>>v.shift][i&v.mask]
}

// Chunks yields what the Vec holds a chunk at a time, in order. The Vec must
// not change while it does.
//
//noalloc:root
func (v *Vec[T]) Chunks() iter.Seq[[]T] {
	v.live()
	return func(yield func([]T) bool) {
		for _, c := range v.chunks[:v.k] {
			if len(c) > 0 && !yield(c) {
				return
			}
		}
	}
}

// All yields each item with its index, in order. The Vec must not change
// while it does.
//
//noalloc:root
func (v *Vec[T]) All() iter.Seq2[int, T] {
	v.live()
	return func(yield func(int, T) bool) {
		i := 0
		for _, c := range v.chunks[:v.k] {
			for _, x := range c {
				if !yield(i, x) {
					return
				}
				i++
			}
		}
	}
}

// Extend adds every item of xs, copying a chunk at a time, and reports
// whether they all fit. When they do not it adds none, and the Vec is full.
//
//noalloc:root
func (v *Vec[T]) Extend(xs []T) bool {
	v.live()
	before := v.n
	for len(xs) > 0 {
		if v.at < v.k {
			c := v.chunks[v.at]
			if k := copy(c[len(c):cap(c)], xs); k > 0 {
				v.chunks[v.at] = c[:len(c)+k]
				v.n += k
				xs = xs[k:]
				continue
			}
			if v.at+1 < v.k {
				v.at++
				continue
			}
		}
		if v.r == nil || v.k == vecChunks {
			break
		}
		c := v.r.Take(v.p)
		if c == nil {
			break
		}
		v.chunks[v.k] = c[:0]
		v.at = v.k
		v.k++
	}
	if len(xs) > 0 {
		v.Truncate(before)
		v.full = true
		return false
	}
	return true
}

// Items is what the Vec holds, valid until the next Push, Truncate, or Reset.
// It panics once the Vec spans more than one chunk; At reads any Vec.
//
//noalloc:root
func (v *Vec[T]) Items() []T {
	v.live()
	if v.k > 1 && len(v.chunks[1]) > 0 {
		panic("bounded: Items of a Vec in more than one chunk")
	}
	return v.chunks[0]
}

// Len is how many items the Vec holds.
//
//noalloc:root
func (v *Vec[T]) Len() int { return v.n }

// Cap is how many items the Vec holds room for now. A Vec reset from a
// Region gains room as it borrows chunks.
//
//noalloc:root
func (v *Vec[T]) Cap() int {
	n := 0
	for _, c := range v.chunks[:v.k] {
		n += cap(c)
	}
	return n
}

// Full reports whether a Push did not fit since the Vec was last reset,
// truncated, or cleared.
//
//noalloc:root
func (v *Vec[T]) Full() bool { return v.full }

// Truncate drops the items past the first n, which Len returned before, and
// makes room to push again. A Vec reset from a Region keeps its chunks.
//
//noalloc:root
func (v *Vec[T]) Truncate(n int) {
	v.live()
	if n < 0 || n > v.n {
		panic("bounded: truncation out of range")
	}
	v.n, v.full, v.at = n, false, 0
	if v.k == 0 {
		return
	}
	l := cap(v.chunks[0])
	for i := range v.chunks[:v.k] {
		c := v.chunks[i]
		keep := min(max(n-i*l, 0), len(c))
		clear(c[keep:])
		v.chunks[i] = c[:keep]
		if keep > 0 {
			v.at = i
		}
	}
	if len(v.chunks[v.at]) == cap(v.chunks[v.at]) && v.at+1 < v.k {
		v.at++
	}
}

// Clear empties the Vec for the next user of its memory.
//
//noalloc:root
func (v *Vec[T]) Clear() { v.Truncate(0) }
