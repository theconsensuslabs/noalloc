package json

import "unsafe"

// An Arena holds the bytes of the strings a Reader decodes. It writes into
// memory its caller gives it, and when that fills asks More for another
// chunk at least need bytes long, moving the string it is in the middle of.
// A string decoded into an Arena stays valid until the chunk holding it is
// reused, so a caller resets the Arena, and takes back the chunks More gave
// out, only once it is done with what it decoded.
//
// A nil More, or one that returns less than need, fails the decode with Full.
//
//noalloc:fixed
type Arena struct {
	mem   []byte
	start int
	More  func(need int) []byte
}

// Reset makes mem the Arena's memory, emptied.
//
//noalloc:root
func (a *Arena) Reset(mem []byte) { a.mem, a.start = mem[:0], 0 }

// Len is how many bytes the Arena's current chunk holds.
//
//noalloc:root
func (a *Arena) Len() int { return len(a.mem) }

func (a *Arena) begin() { a.start = len(a.mem) }

func (a *Arena) write(p []byte) bool {
	n := len(a.mem)
	if n+len(p) > cap(a.mem) {
		if !a.grow(len(p)) {
			return false
		}
		n = len(a.mem)
	}
	a.mem = a.mem[:n+len(p)]
	copy(a.mem[n:], p)
	return true
}

// grow moves the string being written to a chunk from More with room for n
// more bytes.
func (a *Arena) grow(n int) bool {
	part := a.mem[a.start:]
	need := len(part) + n
	if a.More == nil {
		return false
	}
	//noalloc:exempt response: the caller's function, which hands over memory it chose.
	m := a.More(max(need, 2*len(part)))
	if cap(m) < need {
		return false
	}
	m = m[:len(part)]
	copy(m, part)
	a.mem, a.start = m, 0
	return true
}

// finish is the string written since begin.
func (a *Arena) finish() string {
	n := len(a.mem) - a.start
	if n == 0 {
		return ""
	}
	return unsafe.String(&a.mem[a.start], n)
}
