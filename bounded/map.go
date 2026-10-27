package bounded

import (
	"hash/maphash"
	"iter"
	"math"
)

// A Map holds at most a fixed number of entries in memory set aside once. A
// Set of a new key that does not fit adds nothing and leaves the Map full
// until it is cleared or reset.
//
// It is an open-addressing table, probed in order, kept at most half full so
// a lookup stops soon after it starts. Each slot remembers the generation
// that wrote it, so Clear is constant time, and a deletion moves the entries
// after it back rather than leaving a marker.
//
//noalloc:fixed
type Map[K comparable, V any] struct {
	slots []slot[K, V]
	seed  maphash.Seed
	n     int
	limit int
	gen   uint32
	full  bool
	// r and c are where a Map that fills borrows larger slots, set by Grow,
	// and p the pool its slots are from, nil while they are its own.
	r *Region
	c *MapClasses[K, V]
	p *MapPool[K, V]
}

type slot[K comparable, V any] struct {
	key K
	val V
	// gen is the generation of the Map that wrote the slot; any other
	// generation reads as empty.
	gen uint32
}

// Reset gives the Map room for n entries, emptied.
//
//noalloc:root
func (m *Map[K, V]) Reset(n int) {
	//noalloc:exempt setup: the slots the Map holds its entries in, set aside once.
	*m = Map[K, V]{slots: make([]slot[K, V], slotsFor(n)), seed: maphash.MakeSeed(), limit: n, gen: 1}
}

// A MapPool is the slots of Maps that borrow them through a Region, and
// MapClasses MapPools of increasing size a Map moves up through as it fills.
type (
	MapPool[K comparable, V any]    = Pool[slot[K, V]]
	MapClasses[K comparable, V any] = Classes[slot[K, V]]
)

// NewMapClasses makes MapClasses of pools, which must hold Maps of
// increasing size.
func NewMapClasses[K comparable, V any](pools ...*MapPool[K, V]) *MapClasses[K, V] {
	return NewClasses(pools...)
}

// Grow lets the Map, each time it fills, move to the slots of the next of
// c's classes it can borrow for r, giving back the slots it moved from.
// Slots it was given are left as they are.
//
//noalloc:root
func (m *Map[K, V]) Grow(r *Region, c *MapClasses[K, V]) { m.r, m.c = r, c }

// grow moves the Map's entries to larger slots, as Grow describes, and
// reports whether it did.
func (m *Map[K, V]) grow() bool {
	if m.c == nil {
		return false
	}
	slots, p := m.c.grow(m.r, len(m.slots), slotsFor(m.n+1))
	if slots == nil {
		return false
	}
	clear(slots)
	old, oldP, oldGen := m.slots, m.p, m.gen
	m.slots, m.p, m.limit, m.gen, m.n = slots, p, len(slots)/2, 1, 0
	if m.seed == (maphash.Seed{}) {
		m.seed = maphash.MakeSeed()
	}
	for i := range old {
		if s := &old[i]; s.gen == oldGen {
			j, _ := m.find(s.key)
			m.slots[j] = slot[K, V]{key: s.key, val: s.val, gen: m.gen}
			m.n++
		}
	}
	if oldP != nil {
		GiveBack(m.r, oldP, old)
	}
	return true
}

// NewMapPool sets aside count Maps' slots, each Map holding up to entries.
func NewMapPool[K comparable, V any](entries, count int) *MapPool[K, V] {
	return NewPool[slot[K, V]](slotsFor(entries), count)
}

// slotsFor is how many slots a Map of n entries holds: a power of two at
// least twice n, so it stays at most half full.
func slotsFor(n int) int {
	size := 2
	for size < 2*n {
		size *= 2
	}
	return size
}

// ResetFrom gives the Map a chunk of p's slots, borrowed for r, emptied, and
// reports whether r could borrow one. A Map that could not holds nothing,
// as a Map never reset does. It holds up to the entries p was made for.
//
//noalloc:root
func (m *Map[K, V]) ResetFrom(r *Region, p *MapPool[K, V]) bool {
	slots := Take(r, p)
	clear(slots)
	*m = Map[K, V]{slots: slots, seed: maphash.MakeSeed(), limit: len(slots) / 2, gen: 1}
	if slots != nil {
		m.r, m.p = r, p
	}
	return slots != nil
}

func (m *Map[K, V]) home(k K) int {
	return int(maphash.Comparable(m.seed, k) & uint64(len(m.slots)-1))
}

// find is the slot k is in, or the empty slot where it would go.
func (m *Map[K, V]) find(k K) (int, bool) {
	i := m.home(k)
	for {
		s := &m.slots[i]
		if s.gen != m.gen {
			return i, false
		}
		if s.key == k {
			return i, true
		}
		i = (i + 1) & (len(m.slots) - 1)
	}
}

// Get is the value k holds, and whether it holds one.
//
//noalloc:root
func (m *Map[K, V]) Get(k K) (V, bool) {
	if m.n == 0 {
		var zero V
		return zero, false
	}
	i, ok := m.find(k)
	if !ok {
		var zero V
		return zero, false
	}
	return m.slots[i].val, true
}

// Set makes k hold v, and reports whether it fit: a key the Map holds always
// does, and a new one does while the Map holds fewer entries than it was
// reset for. A Map never reset holds nothing.
//
//noalloc:root
func (m *Map[K, V]) Set(k K, v V) bool {
	if len(m.slots) == 0 && !m.grow() {
		m.full = true
		return false
	}
	i, ok := m.find(k)
	if ok {
		m.slots[i].val = v
		return true
	}
	if m.n >= m.limit {
		if !m.grow() {
			m.full = true
			return false
		}
		i, _ = m.find(k)
	}
	m.slots[i] = slot[K, V]{key: k, val: v, gen: m.gen}
	m.n++
	return true
}

// Delete removes k, if the Map holds it.
//
//noalloc:root
func (m *Map[K, V]) Delete(k K) {
	if m.n == 0 {
		return
	}
	i, ok := m.find(k)
	if !ok {
		return
	}
	mask := len(m.slots) - 1
	// Each entry after i that would be found from a home at or before i moves
	// into i, which then moves on to where it was: Knuth's Algorithm R, The
	// Art of Computer Programming, Vol. 3, 6.4.
	for j := (i + 1) & mask; m.slots[j].gen == m.gen; j = (j + 1) & mask {
		h := m.home(m.slots[j].key)
		if (j-h)&mask >= (j-i)&mask {
			m.slots[i] = m.slots[j]
			i = j
		}
	}
	var zero slot[K, V]
	m.slots[i] = zero
	m.n--
}

// Len is how many entries the Map holds.
//
//noalloc:root
func (m *Map[K, V]) Len() int { return m.n }

// Limit is how many entries the Map was reset for.
//
//noalloc:root
func (m *Map[K, V]) Limit() int { return m.limit }

// Full reports whether a Set of a new key did not fit since the Map was last
// reset or cleared.
//
//noalloc:root
func (m *Map[K, V]) Full() bool { return m.full }

// Clear empties the Map for the next user of its memory.
//
//noalloc:root
func (m *Map[K, V]) Clear() {
	m.n, m.full = 0, false
	if m.gen++; m.gen == math.MaxUint32 {
		// A slot written so many generations ago would read as current.
		clear(m.slots)
		m.gen = 1
	}
}

// Drop empties the Map as Clear does and zeroes its slots, so nothing it
// held stays reachable through it.
//
//noalloc:root
func (m *Map[K, V]) Drop() {
	clear(m.slots)
	m.n, m.full = 0, false
}

// All yields each entry, in no particular order. The Map must not change
// while it does.
//
//noalloc:root
func (m *Map[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for i := range m.slots {
			if s := &m.slots[i]; s.gen == m.gen && !yield(s.key, s.val) {
				return
			}
		}
	}
}
