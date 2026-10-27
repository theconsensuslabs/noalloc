package bounded

import (
	"math"
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/theconsensuslabs/noalloc/alloctest"
)

type pair struct {
	a int
	b string
}

// TestMapMatchesAGoMap runs random sets, gets, deletes, and clears against a
// Map and a Go map with the same limit, checking every answer and the
// contents after each step.
func TestMapMatchesAGoMap(t *testing.T) {
	for _, limit := range []int{1, 2, 7, 64} {
		r := rand.New(rand.NewPCG(uint64(limit), 1))
		var m Map[int, int]
		m.Reset(limit)
		model, full := map[int]int{}, false
		for step := range 20000 {
			k := r.IntN(3 * limit)
			switch op := r.IntN(10); {
			case op < 5:
				v := r.Int()
				_, had := model[k]
				fits := had || len(model) < limit
				if fits {
					model[k] = v
				} else {
					full = true
				}
				if got := m.Set(k, v); got != fits {
					t.Fatalf("limit %d step %d: Set(%d) = %v, want %v", limit, step, k, got, fits)
				}
			case op < 8:
				delete(model, k)
				m.Delete(k)
			case op < 9:
				want, wantOK := model[k]
				if got, ok := m.Get(k); ok != wantOK || got != want {
					t.Fatalf("limit %d step %d: Get(%d) = %d, %v; want %d, %v", limit, step, k, got, ok, want, wantOK)
				}
			default:
				clear(model)
				full = false
				m.Clear()
			}
			if m.Len() != len(model) || m.Full() != full {
				t.Fatalf("limit %d step %d: len %d full %v, want %d %v", limit, step, m.Len(), m.Full(), len(model), full)
			}
			seen := 0
			for k, v := range m.All() {
				seen++
				if want, ok := model[k]; !ok || want != v {
					t.Fatalf("limit %d step %d: All yields %d=%d, the model holds %d, %v", limit, step, k, v, want, ok)
				}
			}
			if seen != len(model) {
				t.Fatalf("limit %d step %d: All yields %d entries, want %d", limit, step, seen, len(model))
			}
		}
	}
}

func TestMapTakesStructKeys(t *testing.T) {
	var m Map[pair, int]
	m.Reset(4)
	m.Set(pair{1, "a"}, 1)
	m.Set(pair{1, "b"}, 2)
	if v, ok := m.Get(pair{1, "b"}); !ok || v != 2 {
		t.Errorf("Get(pair{1, b}) = %d, %v; want 2, true", v, ok)
	}
	if _, ok := m.Get(pair{2, "a"}); ok {
		t.Error("Get of a key never set found one")
	}
}

func TestANeverResetMapHoldsNothing(t *testing.T) {
	var m Map[string, int]
	if m.Set("a", 1) || !m.Full() {
		t.Error("Set on a Map never reset fit")
	}
	if _, ok := m.Get("a"); ok {
		t.Error("Get on a Map never reset found a key")
	}
	m.Delete("a")
}

// TestMapClearsAcrossTheGenerationWrap clears a Map through the last
// generation, where an old slot would otherwise read as current.
func TestMapClearsAcrossTheGenerationWrap(t *testing.T) {
	var m Map[int, int]
	m.Reset(4)
	m.gen = math.MaxUint32 - 2
	for range 4 {
		m.Set(1, 1)
		m.Clear()
	}
	if _, ok := m.Get(1); ok || m.Len() != 0 || m.gen == 0 {
		t.Errorf("after clears across the wrap: found a cleared key or gen %d", m.gen)
	}
	m.Set(2, 2)
	if v, ok := m.Get(2); !ok || v != 2 {
		t.Errorf("after the wrap: Get(2) = %d, %v", v, ok)
	}
}

// TestADroppedMapHoldsNothing drops a Map holding pointers: every slot reads
// as zero, so the Map keeps nothing alive, and it takes entries again.
func TestADroppedMapHoldsNothing(t *testing.T) {
	var m Map[int, *pair]
	m.Reset(4)
	for i := range 4 {
		m.Set(i, &pair{a: i})
	}
	m.Drop()
	var zero slot[int, *pair]
	for i, s := range m.slots {
		if s != zero {
			t.Fatalf("slot %d still holds %+v after Drop", i, s)
		}
	}
	if m.Len() != 0 || m.Full() {
		t.Errorf("after Drop: Len %d, Full %v", m.Len(), m.Full())
	}
	if _, ok := m.Get(1); ok {
		t.Error("a dropped key was found")
	}
	if !m.Set(5, &pair{a: 5}) {
		t.Error("a dropped Map took no entry")
	}
	if v, ok := m.Get(5); !ok || v.a != 5 {
		t.Errorf("after Drop: Get(5) = %v, %v", v, ok)
	}
}

func TestMapAllocatesNothing(t *testing.T) {
	var m Map[string, int]
	m.Reset(16)
	keys := []string{"a", "bb", "ccc", "dddd", "eeeee"}
	sum := 0
	alloctest.Zero(t, "a round of sets, gets, a range, a delete, a clear, and a drop", func() {
		for i, k := range keys {
			m.Set(k, i)
		}
		for _, k := range keys {
			v, _ := m.Get(k)
			sum += v
		}
		for _, v := range m.All() {
			sum += v
		}
		m.Delete("ccc")
		m.Clear()
		m.Drop()
	})
}

var mapSink int

// BenchmarkMapAgainstGoMap times 1,024 sets and gets in a Map and in a Go map.
func BenchmarkMapAgainstGoMap(b *testing.B) {
	keys := make([]uint32, 1024)
	for i := range keys {
		keys[i] = uint32(i) * 2654435761 >> 7
	}
	b.Run("bounded set+get", func(b *testing.B) {
		var m Map[uint32, int]
		m.Reset(1024)
		for b.Loop() {
			m.Clear()
			for i, k := range keys {
				m.Set(k, i)
			}
			s := 0
			for _, k := range keys {
				v, _ := m.Get(k)
				s += v
			}
			mapSink = s
		}
	})
	b.Run("go map set+get", func(b *testing.B) {
		m := make(map[uint32]int, 1024)
		for b.Loop() {
			clear(m)
			for i, k := range keys {
				m[k] = i
			}
			s := 0
			for _, k := range keys {
				s += m[k]
			}
			mapSink = s
		}
	})
}

// matchesGoMap runs the same random sets, gets, deletes, and clears on a Map
// and a Go map, keyed by key(i) for i below 300, and fails where they differ.
func matchesGoMap[K comparable](t *testing.T, key func(i int) K) {
	t.Helper()
	rng := rand.New(rand.NewPCG(3, 4))
	var m Map[K, int]
	m.Reset(256)
	want := map[K]int{}
	for step := range 20000 {
		k := key(rng.IntN(300))
		switch rng.IntN(8) {
		case 0, 1, 2:
			if _, ok := want[k]; ok || len(want) < m.Limit() {
				want[k] = step
				if !m.Set(k, step) {
					t.Fatalf("step %d: Set(%v) did not fit with %d of %d entries", step, k, m.Len(), m.Limit())
				}
			}
		case 3, 4:
			delete(want, k)
			m.Delete(k)
		case 5:
			if rng.IntN(100) == 0 {
				clear(want)
				m.Clear()
			}
		default:
			v, ok := m.Get(k)
			if w, wok := want[k]; v != w || ok != wok {
				t.Fatalf("step %d: Get(%v) = %d, %v; want %d, %v", step, k, v, ok, w, wok)
			}
		}
		if m.Len() != len(want) {
			t.Fatalf("step %d: %d entries, want %d", step, m.Len(), len(want))
		}
	}
}

// Every way a key is hashed, the integer fast paths and maphash for the rest,
// sets, gets, deletes, and clears as a Go map does.
func TestEachKeyTypeMatchesAGoMap(t *testing.T) {
	t.Run("uint32", func(t *testing.T) { matchesGoMap(t, func(i int) uint32 { return uint32(i) << 20 }) })
	t.Run("uint64", func(t *testing.T) { matchesGoMap(t, func(i int) uint64 { return uint64(i) << 40 }) })
	t.Run("int", func(t *testing.T) { matchesGoMap(t, func(i int) int { return -i * 1024 }) })
	t.Run("int64", func(t *testing.T) { matchesGoMap(t, func(i int) int64 { return int64(i) * 7 }) })
	t.Run("string", func(t *testing.T) { matchesGoMap(t, func(i int) string { return strconv.Itoa(i) }) })
	type pair struct{ a, b int32 }
	t.Run("struct", func(t *testing.T) { matchesGoMap(t, func(i int) pair { return pair{int32(i), -int32(i)} }) })
}

// longestProbe is the farthest any entry of m sits from the slot its key
// hashes to.
func longestProbe[K comparable, V any](m *Map[K, V]) int {
	mask := len(m.slots) - 1
	longest := 0
	for i := range m.slots {
		if s := &m.slots[i]; s.gen == m.gen {
			longest = max(longest, (i-m.home(s.key))&mask)
		}
	}
	return longest
}

// Integer keys that differ only in their high bits, or only by a stride of a
// power of two, still spread across the slots: a hash that kept their low
// bits would put them all on one probe.
func TestIntegerKeysSpreadAcrossSlots(t *testing.T) {
	const n = 512
	spread := func(t *testing.T, name string, longest int) {
		t.Helper()
		if longest > 64 {
			t.Errorf("%s: an entry sits %d slots from its home, want at most 64", name, longest)
		}
	}
	shapes := map[string]func(i int) uint64{
		"sequential":   func(i int) uint64 { return uint64(i) },
		"stride 1<<10": func(i int) uint64 { return uint64(i) << 10 },
		"stride 1<<20": func(i int) uint64 { return uint64(i) << 20 },
		"stride 1<<32": func(i int) uint64 { return uint64(i) << 32 },
		"high bits":    func(i int) uint64 { return uint64(i) << 54 },
		"negative":     func(i int) uint64 { return uint64(-i - 1) },
	}
	for name, key := range shapes {
		var m64 Map[uint64, int]
		var mi Map[int, int]
		var mi64 Map[int64, int]
		m64.Reset(n)
		mi.Reset(n)
		mi64.Reset(n)
		for i := range n {
			m64.Set(key(i), i)
			mi.Set(int(key(i)), i)
			mi64.Set(int64(key(i)), i)
		}
		spread(t, "uint64 "+name, longestProbe(&m64))
		spread(t, "int "+name, longestProbe(&mi))
		spread(t, "int64 "+name, longestProbe(&mi64))
		if key(n-1) <= math.MaxUint32 || name == "negative" {
			var m32 Map[uint32, int]
			m32.Reset(n)
			for i := range n {
				m32.Set(uint32(key(i)), i)
			}
			if m32.Len() == n {
				spread(t, "uint32 "+name, longestProbe(&m32))
			}
		}
	}
}
