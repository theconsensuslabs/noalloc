package bounded

import (
	"bytes"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/theconsensuslabs/noalloc/alloctest"
)

// A Region borrows up to its budget, a Pool lends no more chunks than it
// holds, and Release gives every chunk back, zeroed when its type holds
// pointers.
func TestARegionBorrowsUpToItsBudget(t *testing.T) {
	p := NewPool[*int](16, 3)
	var r Region
	r.Reset(2 * p.ChunkBytes())
	a, b := r.Take(p), r.Take(p)
	if a == nil || b == nil || len(a) != 16 {
		t.Fatalf("two chunks within the budget: %v, %v", a, b)
	}
	if c := r.Take(p); c != nil || r.Held() != 2*p.ChunkBytes() {
		t.Errorf("a chunk past the budget was lent, or the region holds %d", r.Held())
	}
	a[0] = new(int)
	var other Region
	other.Reset(4 * p.ChunkBytes())
	if c := other.Take(p); c == nil || p.Free() != 0 {
		t.Fatalf("the last chunk: %v, %d free", c, p.Free())
	}
	if c := other.Take(p); c != nil {
		t.Error("an empty pool lent a chunk")
	}
	r.Release()
	other.Release()
	if p.Free() != 3 || r.Held() != 0 {
		t.Errorf("after release: %d free, %d held", p.Free(), r.Held())
	}
	for i := range p.Chunks() {
		for _, x := range p.chunks[i] {
			if x != nil {
				t.Fatalf("chunk %d came back holding %p", i, x)
			}
		}
	}
}

// A Map borrowing slots another Map left holds none of its entries.
func TestAMapFromReusedSlotsStartsEmpty(t *testing.T) {
	p := NewMapPool[int, int](4, 1)
	var r Region
	var m Map[int, int]
	r.Reset(1 << 20)
	m.ResetFrom(&r, p)
	m.Set(1, 10)
	r.Release()
	r.Reset(1 << 20)
	m.ResetFrom(&r, p)
	m.Set(2, 20)
	for k, v := range m.All() {
		if k != 2 {
			t.Errorf("a Map on reused slots holds the last Map's entry %d: %d", k, v)
		}
	}
}

// hasPointers tells types the collector follows into from those it does
// not.
func TestHasPointers(t *testing.T) {
	for _, c := range []struct {
		t    reflect.Type
		want bool
	}{
		{reflect.TypeFor[byte](), false},
		{reflect.TypeFor[[4]uint32](), false},
		{reflect.TypeFor[struct{ a, b int }](), false},
		{reflect.TypeFor[[0]*int](), false},
		{reflect.TypeFor[string](), true},
		{reflect.TypeFor[[]byte](), true},
		{reflect.TypeFor[*int](), true},
		{reflect.TypeFor[any](), true},
		{reflect.TypeFor[struct {
			a int
			b map[int]int
		}](), true},
		{reflect.TypeFor[[2]struct{ f func() }](), true},
		{reflect.TypeFor[slot[uint32, struct{}]](), false},
	} {
		if got := hasPointers(c.t); got != c.want {
			t.Errorf("hasPointers(%v) = %v, want %v", c.t, got, c.want)
		}
	}
}

// Wait takes a chunk once another region gives one back, refuses at once
// past the budget, and gives up when done closes.
func TestWaitTakesAChunkWhenOneComesFree(t *testing.T) {
	p := NewPool[int](4, 1)
	var holder, waiter Region
	holder.Reset(64)
	waiter.Reset(64)
	holder.Take(p)
	got := make(chan bool)
	go func() {
		_, ok := waiter.Wait(nil, p)
		got <- ok
	}()
	holder.Release()
	if !<-got {
		t.Error("Wait did not take the chunk given back")
	}
	done := make(chan struct{})
	close(done)
	if _, ok := holder.Wait(done, p); ok {
		t.Error("Wait took a chunk from an empty pool after done closed")
	}
	var small Region
	small.Reset(8)
	if _, ok := small.Wait(nil, p); ok {
		t.Error("Wait took a chunk past the region's budget")
	}
}

// Regions on many goroutines never hold the same chunk at once.
func TestRegionsShareAPoolSafely(t *testing.T) {
	p := NewPool[int](8, 4)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			var r Region
			for i := range 200 {
				r.Reset(64)
				if c, ok := r.Wait(nil, p); ok {
					for j := range c {
						c[j] = g*1000 + i
					}
					for j := range c {
						if c[j] != g*1000+i {
							t.Errorf("another region wrote into a borrowed chunk")
							return
						}
					}
				}
				r.Release()
			}
		})
	}
	wg.Wait()
	if p.Free() != 4 {
		t.Errorf("%d chunks free after every region released, want 4", p.Free())
	}
}

// A Vec reset from a region spans chunks as it fills, reads back in order,
// truncates across them, ends full when the region has no more room, and
// panics once the region is released.
func TestAVecFromARegionSpansChunks(t *testing.T) {
	p := NewPool[int](4, 8)
	var r Region
	r.Reset(3 * p.ChunkBytes())
	var v Vec[int]
	v.ResetFrom(&r, p)
	for i := range 14 {
		if ok := v.Push(i); ok != (i < 12) {
			t.Fatalf("Push(%d) = %v, want %v", i, ok, i < 12)
		}
	}
	if !v.Full() || v.Len() != 12 || v.Cap() != 12 {
		t.Fatalf("full %v, len %d, cap %d", v.Full(), v.Len(), v.Cap())
	}
	for i := range 12 {
		if v.At(i) != i {
			t.Fatalf("At(%d) = %d", i, v.At(i))
		}
	}
	v.Truncate(5)
	for i := range 7 {
		if !v.Push(100 + i) {
			t.Fatalf("Push after Truncate(5) did not fit at %d", i)
		}
	}
	if v.At(4) != 4 || v.At(5) != 100 || v.At(11) != 106 || r.Held() != 3*p.ChunkBytes() {
		t.Errorf("after refill: At(4) %d, At(5) %d, At(11) %d, held %d", v.At(4), v.At(5), v.At(11), r.Held())
	}
	r.Release()
	defer func() {
		if recover() == nil {
			t.Error("a Vec read after its region was released")
		}
	}()
	v.At(0)
}

// A Buf and a Map borrow one chunk each, and say so when the region has
// none to give.
func TestABufAndAMapBorrowOneChunk(t *testing.T) {
	bytes := NewPool[byte](8, 1)
	maps := NewMapPool[string, int](4, 1)
	var r Region
	r.Reset(1 << 20)
	var b Buf
	if !b.ResetFrom(&r, bytes, nil) || b.Size() != 8 {
		t.Fatalf("a Buf from the region: size %d", b.Size())
	}
	b.WriteString("12345678")
	if b.Full() || string(b.Bytes()) != "12345678" {
		t.Errorf("a Buf from the region holds %q", b.Bytes())
	}
	var m Map[string, int]
	if !m.ResetFrom(&r, maps) || m.Limit() != 4 {
		t.Fatalf("a Map from the region: limit %d", m.Limit())
	}
	for i, k := range []string{"a", "b", "c", "d", "e"} {
		if ok := m.Set(k, i); ok != (i < 4) {
			t.Errorf("Set(%q) = %v", k, ok)
		}
	}
	var b2 Buf
	if b2.ResetFrom(&r, bytes, nil) || !b2.Full() {
		t.Error("a Buf from an empty pool has memory")
	}
	var m2 Map[string, int]
	if m2.ResetFrom(&r, maps) || m2.Set("a", 1) {
		t.Error("a Map from an empty pool holds an entry")
	}
	r.Release()
	if bytes.Free() != 1 || maps.Free() != 1 {
		t.Errorf("after release: %d and %d free", bytes.Free(), maps.Free())
	}
}

func TestBorrowingAllocatesNothing(t *testing.T) {
	ints := NewPool[int](16, 4)
	bytes := NewPool[byte](64, 2)
	maps := NewMapPool[uint32, int](8, 2)
	var r Region
	var v Vec[int]
	var b Buf
	var m Map[uint32, int]
	alloctest.Zero(t, "borrowing, filling, and releasing", func() {
		r.Reset(1 << 20)
		v.ResetFrom(&r, ints)
		for i := range 40 {
			v.Push(i)
		}
		_ = v.At(39)
		b.ResetFrom(&r, bytes, nil)
		b.WriteString("x")
		m.ResetFrom(&r, maps)
		m.Set(1, 1)
		r.Release()
	})
}

// A Buf that may borrow moves once, when it fills, to a larger chunk holding
// what it wrote, and leaves its own memory as it was; one whose region cannot
// borrow flushes or fills as it would have.
func TestABufMovesOnceToABorrowedChunk(t *testing.T) {
	p := NewPool[byte](64, 1)
	var r Region
	r.Reset(1 << 20)
	own := make([]byte, 8)
	var b Buf
	b.Reset(own, nil)
	b.Borrow(&r, NewClasses(p))
	b.WriteString("abcdef")
	view := b.Bytes()
	b.WriteString("ghijkl")
	if b.Full() || string(b.Bytes()) != "abcdefghijkl" || b.Size() != 64 {
		t.Fatalf("after filling: %q, size %d, full %v", b.Bytes(), b.Size(), b.Full())
	}
	if string(view) != "abcdef" || string(own[:6]) != "abcdef" {
		t.Errorf("the memory the buffer moved from changed: %q", own)
	}
	b.WriteString(string(make([]byte, 60)))
	if !b.Full() {
		t.Error("a buffer moved once and filled again did not fill")
	}
	r.Release()

	var w bytes.Buffer
	r.Reset(1 << 20)
	held := r.Take(p)
	var f Buf
	f.Reset(make([]byte, 4), &w)
	f.Borrow(&r, NewClasses(p))
	f.WriteString("abcdefgh")
	f.Flush()
	if held == nil || w.String() != "abcdefgh" || f.Size() != 4 {
		t.Errorf("with the pool lent: wrote %q through %d bytes", w.String(), f.Size())
	}
	r.Release()
}

// A pool counts what it lent, the borrows that found it empty, and the
// borrows a budget refused.
func TestAPoolCountsWhatItLent(t *testing.T) {
	p := NewPool[byte](8, 1)
	var a, b Region
	a.Reset(64)
	b.Reset(64)
	a.Take(p)
	b.Take(p)
	testHookWaiting = a.Release
	b.Wait(nil, p)
	testHookWaiting = nil
	var small Region
	small.Reset(4)
	small.Take(p)
	st := p.Stats()
	if st.Lent != 2 || st.Empty != 2 || st.Waited < 0 || st.OverBudget != 1 {
		t.Errorf("stats %+v, want 2 lent, 2 empty, no negative wait, 1 over budget", st)
	}
	b.Release()
}

// BenchmarkBorrow times a Region borrowing a chunk and giving it back, on one
// goroutine and on all of them sharing one pool.
func BenchmarkBorrow(b *testing.B) {
	p := NewPool[byte](16<<10, 64)
	b.Run("one", func(b *testing.B) {
		var r Region
		for b.Loop() {
			r.Reset(16 << 10)
			r.Take(p)
			r.Release()
		}
	})
	b.Run("parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			var r Region
			for pb.Next() {
				r.Reset(16 << 10)
				r.Take(p)
				r.Release()
			}
		})
	})
}

// visits is how many times p's shards were locked.
func visits[T any](p *Pool[T]) int64 {
	var n int64
	for i := range p.free.shards {
		s := &p.free.shards[i]
		s.mu.Lock()
		n += s.visits
		s.mu.Unlock()
	}
	return n
}

// poolBytes is the memory every borrower shares: p itself and every array it
// holds, but for the chunks' contents and shard home, which a borrow from
// home may write.
func poolBytes[T any](p *Pool[T], home int) []byte {
	out := slices.Clone(unsafe.Slice((*byte)(unsafe.Pointer(p)), unsafe.Sizeof(*p)))
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Struct:
			for i := range v.NumField() {
				walk(v.Field(i))
			}
		case reflect.Slice:
			if v.Cap() == 0 {
				return
			}
			elem := v.Type().Elem()
			if elem != reflect.TypeFor[shard]() {
				out = append(out, unsafe.Slice((*byte)(v.UnsafePointer()), v.Cap()*int(elem.Size()))...)
			}
			for i := range v.Len() {
				if elem == reflect.TypeFor[shard]() && i != home%v.Len() {
					x := v.Index(i)
					out = append(out, unsafe.Slice((*byte)(x.Addr().UnsafePointer()), elem.Size())...)
				}
				if elem.Kind() == reflect.Struct && (elem != reflect.TypeFor[shard]() || i != home%v.Len()) {
					walk(v.Index(i))
				}
			}
		}
	}
	walk(reflect.ValueOf(p).Elem())
	return out
}

// An uncontended borrow, of the chunk in its region's home shard's slot,
// takes no lock to take it or to give it back, and writes nothing to the
// pool's own memory, which every borrower on every processor reads: a shared
// write on each borrow is what made borrowing eight times slower on every
// core.
func TestAnUncontendedBorrowTouchesOneShardAndNoSharedMemory(t *testing.T) {
	p := NewPool[byte](64, 64)
	var r Region
	r.Reset(1 << 20)
	r.Take(p) // the home shard's chunk, given back below, starts the loop there
	r.Release()
	before, shared := visits(p), poolBytes(p, r.home())
	const n = 1000
	for i := range n {
		r.Reset(1 << 20)
		if r.Take(p) == nil {
			t.Fatal("a pool with free chunks lent none")
		}
		if i == 0 && !bytes.Equal(poolBytes(p, r.home()), shared) {
			t.Error("an uncontended borrow wrote to the pool's own memory")
		}
		r.Release()
	}
	if got := visits(p) - before; got != 0 {
		t.Errorf("%d borrows locked shards %d times, want none", n, got)
	}
	if !bytes.Equal(poolBytes(p, r.home()), shared) {
		t.Error("uncontended borrows wrote to the pool's own memory")
	}
	if st := p.Stats(); st.Empty != 0 || st.OverBudget != 0 || st.Waited != 0 {
		t.Errorf("uncontended borrows counted %+v", st)
	}
}

// As many Regions as a pool has shards each borrow from a shard of their
// own, which is what keeps borrowers on different processors apart; homes
// that collide made borrowing on every core up to eight times slower.
func TestRegionsBorrowFromShardsOfTheirOwn(t *testing.T) {
	p := NewPool[byte](64, 4*runtime.GOMAXPROCS(0))
	n := len(p.free.shards)
	seen := map[int]bool{}
	for range n {
		r := new(Region)
		r.Reset(1 << 20)
		seen[r.home()%n] = true
	}
	if len(seen) != n {
		t.Errorf("%d new regions borrow first from %d of the pool's %d shards", n, len(seen), n)
	}
}

// Reserve borrows all of a reservation or none of it, and Take hands out the
// reserved chunks before borrowing past them.
func TestReserveBorrowsAllOrNone(t *testing.T) {
	p := NewPool[byte](8, 3)
	var r Region
	r.Reset(1 << 20)
	if !r.Reserve(nil, p, 2) || p.Free() != 1 || r.Held() != 16 {
		t.Fatalf("a reservation of 2: %d free, %d held", p.Free(), r.Held())
	}
	a, b := r.Take(p), r.Take(p)
	if a == nil || b == nil || p.Free() != 1 || p.Stats().Lent != 2 {
		t.Errorf("Takes of a reservation borrowed past it: %d free, %+v", p.Free(), p.Stats())
	}
	if c := r.Take(p); c == nil || p.Free() != 0 {
		t.Error("a Take past the reservation borrowed nothing")
	}
	r.Release()

	var holder Region
	holder.Reset(1 << 20)
	holder.Take(p)
	holder.Take(p)
	done := make(chan struct{})
	close(done)
	r.Reset(1 << 20)
	if r.Reserve(done, p, 2) || r.Held() != 0 || p.Free() != 1 {
		t.Errorf("a reservation that could not be met: held %d, %d free", r.Held(), p.Free())
	}
	var small Region
	small.Reset(8)
	if small.Reserve(nil, p, 2) || p.Free() != 1 {
		t.Error("a reservation past the budget borrowed")
	}
	holder.Release()
	r.Release()
	if p.Free() != 3 {
		t.Errorf("after release, %d of 3 free", p.Free())
	}
}

// A reservation waits for a chunk another region gives back.
func TestReserveWaitsForChunksGivenBack(t *testing.T) {
	p := NewPool[byte](8, 2)
	var holder, r Region
	holder.Reset(1 << 20)
	r.Reset(1 << 20)
	holder.Take(p)
	waited := false
	testHookWaiting = func() { waited = true; holder.Release() }
	defer func() { testHookWaiting = nil }()
	if !r.Reserve(nil, p, 2) || r.Held() != 16 || !waited {
		t.Errorf("the reservation was not met once a chunk came back: held %d", r.Held())
	}
	r.Release()
}

// BenchmarkBorrowAgainstSyncPool times borrowing and returning a 16 KB buffer
// from a Pool and from a sync.Pool, on one goroutine and on all of them.
func BenchmarkBorrowAgainstSyncPool(b *testing.B) {
	sp := sync.Pool{New: func() any { return new([16 << 10]byte) }}
	p := NewPool[byte](16<<10, 64)
	b.Run("sync.Pool one", func(b *testing.B) {
		for b.Loop() {
			x := sp.Get().(*[16 << 10]byte)
			x[0] = 1
			sp.Put(x)
		}
	})
	b.Run("bounded one", func(b *testing.B) {
		var r Region
		for b.Loop() {
			r.Reset(16 << 10)
			r.Take(p)[0] = 1
			r.Release()
		}
	})
	b.Run("sync.Pool parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				x := sp.Get().(*[16 << 10]byte)
				x[0] = 1
				sp.Put(x)
			}
		})
	})
	b.Run("bounded parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			r := new(Region)
			for pb.Next() {
				r.Reset(16 << 10)
				r.Take(p)[0] = 1
				r.Release()
			}
		})
	})
}

// Every way a chunk comes back reaches every way of waiting for one, when it
// comes back as the waiter finds every shard empty and before it blocks.
func TestEachGiveBackReachesEachWaiter(t *testing.T) {
	givers := map[string]func(t *testing.T, p *Pool[byte]) (give func()){
		"Release": func(t *testing.T, p *Pool[byte]) func() {
			var r Region
			r.Reset(p.ChunkBytes())
			if r.Take(p) == nil {
				t.Fatal("the pool's one chunk was not lent")
			}
			return r.Release
		},
		"GiveBack": func(t *testing.T, p *Pool[byte]) func() {
			var r Region
			r.Reset(p.ChunkBytes())
			chunk := r.Take(p)
			if chunk == nil {
				t.Fatal("the pool's one chunk was not lent")
			}
			return func() { r.GiveBack(p, chunk) }
		},
		"a Slice moving up a class": func(t *testing.T, p *Pool[byte]) func() {
			var r Region
			r.Reset(1 << 20)
			var s Slice[byte]
			s.ResetFrom(&r, NewClasses(p, NewPool[byte](2*p.ChunkLen(), 1)))
			if !s.Extend(make([]byte, p.ChunkLen())) || p.Free() != 0 {
				t.Fatal("the Slice did not take the pool's one chunk")
			}
			return func() { s.Push(1) }
		},
	}
	waiters := map[string]func(r *Region, p *Pool[byte]) bool{
		"Wait": func(r *Region, p *Pool[byte]) bool {
			_, ok := r.Wait(nil, p)
			return ok
		},
		"Reserve": func(r *Region, p *Pool[byte]) bool { return r.Reserve(nil, p, 1) },
	}
	for gn, giver := range givers {
		for wn, wait := range waiters {
			t.Run(gn+" to "+wn, func(t *testing.T) {
				p := NewPool[byte](64, 1)
				give := giver(t, p)
				var waiter Region
				waiter.Reset(p.ChunkBytes())
				pushing, pushGo := make(chan struct{}), make(chan struct{})
				waiting, waitGo := make(chan struct{}), make(chan struct{})
				testHookPushing = func() { close(pushing); <-pushGo }
				testHookWaiting = func() { close(waiting); <-waitGo }
				defer func() { testHookPushing, testHookWaiting = nil, nil }()

				given := make(chan struct{})
				go func() { give(); close(given) }()
				<-pushing
				got := make(chan bool)
				go func() { got <- wait(&waiter, p) }()
				<-waiting
				close(pushGo)
				<-given
				close(waitGo)

				select {
				case ok := <-got:
					if !ok {
						t.Fatal("the waiter failed with no done channel")
					}
				case <-time.After(time.Minute):
					t.Fatalf("the waiter still blocked after a minute with %d chunk(s) free", p.Free())
				}
				testHookPushing, testHookWaiting = nil, nil
				waiter.Release()
				if p.Free() != 1 {
					t.Errorf("%d chunks free after the waiter released, want 1", p.Free())
				}
			})
		}
	}
}

// Through every way of borrowing and giving back, each pool's lent chunks are
// exactly what its regions hold.
func TestEveryChunkIsFreeOrHeldOnce(t *testing.T) {
	small, large := NewPool[byte](16, 6), NewPool[byte](64, 4)
	classes := NewClasses(small, large)
	maps := NewMapClasses(NewMapPool[int, int](4, 4), NewMapPool[int, int](16, 4))
	pools := []interface {
		Chunks() int
		Free() int
		ChunkBytes() int
	}{small, large, maps.Pools()[0], maps.Pools()[1]}
	closed := make(chan struct{})
	close(closed)
	regions := make([]Region, 3)
	bufs, slices, ms := make([]Buf, 3), make([]Slice[byte], 3), make([]Map[int, int], 3)
	var taken [3][][]byte
	ops := []func(i int, rng *rand.Rand){
		func(i int, _ *rand.Rand) {
			if c := regions[i].Take(small); c != nil {
				taken[i] = append(taken[i], c)
			}
		},
		func(i int, _ *rand.Rand) {
			if c, ok := regions[i].Wait(closed, large); ok {
				taken[i] = append(taken[i], c)
			}
		},
		func(i int, rng *rand.Rand) { regions[i].Reserve(closed, small, 1+rng.IntN(3)) },
		func(i int, _ *rand.Rand) {
			if n := len(taken[i]); n > 0 {
				c := taken[i][n-1]
				taken[i] = taken[i][:n-1]
				if len(c) == small.ChunkLen() {
					regions[i].GiveBack(small, c)
				} else {
					regions[i].GiveBack(large, c)
				}
			}
		},
		func(i int, _ *rand.Rand) {
			regions[i].Release()
			taken[i] = nil
			bufs[i], slices[i], ms[i] = Buf{}, Slice[byte]{}, Map[int, int]{}
		},
		func(i int, rng *rand.Rand) {
			if bufs[i].Size() == 0 {
				bufs[i].ResetFrom(&regions[i], small, nil)
				bufs[i].Borrow(&regions[i], classes)
			}
			bufs[i].Write(make([]byte, rng.IntN(40)))
		},
		func(i int, rng *rand.Rand) {
			if slices[i].Cap() == 0 {
				slices[i].ResetFrom(&regions[i], classes)
			}
			slices[i].Extend(make([]byte, rng.IntN(30)))
		},
		func(i int, rng *rand.Rand) {
			if ms[i].Limit() == 0 {
				ms[i].Grow(&regions[i], maps)
			}
			ms[i].Set(rng.IntN(100), i)
		},
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range regions {
		regions[i].Reset(1 << 20)
	}
	for step := range 20000 {
		i, op := rng.IntN(len(regions)), rng.IntN(len(ops))
		ops[op](i, rng)
		lent, held := 0, 0
		for _, p := range pools {
			lent += (p.Chunks() - p.Free()) * p.ChunkBytes()
		}
		for k := range regions {
			held += regions[k].Held()
		}
		if lent != held {
			t.Fatalf("step %d, op %d: pools lent %d bytes and regions hold %d", step, op, lent, held)
		}
	}
}
