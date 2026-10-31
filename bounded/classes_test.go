package bounded

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/theconsensuslabs/noalloc/alloctest"
)

// A Vec in chunks finds each item by shift and mask, yields its chunks in
// order, and extends across them all or not at all.
func TestAVecInChunksReadsBackInOrder(t *testing.T) {
	p := NewPool[int](8, 8)
	var r Region
	r.Reset(4 * p.ChunkBytes())
	var v Vec[int]
	v.ResetFrom(&r, p)
	want := make([]int, 0, 30)
	for i := range 30 {
		v.Push(i)
		want = append(want, i)
	}
	for i := range want {
		if v.At(i) != want[i] {
			t.Fatalf("At(%d) = %d", i, v.At(i))
		}
	}
	var got []int
	for c := range v.Chunks() {
		if len(c) > 8 {
			t.Errorf("a chunk of %d items, past 8", len(c))
		}
		got = append(got, c...)
	}
	if !slices.Equal(got, want) {
		t.Errorf("Chunks yielded %v", got)
	}
	for i, x := range v.All() {
		if x != want[i] {
			t.Fatalf("All yielded %d at %d", x, i)
		}
	}
	if !v.Extend([]int{30, 31}) || v.Len() != 32 || v.At(31) != 31 {
		t.Errorf("Extend into the last room: len %d", v.Len())
	}
	if v.Extend([]int{32}) || v.Len() != 32 || !v.Full() {
		t.Errorf("an Extend past the budget added items or left the Vec not full: len %d", v.Len())
	}
	v.Truncate(20)
	if !v.Extend(make([]int, 12)) || v.Len() != 32 {
		t.Errorf("an Extend across chunks after Truncate: len %d", v.Len())
	}
	if v.Extend(make([]int, 1)) || v.Len() != 32 {
		t.Error("a failed Extend changed the Vec")
	}
	r.Release()
}

func TestAVecBorrowsOnlyPowerOfTwoChunks(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a Vec borrowed chunks of 6 items")
		}
	}()
	var r Region
	r.Reset(1 << 20)
	var v Vec[int]
	v.ResetFrom(&r, NewPool[int](6, 1))
}

// GiveBack returns one chunk before its region is released, and refuses a
// chunk the region does not hold.
func TestGiveBackReturnsOneChunk(t *testing.T) {
	p := NewPool[byte](16, 2)
	var r Region
	r.Reset(1 << 20)
	a, b := r.Take(p), r.Take(p)
	r.GiveBack(p, a)
	if p.Free() != 1 || r.Held() != 16 {
		t.Errorf("after one chunk given back: %d free, %d held", p.Free(), r.Held())
	}
	for _, bad := range [][]byte{a, make([]byte, 16), b[1:]} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("GiveBack took a chunk the region does not hold")
				}
			}()
			r.GiveBack(p, bad)
		}()
	}
	r.Release()
	if p.Free() != 2 {
		t.Errorf("after release: %d free", p.Free())
	}
}

// A Slice moves up through its classes as it fills, keeping what it holds
// and giving back each chunk it leaves, skips a class with none free, and is
// full once no class has room.
func TestASliceMovesUpThroughItsClasses(t *testing.T) {
	small, mid, large := NewPool[int](4, 2), NewPool[int](16, 1), NewPool[int](64, 1)
	c := NewClasses(small, mid, large)
	var r Region
	r.Reset(1 << 20)
	var s Slice[int]
	s.ResetFrom(&r, c)
	for i := range 64 {
		if !s.Push(i) {
			t.Fatalf("Push(%d) did not fit", i)
		}
	}
	if s.Cap() != 64 || small.Free() != 2 || mid.Free() != 1 || large.Free() != 0 {
		t.Errorf("at 64 items: cap %d, free %d %d %d", s.Cap(), small.Free(), mid.Free(), large.Free())
	}
	for i, x := range s.Items() {
		if x != i {
			t.Fatalf("item %d is %d after moving", i, x)
		}
	}
	if lent := small.Stats().Lent + mid.Stats().Lent + large.Stats().Lent; lent != 3 {
		t.Errorf("growing to 64 items borrowed %d chunks, want one of each class", lent)
	}
	if s.Push(64) || !s.Full() || s.Len() != 64 {
		t.Error("a Push past the largest class fit")
	}
	r.Release()

	r.Reset(1 << 20)
	held := r.Take(mid)
	s.ResetFrom(&r, c)
	if !s.Extend(make([]int, 10)) || s.Cap() != 64 {
		t.Errorf("with the middle class lent, 10 items went to a chunk of %d", s.Cap())
	}
	_ = held
	r.Release()

	own := []int{7, 7, 7}
	s.Reset(own[:2:2])
	s.Grow(&r, c)
	r.Reset(1 << 20)
	s.Push(1)
	s.Push(2)
	s.Push(3)
	if !slices.Equal(s.Items(), []int{1, 2, 3}) || !slices.Equal(own, []int{1, 2, 7}) || small.Free() != 1 {
		t.Errorf("growing out of its own memory: %v, own %v, %d small free", s.Items(), own, small.Free())
	}
	r.Release()
}

// A Buf moves up through classes each time it fills, giving back the chunk
// it leaves unless bytes in it are pinned.
func TestABufMovesUpThroughClasses(t *testing.T) {
	mid, large := NewPool[byte](16, 1), NewPool[byte](64, 1)
	var r Region
	r.Reset(1 << 20)
	var b Buf
	b.Reset(make([]byte, 4), nil)
	b.Borrow(&r, NewClasses(mid, large))
	b.WriteString("abcdefgh")
	if b.Size() != 16 || mid.Free() != 0 {
		t.Fatalf("after 8 bytes: size %d", b.Size())
	}
	b.WriteString("ijklmnopqrstuvwxyz")
	if b.Size() != 64 || mid.Free() != 1 || string(b.Bytes()) != "abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("after 26 bytes: size %d, %d middle free, %q", b.Size(), mid.Free(), b.Bytes())
	}
	r.Release()

	r.Reset(1 << 20)
	b.Reset(make([]byte, 4), nil)
	b.Borrow(&r, NewClasses(mid, large))
	b.WriteString("abcdefgh")
	b.Pin(8)
	b.WriteString("ijklmnopqrstuvwxyz")
	if b.Size() != 64 || mid.Free() != 0 {
		t.Errorf("a pinned chunk was given back: size %d, %d middle free", b.Size(), mid.Free())
	}
	r.Release()
}

// A Map that grows through classes answers as a Go map does across random
// sets, gets, and deletes, and gives back the slots it leaves.
func TestAGrowingMapMatchesAGoMap(t *testing.T) {
	pools := []*MapPool[int, int]{NewMapPool[int, int](8, 1), NewMapPool[int, int](32, 1), NewMapPool[int, int](128, 1)}
	rng := rand.New(rand.NewPCG(1, 2))
	var r Region
	r.Reset(1 << 20)
	var m Map[int, int]
	m.Reset(2)
	m.Grow(&r, NewMapClasses(pools...))
	want := map[int]int{}
	for range 2000 {
		k := rng.IntN(200)
		switch rng.IntN(3) {
		case 0, 1:
			ok := m.Set(k, k*3)
			_, had := want[k]
			if ok != (had || len(want) < 128) {
				t.Fatalf("Set(%d) = %v with %d entries", k, ok, len(want))
			}
			if ok {
				want[k] = k * 3
			}
		case 2:
			m.Delete(k)
			delete(want, k)
		}
		if m.Len() != len(want) {
			t.Fatalf("len %d, want %d", m.Len(), len(want))
		}
	}
	for k, v := range want {
		if got, ok := m.Get(k); !ok || got != v {
			t.Errorf("Get(%d) = %d, %v; want %d", k, got, ok, v)
		}
	}
	if pools[0].Free() != 1 || pools[1].Free() != 1 || pools[2].Free() != 0 {
		t.Errorf("free after growing: %d %d %d", pools[0].Free(), pools[1].Free(), pools[2].Free())
	}
	r.Release()
}

func TestGrowingAllocatesNothing(t *testing.T) {
	ints := NewClasses(NewPool[int](8, 2), NewPool[int](64, 2))
	bytes := NewClasses(NewPool[byte](64, 2), NewPool[byte](512, 2))
	maps := NewMapClasses(NewMapPool[uint32, int](4, 2), NewMapPool[uint32, int](64, 2))
	chunked := NewPool[int](16, 8)
	var r Region
	var s Slice[int]
	var v Vec[int]
	var b Buf
	var m Map[uint32, int]
	own := make([]byte, 8)
	sum := 0
	alloctest.Zero(t, "growing a Slice, a Vec, a Buf, and a Map", func() {
		r.Reset(1 << 20)
		s.ResetFrom(&r, ints)
		for i := range 50 {
			s.Push(i)
		}
		v.ResetFrom(&r, chunked)
		for i := range 50 {
			v.Push(i)
		}
		for c := range v.Chunks() {
			for _, x := range c {
				sum += x
			}
		}
		b.Reset(own, nil)
		b.Borrow(&r, bytes)
		for range 20 {
			b.WriteString("0123456789")
		}
		m.ResetFrom(&r, maps.Pools()[0])
		m.Grow(&r, maps)
		for i := range uint32(40) {
			m.Set(i, int(i))
		}
		r.Release()
	})
}

var benchSum int

// BenchmarkVecRead reads 4,096 ints from a plain slice, a Vec in chunks by
// At, and the same Vec a chunk at a time.
func BenchmarkVecRead(b *testing.B) {
	p := NewPool[int](256, 16)
	var r Region
	r.Reset(1 << 20)
	var v Vec[int]
	v.ResetFrom(&r, p)
	plain := make([]int, 4096)
	for i := range plain {
		plain[i] = i
		v.Push(i)
	}
	b.Run("slice", func(b *testing.B) {
		for b.Loop() {
			sum := 0
			for _, x := range plain {
				sum += x
			}
			benchSum = sum
		}
	})
	b.Run("At", func(b *testing.B) {
		for b.Loop() {
			sum := 0
			for i := range v.Len() {
				sum += v.At(i)
			}
			benchSum = sum
		}
	})
	b.Run("Chunks", func(b *testing.B) {
		for b.Loop() {
			sum := 0
			for c := range v.Chunks() {
				for _, x := range c {
					sum += x
				}
			}
			benchSum = sum
		}
	})
}

// Truncate keeps a Slice in the chunk it moved to and clears what it drops,
// and Clear empties it for its memory's next user.
func TestASliceTruncatesInPlace(t *testing.T) {
	c := NewClasses(NewPool[*int](4, 2), NewPool[*int](16, 2))
	var r Region
	r.Reset(1 << 20)
	var s Slice[*int]
	s.ResetFrom(&r, c)
	for i := range 6 {
		s.Push(&i)
	}
	held := s.Items()[:cap(s.Items())]
	s.Truncate(2)
	if s.Len() != 2 || s.Cap() != 16 || s.Full() {
		t.Errorf("after Truncate(2): len %d, cap %d, full %v", s.Len(), s.Cap(), s.Full())
	}
	for i, p := range held[2:6] {
		if p != nil {
			t.Errorf("item %d still points at %d after Truncate", i+2, *p)
		}
	}
	s.Clear()
	if s.Len() != 0 || s.Cap() != 16 || held[0] != nil {
		t.Errorf("after Clear: len %d, cap %d", s.Len(), s.Cap())
	}
	defer func() {
		if recover() == nil {
			t.Error("Truncate past Len did not panic")
		}
	}()
	s.Truncate(1)
	r.Release()
}

// Every method of a container that borrows through a Region and reads or
// writes its chunks panics once the Region is released.
func TestEachMethodPanicsAfterItsRegionIsReleased(t *testing.T) {
	pool := NewPool[int](8, 4)
	classes := NewClasses(pool, NewPool[int](32, 2))
	type kind struct {
		fill    func(r *Region) any
		methods map[string]func(c any)
	}
	kinds := map[string]kind{
		"Vec": {
			fill: func(r *Region) any {
				v := &Vec[int]{}
				v.ResetFrom(r, pool)
				v.Push(1)
				return v
			},
			methods: map[string]func(c any){
				"Push":     func(c any) { c.(*Vec[int]).Push(2) },
				"Extend":   func(c any) { c.(*Vec[int]).Extend([]int{2}) },
				"At":       func(c any) { c.(*Vec[int]).At(0) },
				"Items":    func(c any) { c.(*Vec[int]).Items() },
				"Chunks":   func(c any) { c.(*Vec[int]).Chunks() },
				"All":      func(c any) { c.(*Vec[int]).All() },
				"Truncate": func(c any) { c.(*Vec[int]).Truncate(0) },
				"Clear":    func(c any) { c.(*Vec[int]).Clear() },
			},
		},
		"Slice": {
			fill: func(r *Region) any {
				s := &Slice[int]{}
				s.ResetFrom(r, classes)
				s.Push(1)
				return s
			},
			methods: map[string]func(c any){
				"Push":     func(c any) { c.(*Slice[int]).Push(2) },
				"Extend":   func(c any) { c.(*Slice[int]).Extend([]int{2}) },
				"Items":    func(c any) { c.(*Slice[int]).Items() },
				"Truncate": func(c any) { c.(*Slice[int]).Truncate(0) },
				"Clear":    func(c any) { c.(*Slice[int]).Clear() },
			},
		},
	}
	for kn, k := range kinds {
		for mn, method := range k.methods {
			t.Run(kn+"."+mn, func(t *testing.T) {
				var r Region
				r.Reset(1 << 10)
				c := k.fill(&r)
				r.Release()
				r.Reset(1 << 10)
				defer func() {
					if recover() == nil {
						t.Errorf("%s.%s ran on chunks its region gave back", kn, mn)
					}
				}()
				method(c)
			})
		}
	}
}
