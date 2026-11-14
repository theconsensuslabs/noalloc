package bounded

// Classes are Pools of chunks of increasing length. A container that fills
// its chunk moves to a chunk of the next class it can borrow, copying what it
// holds, and gives the old one back, as append moves a slice to a larger
// array. Each class is a fixed pool, so the moves are bounded by the classes,
// and the copies add up to less than the final chunk when each class at
// least doubles the last.
type Classes[T any] struct {
	pools []*Pool[T]
}

// NewClasses makes Classes of pools, whose chunk lengths must increase.
func NewClasses[T any](pools ...*Pool[T]) *Classes[T] {
	if len(pools) == 0 {
		panic("bounded: classes need at least one pool")
	}
	for i := 1; i < len(pools); i++ {
		if pools[i].ChunkLen() <= pools[i-1].ChunkLen() {
			panic("bounded: classes' chunk lengths must increase")
		}
	}
	//noalloc:exempt setup: the list of a set of classes' pools, made once.
	return &Classes[T]{pools: pools}
}

// Pools is each class's pool, smallest first, for their counters.
//
//noalloc:root
func (c *Classes[T]) Pools() []*Pool[T] { return c.pools }

// grow borrows for r a chunk of the smallest class longer than have that
// holds at least need, trying larger classes when one has none free, and
// returns it with its pool.
func (c *Classes[T]) grow(r *Region, have, need int) ([]T, *Pool[T]) {
	for _, p := range c.pools {
		if l := p.ChunkLen(); l <= have || l < need {
			continue
		}
		if mem := r.Take(p); mem != nil {
			return mem, p
		}
	}
	return nil, nil
}

// A Slice is a contiguous list in memory it is given or borrows from
// Classes. When it fills it moves to a chunk of a larger class, as append
// would, until no class has room; then a Push adds nothing and leaves it full
// until it is truncated or reset.
//
//noalloc:fixed
type Slice[T any] struct {
	items []T
	full  bool
	// r and c are where it borrows its next chunk, and p the pool its
	// current chunk is from, nil while it is in memory it was given. gen is
	// r's releases when it borrowed that chunk.
	r   *Region
	c   *Classes[T]
	p   *Pool[T]
	gen uint64
}

// live panics when the Region the Slice's chunk is from was released since
// it borrowed the chunk.
func (s *Slice[T]) live() {
	if s.p != nil && s.r.gen != s.gen {
		panic("bounded: a Slice used after its region was released")
	}
}

// Reset makes mem the Slice's memory, emptied. It moves to a borrowed chunk
// only after Grow.
//
//noalloc:root
func (s *Slice[T]) Reset(mem []T) {
	//noalloc:exempt setup: the memory the Slice is given, which is its own from here on.
	*s = Slice[T]{items: mem[:0]}
}

// ResetFrom empties the Slice, which borrows its first chunk and each larger
// one from c for r. What it holds is valid until r is released.
//
//noalloc:root
func (s *Slice[T]) ResetFrom(r *Region, c *Classes[T]) {
	*s = Slice[T]{r: r, c: c}
}

// Grow lets a Slice in memory it was given move to chunks borrowed from c for
// r once it fills. The memory it was given is left as it is.
//
//noalloc:root
func (s *Slice[T]) Grow(r *Region, c *Classes[T]) { s.r, s.c = r, c }

// room makes room for n more items, moving to a larger chunk when it must,
// and reports whether there is room.
func (s *Slice[T]) room(n int) bool {
	if len(s.items)+n <= cap(s.items) {
		return true
	}
	if s.full || s.c == nil {
		s.full = true
		return false
	}
	mem, p := s.c.grow(s.r, cap(s.items), len(s.items)+n)
	if mem == nil {
		s.full = true
		return false
	}
	k := copy(mem, s.items)
	if s.p != nil {
		s.r.GiveBack(s.p, s.items[:cap(s.items)])
	}
	s.items, s.p, s.gen = mem[:k], p, s.r.gen
	return true
}

// Push adds x, and reports whether it fit.
//
//noalloc:root
func (s *Slice[T]) Push(x T) bool {
	s.live()
	if !s.room(1) {
		return false
	}
	s.items = s.items[:len(s.items)+1]
	s.items[len(s.items)-1] = x
	return true
}

// Extend adds every item of xs, and reports whether they fit. When they do
// not it adds none, and the Slice is full.
//
//noalloc:root
func (s *Slice[T]) Extend(xs []T) bool {
	s.live()
	if !s.room(len(xs)) {
		return false
	}
	n := len(s.items)
	s.items = s.items[:n+len(xs)]
	copy(s.items[n:], xs)
	return true
}

// Items is what the Slice holds, valid until the next Push, Extend, Truncate,
// or Reset, since a Push may move it.
//
//noalloc:root
func (s *Slice[T]) Items() []T {
	s.live()
	return s.items
}

// Len is how many items the Slice holds.
//
//noalloc:root
func (s *Slice[T]) Len() int { return len(s.items) }

// Cap is how many items it holds room for before it next moves.
//
//noalloc:root
func (s *Slice[T]) Cap() int { return cap(s.items) }

// Full reports whether a Push did not fit since the Slice was last reset,
// truncated, or cleared.
//
//noalloc:root
func (s *Slice[T]) Full() bool { return s.full }

// Truncate drops the items past the first n, which Len returned before, and
// makes room to push again. It keeps the chunk it is in.
//
//noalloc:root
func (s *Slice[T]) Truncate(n int) {
	s.live()
	if n < 0 || n > len(s.items) {
		panic("bounded: truncation out of range")
	}
	clear(s.items[n:])
	s.items, s.full = s.items[:n], false
}

// Clear empties the Slice for the next user of its memory.
//
//noalloc:root
func (s *Slice[T]) Clear() { s.Truncate(0) }
