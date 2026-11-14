package bounded

import (
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// A Pool is a fixed number of chunks of T, set aside once, which Regions
// borrow and give back. Chunks of a type holding pointers stay that type,
// so the garbage collector sees what they point to.
//
//noalloc:fixed
type Pool[T any] struct {
	chunks [][]T
	free   freeList
	bytes  int
	// base is where the chunks start, one after another.
	base uintptr
	// pointers says T holds pointers, so a chunk given back is zeroed and
	// keeps nothing alive.
	pointers bool
}

// NewPool sets aside count chunks of chunkLen T each.
func NewPool[T any](chunkLen, count int) *Pool[T] {
	var zero T
	if chunkLen < 1 || count < 1 || count > 1<<31-1 || unsafe.Sizeof(zero) == 0 {
		panic("bounded: a pool needs at least one chunk of at least one item that takes memory")
	}
	//noalloc:exempt setup: a pool's chunks, set aside once.
	mem := make([]T, chunkLen*count)
	//noalloc:exempt setup: a pool's list of its chunks, set aside once.
	p := &Pool[T]{
		chunks: make([][]T, count), bytes: chunkLen * int(unsafe.Sizeof(zero)),
		pointers: hasPointers(reflect.TypeFor[T]()),
	}
	for i := range count {
		p.chunks[i] = mem[i*chunkLen : (i+1)*chunkLen : (i+1)*chunkLen]
	}
	p.base = uintptr(unsafe.Pointer(&mem[0]))
	p.free.init(count)
	return p
}

// ChunkLen is how many items each chunk holds.
//
//noalloc:root
func (p *Pool[T]) ChunkLen() int { return len(p.chunks[0]) }

// ChunkBytes is the memory each chunk takes.
//
//noalloc:root
func (p *Pool[T]) ChunkBytes() int { return p.bytes }

// Chunks is how many chunks the pool holds.
//
//noalloc:root
func (p *Pool[T]) Chunks() int { return len(p.chunks) }

// Free is how many chunks no Region holds.
//
//noalloc:root
func (p *Pool[T]) Free() int { return p.free.free() }

// Stats is what the pool has lent since it was made.
//
//noalloc:root
func (p *Pool[T]) Stats() PoolStats { return p.free.stats() }

// giveBack makes chunk i free, zeroing it first when T holds pointers, so
// nothing a Region wrote stays reachable through it.
func (p *Pool[T]) giveBack(i int32, home int) {
	if p.pointers {
		clear(p.chunks[i])
	}
	p.free.push(i, home)
}

// hasPointers reports whether a value of t holds a pointer the garbage
// collector follows.
func hasPointers(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.UnsafePointer, reflect.Map, reflect.Slice, reflect.String, reflect.Interface,
		reflect.Chan, reflect.Func:
		return true
	case reflect.Array:
		return t.Len() > 0 && hasPointers(t.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			if hasPointers(t.Field(i).Type) {
				return true
			}
		}
	}
	return false
}

func (p *Pool[T]) chunkBytes() int { return p.bytes }

// lender is a Pool of any type, as a Region holds it.
type lender interface {
	giveBack(i int32, home int)
	chunkBytes() int
}

// regionLoans is the most chunks one Region holds at once.
const regionLoans = 64

// A Region is the chunks one page borrows from Pools, at most its budget in
// bytes across all of them, given back together by Release. A Region is
// used by one goroutine at a time; its Pools are shared.
//
//noalloc:fixed
type Region struct {
	loans  [regionLoans]loan
	n      int
	budget int
	held   int
	// gen counts releases, so a Vec can tell its chunks were given back.
	gen uint64
	// shard is the free list shard r borrows from first, plus one, set from
	// homes on r's first Reset.
	shard uint32
	// spares counts the chunks Reserve borrowed that no Take has handed out.
	spares int
}

// homes hands each Region its home shard in turn, so the first as many
// Regions as there are processors each have a shard of their own.
var homes atomic.Uint32

type loan struct {
	from lender
	i    int32
	// spare marks a chunk Reserve borrowed that no Take has handed out.
	spare bool
}

// Reset gives back what r holds and lets it borrow up to budget bytes.
//
//noalloc:root
func (r *Region) Reset(budget int) {
	r.Release()
	r.budget = budget
	if r.shard == 0 {
		r.shard = homes.Add(1)
	}
}

// Release gives back every chunk r holds. Memory taken from them must not
// be used after.
//
//noalloc:root
func (r *Region) Release() {
	for i := range r.loans[:r.n] {
		l := &r.loans[i]
		l.from.giveBack(l.i, r.home())
		*l = loan{}
	}
	r.n, r.held, r.spares = 0, 0, 0
	r.gen++
}

// Held is the memory r holds.
//
//noalloc:root
func (r *Region) Held() int { return r.held }

// home is the shard of a Pool r tries first: a Region kept in a slot's
// memory borrows from the same shard page after page, and gives its chunks
// back there.
func (r *Region) home() int { return int(r.shard) }

// fits reports whether r may borrow a chunk of p.
func (r *Region) fits(p lender) bool {
	return r.n < regionLoans && r.held+p.chunkBytes() <= r.budget
}

// keep records chunk i of p as r's.
func (r *Region) keep(p lender, i int32, bytes int, spare bool) {
	r.loans[r.n] = loan{from: p, i: i, spare: spare}
	r.n++
	r.held += bytes
	if spare {
		r.spares++
	}
}

// spare hands out a chunk of p that Reserve borrowed for r, if one is left.
func (r *Region) spare(p lender) (int32, bool) {
	if r.spares == 0 {
		return 0, false
	}
	for k := range r.loans[:r.n] {
		if l := &r.loans[k]; l.spare && l.from == p {
			l.spare = false
			r.spares--
			return l.i, true
		}
	}
	return 0, false
}

// GiveBack gives back to p a chunk Take or Wait borrowed for r, before r is
// released: what a container does once it moves to a larger chunk. The chunk
// must not be used after.
//
//noalloc:root
func (r *Region) GiveBack[T any](p *Pool[T], chunk []T) {
	i := p.index(chunk)
	for k := range r.loans[:r.n] {
		if l := &r.loans[k]; l.from == lender(p) && l.i == i && !l.spare {
			r.n--
			*l = r.loans[r.n]
			r.loans[r.n] = loan{}
			r.held -= p.bytes
			p.giveBack(i, r.home())
			return
		}
	}
	panic("bounded: a chunk given back that its region does not hold")
}

// index is which of p's chunks chunk is.
func (p *Pool[T]) index(chunk []T) int32 {
	at := uintptr(unsafe.Pointer(unsafe.SliceData(chunk)))
	off := at - p.base
	i := off / uintptr(p.bytes)
	if cap(chunk) == 0 || at < p.base || i >= uintptr(len(p.chunks)) || off%uintptr(p.bytes) != 0 {
		panic("bounded: a chunk given back that is not this pool's")
	}
	return int32(i)
}

// Reserve borrows n chunks of p for r now, waiting for each until done
// closes, and hands them to r's next Takes of p before any chunk past them.
// It borrows all n or none: when done closes first, or r's budget has no
// room for all n, it gives back what it borrowed and reports false.
//
// A Region waits for its last chunks while holding its first, so Regions
// reserving at once can wait on each other forever unless p holds every
// reservation they make together. Reserve under the admission that bounds
// how many pages run at once, and size p to hold each one's reservation.
//
//noalloc:root
func (r *Region) Reserve[T any](done <-chan struct{}, p *Pool[T], n int) bool {
	if r.n+n > regionLoans || r.held+n*p.bytes > r.budget {
		p.free.overBudget.Add(1)
		return false
	}
	first := r.n
	for range n {
		i, ok := p.free.wait(done, r.home())
		if !ok {
			for k := first; k < r.n; k++ {
				l := &r.loans[k]
				l.from.giveBack(l.i, r.home())
				r.held -= p.bytes
				*l = loan{}
			}
			r.n, r.spares = first, r.spares-(r.n-first)
			return false
		}
		r.keep(p, i, p.bytes, true)
	}
	return true
}

// Take borrows a chunk of p for r, or nil when r's budget has no room for it
// or p has none free. A chunk of a type holding pointers comes zeroed; any
// other holds what its last borrower left.
//
//noalloc:root
func (r *Region) Take[T any](p *Pool[T]) []T {
	if i, ok := r.spare(p); ok {
		return p.chunks[i]
	}
	if !r.fits(p) {
		p.free.overBudget.Add(1)
		return nil
	}
	i, ok := p.free.pop(r.home())
	if !ok {
		return nil
	}
	r.keep(p, i, p.bytes, false)
	return p.chunks[i]
}

// Wait borrows a chunk of p for r as Take does, waiting for one to come free
// until done closes. It reports false at once when r's budget has no room,
// and when done closes first.
//
//noalloc:root
func (r *Region) Wait[T any](done <-chan struct{}, p *Pool[T]) ([]T, bool) {
	if i, ok := r.spare(p); ok {
		return p.chunks[i], true
	}
	if !r.fits(p) {
		p.free.overBudget.Add(1)
		return nil, false
	}
	i, ok := p.free.wait(done, r.home())
	if !ok {
		return nil, false
	}
	r.keep(p, i, p.bytes, false)
	return p.chunks[i], true
}

// PoolStats counts what a Pool has lent since it was made.
type PoolStats struct {
	// Lent is how many chunks were borrowed.
	Lent int64
	// Empty is how many borrows found no chunk free: a Take refused, or a
	// Wait that waited. Waited is the time Waits spent waiting, and
	// OverBudget how many borrows a Region's budget refused.
	Empty      int64
	Waited     time.Duration
	OverBudget int64
}

// freeList is the indices of a pool's free chunks, in shards, one for each
// processor, so borrowers running at once seldom touch the same one. A
// borrower starts at its Region's home shard and looks at the others only
// when it is empty. While a Wait is waiting, each chunk given back sends a
// free chunk to it through handoff.
type freeList struct {
	shards  []shard
	waiting atomic.Int64
	handoff chan int32
	// The counters PoolStats reports past each shard's lent, each written
	// only when a borrow finds no chunk free or no room in its budget.
	empty, waited, overBudget atomic.Int64
}

// shard is one part of a freeList: a slot for one free chunk that a borrow
// takes and a give-back fills without the lock, a stack of indices behind the
// lock, how many it lent, and how many times a borrow or a give-back locked
// it, padded so two shards never share a cache line, 128 bytes on the
// largest.
type shard struct {
	mu     sync.Mutex
	idx    []int32
	lent   atomic.Int64
	visits int64
	// slot holds one free chunk's index plus one, or zero, taken and filled
	// without the lock.
	slot atomic.Int32
	_    [128 - unsafe.Sizeof(sync.Mutex{}) - unsafe.Sizeof([]int32(nil)) - 20]byte
}

// init makes every one of count chunks free, spread over the shards. Each
// shard holds room for every chunk, so a chunk given back finds room in the
// first shard it tries, however the others fill meanwhile.
func (f *freeList) init(count int) {
	n := min(runtime.GOMAXPROCS(0), count)
	per := count
	//noalloc:exempt setup: a pool's free lists, set aside once.
	f.shards = make([]shard, n)
	for i := range f.shards {
		//noalloc:exempt setup: a pool's free lists, set aside once.
		f.shards[i].idx = make([]int32, 0, per)
	}
	//noalloc:exempt setup: a pool's handoff to waiting borrowers, set aside once.
	f.handoff = make(chan int32, count)
	for i := range count {
		s := &f.shards[i%n]
		s.idx = s.idx[:len(s.idx)+1]
		s.idx[len(s.idx)-1] = int32(i)
	}
}

// pop takes a free chunk's index, from the shards or from a handoff no
// waiter took.
func (f *freeList) pop(home int) (int32, bool) {
	if i, ok := f.popShards(home, true); ok {
		return i, true
	}
	select {
	case i := <-f.handoff:
		f.tookHandoff()
		return i, true
	default:
		f.empty.Add(1)
		return 0, false
	}
}

// popShards takes a chunk's index off the shards, counting it lent when
// count is set.
func (f *freeList) popShards(home int, count bool) (int32, bool) {
	at := home % len(f.shards)
	for k := range f.shards {
		s := &f.shards[(at+k)%len(f.shards)]
		if v := s.slot.Swap(0); v != 0 {
			if count {
				s.lent.Add(1)
			}
			return v - 1, true
		}
		s.mu.Lock()
		s.visits++
		if n := len(s.idx); n > 0 {
			i := s.idx[n-1]
			s.idx = s.idx[:n-1]
			if count {
				s.lent.Add(1)
			}
			s.mu.Unlock()
			return i, true
		}
		s.mu.Unlock()
	}
	return 0, false
}

// tookHandoff counts a chunk lent through handoff, against the first shard.
func (f *freeList) tookHandoff() {
	f.shards[0].lent.Add(1)
}

// free is how many chunks are free, on a shard or in handoff.
func (f *freeList) free() int {
	n := len(f.handoff)
	for i := range f.shards {
		s := &f.shards[i]
		s.mu.Lock()
		n += len(s.idx)
		s.mu.Unlock()
		if s.slot.Load() != 0 {
			n++
		}
	}
	return n
}

// wait takes a free chunk's index, waiting for one to be given back until
// done closes.
func (f *freeList) wait(done <-chan struct{}, home int) (int32, bool) {
	if i, ok := f.popShards(home, true); ok {
		return i, true
	}
	f.empty.Add(1)
	f.waiting.Add(1)
	defer f.waiting.Add(-1)
	// A chunk given back before waiting was counted is on a shard.
	if i, ok := f.popShards(home, true); ok {
		return i, true
	}
	if testHookWaiting != nil {
		testHookWaiting() //noalloc:exempt test hook: nil outside the tests.
	}
	since := time.Now()
	defer func() { f.waited.Add(int64(time.Since(since))) }()
	select {
	case i := <-f.handoff:
		f.tookHandoff()
		return i, true
	case <-done:
		return 0, false
	}
}

// push makes chunk i free on shard home, then sends a free chunk to handoff
// when a borrower is waiting. A waiter counts itself before it looks at the
// shards, so either it finds the chunk there or push sees it waiting.
func (f *freeList) push(i int32, home int) {
	if testHookPushing != nil {
		testHookPushing() //noalloc:exempt test hook: nil outside the tests.
	}
	s := &f.shards[home%len(f.shards)]
	if s.slot.CompareAndSwap(0, i+1) {
		f.handToWaiter(home)
		return
	}
	s.mu.Lock()
	s.visits++
	if len(s.idx) == cap(s.idx) {
		s.mu.Unlock()
		panic("bounded: a chunk given back twice")
	}
	s.idx = s.idx[:len(s.idx)+1]
	s.idx[len(s.idx)-1] = i
	s.mu.Unlock()
	f.handToWaiter(home)
}

// handToWaiter sends a free chunk to handoff when a borrower is waiting.
func (f *freeList) handToWaiter(home int) {
	if f.waiting.Load() > 0 {
		if j, ok := f.popShards(home, false); ok {
			f.handoff <- j
		}
	}
}

// testHookPushing runs as push puts a chunk on a shard, and testHookWaiting as
// a Wait finds every shard empty and blocks. Only tests set them.
var testHookPushing, testHookWaiting func()

func (f *freeList) stats() PoolStats {
	st := PoolStats{Empty: f.empty.Load(), Waited: time.Duration(f.waited.Load()), OverBudget: f.overBudget.Load()}
	for i := range f.shards {
		st.Lent += f.shards[i].lent.Load()
	}
	return st
}
