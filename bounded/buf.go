// Package bounded holds containers whose memory is set once and never grows:
// a Buf of bytes, a Vec of any type, and a Map. A write that does not fit
// writes nothing and leaves the container full, so a caller checks once, after
// the writes, rather than before each.
//
// Their memory is either given to them or borrowed through a Region, which is
// what one page may take from Pools that are set aside once and shared. A
// page borrows only what its input needs, up to the Region's budget, and
// gives it all back when it ends.
package bounded

import (
	"io"
	"unsafe"
)

// A Buf is a fixed amount of memory that writes fill in place. A write
// that does not fit writes nothing and leaves the buffer full until it is
// truncated or reset. With a flush writer, a write that does not fit first
// writes out what the buffer holds and starts it over.
//
// Len and Truncate count from the last flush, so they serve a buffer without
// a flush writer, whose bytes stay where they were written.
//
//noalloc:fixed
type Buf struct {
	mem   []byte
	n     int
	full  bool
	flush io.Writer
	err   error
	// pin is how many bytes from the start are lent out, which no write may
	// reuse until Clear.
	pin int
	// r and c are where a buffer that fills borrows a larger chunk, set by
	// Borrow, and p the pool its memory is from, nil while it is memory it
	// was given.
	r *Region
	c *Classes[byte]
	p *Pool[byte]
}

// Reset makes mem the buffer's memory, emptied, writing out through flush
// when it is not nil. len(mem) is the budget; capacity past it is not used.
//
//noalloc:root
func (b *Buf) Reset(mem []byte, flush io.Writer) {
	//noalloc:exempt setup: the memory the buffer is given, which is its own from here on.
	*b = Buf{mem: mem[:len(mem):len(mem)], flush: flush}
}

// ResetFrom makes a chunk of p, borrowed for r, the buffer's memory, as Reset
// does, and reports whether r could borrow one. When it could not, the buffer
// has no memory and is full. A buffer is one chunk: it does not grow.
//
//noalloc:root
func (b *Buf) ResetFrom(r *Region, p *Pool[byte], flush io.Writer) bool {
	mem := Take(r, p)
	b.Reset(mem, flush)
	b.full = mem == nil
	if mem != nil {
		b.r, b.p = r, p
	}
	return mem != nil
}

// Borrow lets the buffer, each time it fills, move to a chunk of the next
// of c's classes it can borrow for r: what it holds is copied over and it
// goes on there, giving back the chunk it moved from. Memory it was given is
// left as it is, and so is a chunk with pinned bytes, until r is released.
// Once r cannot borrow it stays where it is.
//
//noalloc:root
func (b *Buf) Borrow(r *Region, c *Classes[byte]) { b.r, b.c = r, c }

// move moves the buffer to a borrowed chunk with room for k more bytes, as
// Borrow describes, and reports whether it did.
func (b *Buf) move(k int) bool {
	if b.c == nil {
		return false
	}
	mem, p := b.c.grow(b.r, len(b.mem), b.n+k)
	if mem == nil {
		b.c = nil
		return false
	}
	copy(mem, b.mem[:b.n])
	if b.p != nil && b.pin == 0 {
		GiveBack(b.r, b.p, b.mem)
	}
	b.mem, b.p = mem, p
	return true
}

// Len is how many bytes the buffer holds.
//
//noalloc:root
func (b *Buf) Len() int { return b.n }

// Bytes is what the buffer holds. It stays valid until the next write.
//
//noalloc:root
func (b *Buf) Bytes() []byte { return b.mem[:b.n] }

// Size is the buffer's budget.
//
//noalloc:root
func (b *Buf) Size() int { return len(b.mem) }

// Free is how many more bytes fit before the buffer is full or flushes.
//
//noalloc:root
func (b *Buf) Free() int { return len(b.mem) - b.n }

// Full reports whether a write did not fit since the buffer was last reset or
// truncated.
//
//noalloc:root
func (b *Buf) Full() bool { return b.full }

// Truncate drops what was written past n, which Len returned before, and
// makes room to write again.
//
//noalloc:root
func (b *Buf) Truncate(n int) {
	if n < 0 || n > b.n {
		panic("bounded: truncation out of range")
	}
	if n < b.pin {
		panic("bounded: truncation into pinned bytes")
	}
	b.n, b.full = n, false
}

// Pin lends out the first n bytes, which Len returned before: until Clear,
// Truncate below n panics, and so does a flush, which would reuse them. A
// write that would flush them leaves the buffer full instead.
//
//noalloc:root
func (b *Buf) Pin(n int) {
	if n < 0 || n > b.n {
		panic("bounded: pin out of range")
	}
	b.pin = max(b.pin, n)
}

// Clear empties the buffer, ends every pin, and forgets a flush error, for
// memory that goes back to whoever writes into it next.
//
//noalloc:root
func (b *Buf) Clear() {
	b.n, b.full, b.pin, b.err = 0, false, 0, nil
}

// Err is the first error the flush writer returned. Writes after it are
// dropped.
//
//noalloc:root
func (b *Buf) Err() error { return b.err }

// Room reports whether k more bytes fit, flushing first to make room when it
// can. It sets the buffer full when they do not.
//
//noalloc:root
func (b *Buf) Room(k int) bool {
	if b.full {
		return false
	}
	if b.n+k <= len(b.mem) {
		return true
	}
	if b.move(k) && b.n+k <= len(b.mem) {
		return true
	}
	if b.flush != nil && b.n > 0 && b.pin == 0 {
		b.Flush()
	}
	b.full = b.n+k > len(b.mem)
	return !b.full
}

// WriteString writes s. With a flush writer, s longer than the whole buffer
// goes straight to it, so s escapes as it does through a bufio.Writer.
//
//noalloc:root
func (b *Buf) WriteString(s string) {
	if b.n+len(s) > len(b.mem) && !b.full {
		b.move(len(s))
	}
	if b.flush != nil && !b.full && b.pin == 0 && b.n+len(s) > len(b.mem) {
		b.Flush()
		if len(s) > len(b.mem) {
			b.send(unsafe.Slice(unsafe.StringData(s), len(s)))
			return
		}
	}
	if b.Room(len(s)) {
		b.n += copy(b.mem[b.n:], s)
	}
}

// Write writes p, as WriteString writes a string.
//
//noalloc:root
func (b *Buf) Write(p []byte) {
	b.WriteString(unsafe.String(unsafe.SliceData(p), len(p)))
}

// WriteByte writes c, and returns the flush writer's error, as Err does.
//
//noalloc:root
func (b *Buf) WriteByte(c byte) error {
	if b.Room(1) {
		b.mem[b.n] = c
		b.n++
	}
	return b.err
}

// WriteInt writes v in decimal.
//
//noalloc:root
func (b *Buf) WriteInt(v int) {
	u := uint64(v)
	if v < 0 {
		u = -u
	}
	b.writeDigits(u, v < 0)
}

// WriteUint writes v in decimal.
//
//noalloc:root
func (b *Buf) WriteUint(v uint64) { b.writeDigits(v, false) }

// writeDigits writes u in decimal, after a minus sign when neg. It needs room
// for the longest a 64-bit integer prints, "-9223372036854775808" being 20
// bytes, whatever u is, so how full a buffer gets does not depend on the value.
func (b *Buf) writeDigits(u uint64, neg bool) {
	var digits [20]byte
	i := len(digits)
	for {
		i--
		digits[i] = byte('0' + u%10)
		if u /= 10; u == 0 {
			break
		}
	}
	if neg {
		i--
		digits[i] = '-'
	}
	if b.Room(len(digits)) {
		b.n += copy(b.mem[b.n:], digits[i:])
	}
}

// Avail is an empty slice whose capacity is the memory past what the buffer
// holds, for a writer that appends to it or reslices it up to its capacity.
// Commit counts what it wrote. An append past the capacity lands in new
// memory rather than the buffer.
//
//noalloc:root
func (b *Buf) Avail() []byte { return b.mem[b.n:b.n:len(b.mem)] }

// Commit counts the first n bytes past what the buffer holds, written through
// Avail, as written.
//
//noalloc:root
func (b *Buf) Commit(n int) {
	if n < 0 || b.n+n > len(b.mem) {
		panic("bounded: commit past the buffer")
	}
	b.n += n
}

// AppendString writes what fn appends for s, when k bytes fit. fn must be
// shaped like append and append at most k bytes; when it appends more than
// the room left, the buffer writes nothing and is full.
func (b *Buf) AppendString(k int, fn func(dst []byte, s string) []byte, s string) {
	if !b.Room(k) {
		return
	}
	dst := b.mem[b.n:b.n:len(b.mem)]
	out := fn(dst, s)
	if len(out) > 0 && unsafe.SliceData(out) != unsafe.SliceData(b.mem[b.n:len(b.mem)]) {
		b.full = true
		return
	}
	b.n += len(out)
}

// Flush writes out what the buffer holds and empties it. Without a flush
// writer it does nothing.
//
//noalloc:root
func (b *Buf) Flush() {
	if b.pin > 0 {
		panic("bounded: flushing pinned bytes")
	}
	if b.flush != nil && b.n > 0 {
		b.send(b.mem[:b.n])
		b.n = 0
	}
}

func (b *Buf) send(p []byte) {
	if b.err == nil {
		//noalloc:exempt response: the bytes written to the reader, which may allocate as it consumes them.
		_, b.err = b.flush.Write(p)
	}
}
