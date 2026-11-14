package bounded

import (
	"maps"
	"slices"
	"testing"
	"unsafe"
)

// The fuzz tests below drive each container through operations read from
// the fuzzer's bytes and compare it, after every one, with a model built
// from Go's own maps and slices.

// mapKeys is how many keys a fuzzed Map sees: few enough that they collide
// in its slots and a deletion has entries after it to move back.
const mapKeys = 24

// FuzzMapMatchesAGoMap sets, gets, deletes, clears, and drops keys of a Map,
// fixed in size or growing through classes, and checks it against a Go map
// held to the same limit: each Set fits exactly when the model says, and
// Len, Full, every key's Get, and All agree.
func FuzzMapMatchesAGoMap(f *testing.F) {
	f.Add(uint8(4), false, []byte("\x00\x01\x00\x02\x00\x03\x02\x01\x01\x02\x00\x09\x00\x11"))
	f.Add(uint8(2), true, []byte("\x00\x01\x00\x02\x00\x03\x00\x04\x00\x05\x02\x03\x03\x00\x06"))
	f.Add(uint8(16), false, []byte("\x00\x00\x00\x08\x00\x10\x02\x00\x01\x08\x04\x00\x01"))
	f.Fuzz(func(t *testing.T, size uint8, grow bool, data []byte) {
		var m Map[uint8, uint16]
		limit := int(size % 17)
		m.Reset(limit)
		var r Region
		if grow {
			// Classes of 8, 16, and 64 entries: past the last, a Set of a
			// new key does not fit.
			r.Reset(1 << 20)
			defer r.Release()
			m.Grow(&r, NewMapClasses(NewMapPool[uint8, uint16](8, 2), NewMapPool[uint8, uint16](16, 2),
				NewMapPool[uint8, uint16](64, 2)))
			limit = max(limit, 64)
		}
		model, full := map[uint8]uint16{}, false
		for step := 0; len(data) >= 2; step++ {
			op, k := data[0]%5, data[1]%mapKeys
			data = data[2:]
			switch op {
			case 0:
				v := uint16(step)
				_, had := model[k]
				fits := had || len(model) < limit
				if got := m.Set(k, v); got != fits {
					t.Fatalf("step %d: Set(%d) fit %v, model %v (holding %d of %d)", step, k, got, fits, len(model), limit)
				}
				if fits {
					model[k] = v
				} else {
					full = true
				}
			case 1:
				got, ok := m.Get(k)
				want, wantOK := model[k]
				if got != want || ok != wantOK {
					t.Fatalf("step %d: Get(%d) = %d, %v; model %d, %v", step, k, got, ok, want, wantOK)
				}
			case 2:
				m.Delete(k)
				delete(model, k)
			case 3:
				m.Clear()
				clear(model)
				full = false
			case 4:
				m.Drop()
				clear(model)
				full = false
			}
			if m.Len() != len(model) || m.Full() != full {
				t.Fatalf("step %d: Len %d Full %v, model %d %v", step, m.Len(), m.Full(), len(model), full)
			}
			for k := range uint8(mapKeys) {
				got, ok := m.Get(k)
				want, wantOK := model[k]
				if got != want || ok != wantOK {
					t.Fatalf("step %d: key %d holds %d, %v; model %d, %v", step, k, got, ok, want, wantOK)
				}
			}
			if all := maps.Collect(m.All()); !maps.Equal(all, model) {
				t.Fatalf("step %d: All yields %v, model %v", step, all, model)
			}
		}
	})
}

// listOps applies the fuzzer's operations to a list through push, extend,
// and truncate, and to a model slice of at most capacity items, calling
// check after each.
func listOps(t *testing.T, data []byte, capacity int, push func(uint16) bool, extend func([]uint16) bool,
	truncate func(int), check func(step int, model []uint16, full bool),
) {
	t.Helper()
	var model []uint16
	full := false
	for step := 0; len(data) >= 2; step++ {
		op, n := data[0]%4, int(data[1])
		data = data[2:]
		switch op {
		case 0:
			fits := len(model) < capacity
			if got := push(uint16(step)); got != fits {
				t.Fatalf("step %d: Push fit %v, model %v (holding %d of %d)", step, got, fits, len(model), capacity)
			}
			if fits {
				model = append(model, uint16(step))
			} else {
				full = true
			}
		case 1:
			xs := make([]uint16, n%40)
			for i := range xs {
				xs[i] = uint16(step<<6 | i)
			}
			fits := len(model)+len(xs) <= capacity
			if got := extend(xs); got != fits {
				t.Fatalf("step %d: Extend of %d fit %v, model %v (holding %d of %d)", step, len(xs), got, fits, len(model), capacity)
			}
			if fits {
				model = append(model, xs...)
			} else {
				full = true
			}
		case 2:
			k := 0
			if len(model) > 0 {
				k = n % (len(model) + 1)
			}
			truncate(k)
			model, full = model[:k], false
		case 3:
			truncate(0)
			model, full = model[:0], false
		}
		check(step, model, full)
	}
}

// FuzzVecMatchesASlice pushes, extends, and truncates a Vec, in memory it is
// given or in chunks borrowed from a Region, and checks it against a slice
// of the same capacity: what fits, Len, Full, At, All, Chunks, and Items.
func FuzzVecMatchesASlice(f *testing.F) {
	f.Add(uint8(8), false, []byte("\x00\x00\x00\x00\x01\x05\x02\x02\x01\x09\x00\x00\x03\x00"))
	f.Add(uint8(0), true, []byte("\x01\x07\x01\x07\x00\x00\x02\x05\x01\x20\x00\x00"))
	f.Fuzz(func(t *testing.T, size uint8, chunked bool, data []byte) {
		var v Vec[uint16]
		capacity := int(size % 33)
		var r Region
		if chunked {
			// 8 chunks of 4: a Vec in chunks holds 32 items.
			r.Reset(1 << 20)
			defer r.Release()
			v.ResetFrom(&r, NewPool[uint16](4, 8))
			capacity = 32
		} else {
			v.Reset(make([]uint16, 0, capacity))
		}
		listOps(t, data, capacity, v.Push, v.Extend, v.Truncate, func(step int, model []uint16, full bool) {
			if v.Len() != len(model) || v.Full() != full {
				t.Fatalf("step %d: Len %d Full %v, model %d %v", step, v.Len(), v.Full(), len(model), full)
			}
			for i, want := range model {
				if got := v.At(i); got != want {
					t.Fatalf("step %d: At(%d) = %d, model %d", step, i, got, want)
				}
			}
			var all, chunks []uint16
			for i, x := range v.All() {
				if i != len(all) {
					t.Fatalf("step %d: All yields index %d after %d items", step, i, len(all))
				}
				all = append(all, x)
			}
			for c := range v.Chunks() {
				chunks = append(chunks, c...)
			}
			if !slices.Equal(all, model) || !slices.Equal(chunks, model) {
				t.Fatalf("step %d: All %v, Chunks %v, model %v", step, all, chunks, model)
			}
			if !chunked && !slices.Equal(v.Items(), model) {
				t.Fatalf("step %d: Items %v, model %v", step, v.Items(), model)
			}
		})
	})
}

// FuzzSliceMatchesASlice pushes, extends, and truncates a Slice, in memory
// it is given and moving up through classes once it fills, and checks it
// against a slice of the same capacity: what fits, Len, Full, and Items.
func FuzzSliceMatchesASlice(f *testing.F) {
	f.Add(uint8(4), true, []byte("\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x0f\x02\x03\x01\x27"))
	f.Add(uint8(9), false, []byte("\x01\x09\x00\x00\x03\x00\x01\x04"))
	f.Fuzz(func(t *testing.T, size uint8, grow bool, data []byte) {
		var s Slice[uint16]
		capacity := int(size % 9)
		s.Reset(make([]uint16, 0, capacity))
		var r Region
		if grow {
			// Classes of 4, 16, and 64: past the last, nothing more fits.
			r.Reset(1 << 20)
			defer r.Release()
			s.Grow(&r, NewClasses(NewPool[uint16](4, 2), NewPool[uint16](16, 2), NewPool[uint16](64, 2)))
			capacity = 64
		}
		listOps(t, data, capacity, s.Push, s.Extend, s.Truncate, func(step int, model []uint16, full bool) {
			if s.Len() != len(model) || s.Full() != full || !slices.Equal(s.Items(), model) {
				t.Fatalf("step %d: Items %v Full %v, model %v %v", step, s.Items(), s.Full(), model, full)
			}
		})
	})
}

// FuzzRegionsShareAPool takes, gives back, reserves, and releases chunks of
// one Pool for two Regions, and checks each step against a model of what
// each holds: a Take fails exactly when the Region's budget or the Pool has
// no room, Held and Free match, and no chunk is ever lent to two holders.
func FuzzRegionsShareAPool(f *testing.F) {
	f.Add(uint8(3), uint8(8), []byte("\x00\x00\x00\x01\x01\x00\x02\x01\x03\x00\x00\x00\x04\x01"))
	f.Add(uint8(6), uint8(2), []byte("\x02\x00\x02\x01\x00\x00\x00\x00\x03\x01\x00\x01"))
	f.Fuzz(func(t *testing.T, count, budget uint8, data []byte) {
		const chunkLen = 4
		chunks := int(count%8) + 1
		p := NewPool[uint64](chunkLen, chunks)
		chunkBytes := p.ChunkBytes()
		var regions [2]Region
		// held is each region's chunks, the first spare of them Reserve
		// borrowed that no Take has handed out.
		type model struct {
			held   [][]uint64
			spares int
			budget int
		}
		var ms [2]model
		reset := func(i int, chunksOf int) {
			regions[i].Reset(chunksOf * chunkBytes)
			ms[i] = model{budget: chunksOf * chunkBytes}
		}
		reset(0, int(budget%10))
		reset(1, int(budget/10%10))
		defer regions[0].Release()
		defer regions[1].Release()
		free := chunks
		for step := 0; len(data) >= 2; step++ {
			op, i, n := data[0]%5, int(data[1]&1), int(data[1]>>1)
			data = data[2:]
			r, m := &regions[i], &ms[i]
			switch op {
			case 0:
				got := r.Take(p)
				switch {
				case m.spares > 0:
					if got == nil {
						t.Fatalf("step %d: Take with a reserved chunk failed", step)
					}
					m.spares--
					m.held[m.spares] = got
				case (len(m.held)+1)*chunkBytes > m.budget || free == 0:
					if got != nil {
						t.Fatalf("step %d: Take past the budget or an empty pool succeeded", step)
					}
					continue
				default:
					if got == nil {
						t.Fatalf("step %d: Take with room failed (held %d of %d bytes, %d free)", step,
							len(m.held)*chunkBytes, m.budget, free)
					}
					m.held = append(m.held, got)
					free--
				}
			case 1:
				// A chunk handed out, not a reserved one.
				if len(m.held) == m.spares {
					continue
				}
				k := m.spares + n%(len(m.held)-m.spares)
				r.GiveBack(p, m.held[k])
				m.held = slices.Delete(m.held, k, k+1)
				free++
			case 2:
				k := n%3 + 1
				done := make(chan struct{})
				close(done)
				fits := (len(m.held)+k)*chunkBytes <= m.budget && k <= free
				if got := r.Reserve(done, p, k); got != fits {
					t.Fatalf("step %d: Reserve(%d) = %v, model %v", step, k, got, fits)
				}
				if fits {
					// The reserved chunks are handed out by the next Takes;
					// the model counts them until then.
					for range k {
						m.held = slices.Insert(m.held, 0, nil)
					}
					m.spares += k
					free -= k
				}
			case 3:
				r.Release()
				free += len(m.held)
				m.held, m.spares = nil, 0
			case 4:
				free += len(m.held)
				reset(i, n%10)
			}
			for j := range regions {
				if got, want := regions[j].Held(), len(ms[j].held)*chunkBytes; got != want {
					t.Fatalf("step %d: region %d holds %d bytes, model %d", step, j, got, want)
				}
			}
			if p.Free() != free {
				t.Fatalf("step %d: pool has %d free, model %d", step, p.Free(), free)
			}
			seen := map[*uint64]bool{}
			for j := range ms {
				for _, c := range ms[j].held {
					if c == nil {
						continue
					}
					at := unsafe.SliceData(c)
					if seen[at] {
						t.Fatalf("step %d: a chunk is lent twice", step)
					}
					seen[at] = true
				}
			}
		}
	})
}
