package bounded

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/theconsensuslabs/noalloc/alloctest"
)

// model is what a Buf without a flush writer holds after each write: the
// bytes of every write that fit, and whether one did not.
type model struct {
	budget int
	b      []byte
	full   bool
}

func (m *model) write(p []byte) {
	if m.full || len(m.b)+len(p) > m.budget {
		m.full = true
		return
	}
	m.b = append(m.b, p...)
}

// appendRepeat appends s twice: a function shaped like append whose output is
// twice its input, so a caller can give it a bound that is too small.
func appendRepeat(dst []byte, s string) []byte { return append(append(dst, s...), s...) }

// ops runs the writes a fuzz input spells against b, and against m when it is
// not nil, checking after each that b holds what m does.
func ops(t *testing.T, data []byte, b *Buf, m *model) {
	t.Helper()
	for len(data) >= 2 {
		op, size := data[0]%5, int(data[1])
		data = data[2:]
		size = min(size, len(data))
		arg := string(data[:size])
		data = data[size:]
		var want []byte
		switch op {
		case 0:
			b.WriteString(arg)
			want = []byte(arg)
		case 1:
			b.Write([]byte(arg))
			want = []byte(arg)
		case 2:
			v := int(int64(size)*math.MaxInt64/255) * (1 - 2*(size%2))
			b.WriteInt(v)
			want = strconv.AppendInt(nil, int64(v), 10)
			if m != nil && !m.full && len(m.b)+20 > m.budget {
				// WriteInt asks for room for the longest int.
				m.full = true
				want = nil
			}
		case 3:
			b.AppendString(2*len(arg), appendRepeat, arg)
			want = []byte(arg + arg)
		case 4:
			if m != nil && size <= b.Len() {
				b.Truncate(size)
				m.b, m.full = m.b[:size], false
				continue
			}
		}
		if m == nil {
			continue
		}
		if op != 4 && want != nil {
			m.write(want)
		}
		if !bytes.Equal(b.Bytes(), m.b) || b.Full() != m.full || b.Len() != len(m.b) {
			t.Fatalf("after op %d: buffer holds %q (full %v), model %q (full %v)", op, b.Bytes(), b.Full(), m.b, m.full)
		}
	}
}

func FuzzBufferMatchesModel(f *testing.F) {
	f.Add(uint8(16), []byte("\x00\x05hello\x03\x02hi\x02\x09\x00\x02ab\x04\x01"))
	f.Add(uint8(4), []byte("\x00\x05hello\x00\x03abc"))
	f.Add(uint8(40), []byte("\x02\xff\x02\x00\x02\x01"))
	// A number after other bytes, then more bytes after it.
	f.Add(uint8(64), []byte("\x00\x03abc\x02\x07\x00\x02de"))
	f.Fuzz(func(t *testing.T, budget uint8, data []byte) {
		mem := make([]byte, int(budget), int(budget)+8)
		past := mem[len(mem):cap(mem)]
		copy(past, "SENTINEL")
		var b Buf
		b.Reset(mem, nil)
		ops(t, data, &b, &model{budget: int(budget)})
		if string(past) != "SENTINEL" {
			t.Fatalf("a write reached past the budget: %q", past)
		}
	})
}

// FuzzFlushedOutputIsEveryWrite checks that a buffer with a flush writer
// writes out exactly what was written, in order, whatever its size, as long
// as no single bounded write needs more than the whole buffer.
func FuzzFlushedOutputIsEveryWrite(f *testing.F) {
	f.Add(uint8(8), []byte("\x00\x05hello\x00\x0bhello world\x02\x07\x03\x02xy"))
	f.Add(uint8(40), []byte("\x00\x03abc\x02\x05\x00\x02de"))
	f.Fuzz(func(t *testing.T, budget uint8, data []byte) {
		size := int(budget) + 20
		var out, want bytes.Buffer
		var b Buf
		b.Reset(make([]byte, size), &out)
		for len(data) >= 2 {
			op, n := data[0]%4, min(int(data[1]), len(data)-2)
			arg := string(data[2 : 2+n])
			data = data[2+n:]
			switch op {
			case 0:
				b.WriteString(arg)
				want.WriteString(arg)
			case 1:
				b.Write([]byte(arg))
				want.WriteString(arg)
			case 2:
				b.WriteInt(-n)
				want.WriteString(strconv.Itoa(-n))
			case 3:
				if 2*n > size {
					continue
				}
				b.AppendString(2*n, appendRepeat, arg)
				want.WriteString(arg + arg)
			}
			if b.Full() {
				t.Fatalf("a flushing buffer of %d bytes went full", size)
			}
		}
		b.Flush()
		if !bytes.Equal(out.Bytes(), want.Bytes()) {
			t.Fatalf("flushed %q, want %q", out.Bytes(), want.Bytes())
		}
	})
}

func TestAppendStringPastItsBoundWritesNothing(t *testing.T) {
	mem := make([]byte, 6, 16)
	copy(mem[6:16], "SENTINEL!!")
	var b Buf
	b.Reset(mem, nil)
	b.WriteString("ab")
	// appendRepeat writes 6 bytes for "abc" where only 4 are left.
	b.AppendString(3, appendRepeat, "abc")
	if !b.Full() || string(b.Bytes()) != "ab" || string(mem[6:16]) != "SENTINEL!!" {
		t.Fatalf("holds %q (full %v), past the budget %q", b.Bytes(), b.Full(), mem[6:16])
	}
	b.Truncate(2)
	b.AppendString(4, appendRepeat, "cd")
	if b.Full() || string(b.Bytes()) != "abcdcd" {
		t.Fatalf("after truncating: %q (full %v)", b.Bytes(), b.Full())
	}
}

type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n -= len(p); w.n < 0 {
		return 0, errors.New("reader went away")
	}
	return len(p), nil
}

func TestFlushErrorStopsWrites(t *testing.T) {
	w := &failingWriter{n: 8}
	var b Buf
	b.Reset(make([]byte, 8), w)
	b.WriteString("12345678")
	b.WriteString("9")
	b.WriteString(strings.Repeat("x", 20))
	b.Flush()
	if b.Err() == nil || b.Err().Error() != "reader went away" {
		t.Fatalf("Err() = %v, want the writer's error", b.Err())
	}
	// The first 8 bytes go out, then "9" fails, and nothing is written after.
	if w.n != -1 {
		t.Fatalf("writer saw %d bytes past its 8, want only the one write that failed", -w.n)
	}
}

func TestTruncatePastLenPanics(t *testing.T) {
	var b Buf
	b.Reset(make([]byte, 4), nil)
	b.WriteString("ab")
	defer func() {
		if recover() == nil {
			t.Fatal("Truncate(3) on 2 bytes did not panic")
		}
	}()
	b.Truncate(3)
}

var comma = []byte(",")

func TestWritesAllocateNothing(t *testing.T) {
	long := strings.Repeat("y", 300)
	var plain, flushing Buf
	mem, flushMem := make([]byte, 256), make([]byte, 64)
	for name, b := range map[string]*Buf{"plain": &plain, "flushing": &flushing} {
		alloctest.Zero(t, name+" writes", func() {
			if b == &plain {
				b.Reset(mem, nil)
			} else {
				b.Reset(flushMem, io.Discard)
			}
			b.WriteString("{\"n\":")
			b.WriteInt(math.MinInt)
			b.Write(comma)
			b.AppendString(10, appendRepeat, "abcde")
			b.WriteString(long)
			b.Truncate(b.Len())
			b.Flush()
		})
	}
}

// Pinned bytes are never reused: truncating into them or flushing them
// panics, a write that would flush them fills the buffer instead, and Clear
// ends the pin.
func TestPinnedBytesAreNeverReused(t *testing.T) {
	panics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	var b Buf
	b.Reset(make([]byte, 16), nil)
	b.WriteString("row one|")
	b.Pin(b.Len())
	b.WriteString("row two")
	panics("truncating into pinned bytes", func() { b.Truncate(3) })
	b.Truncate(8)
	if got := string(b.Bytes()); got != "row one|" {
		t.Fatalf("after truncating to the pin the buffer holds %q", got)
	}

	var out bytes.Buffer
	var f Buf
	f.Reset(make([]byte, 8), &out)
	f.WriteString("pinned")
	f.Pin(f.Len())
	f.WriteString("past the room")
	if !f.Full() || out.Len() != 0 || string(f.Bytes()) != "pinned" {
		t.Errorf("a write past the room flushed pinned bytes: wrote %q, holds %q (full %v)", out.Bytes(), f.Bytes(), f.Full())
	}
	panics("flushing pinned bytes", f.Flush)
	f.Clear()
	f.WriteString("again")
	f.Flush()
	if out.String() != "again" {
		t.Errorf("after Clear the buffer flushed %q, want %q", out.String(), "again")
	}
	panics("pinning past what the buffer holds", func() { b.Pin(b.Len() + 1) })
}

func TestWriteIntPrintsAsStrconvDoes(t *testing.T) {
	values := []int{0, 1, -1, 9, 10, -10, 99, 100, math.MaxInt, math.MinInt, math.MaxInt32, math.MinInt32}
	for x := uint64(1); x < 1<<63; x = x*7 + 3 {
		values = append(values, int(x), -int(x))
	}
	var b Buf
	mem := make([]byte, 64)
	for _, v := range values {
		b.Reset(mem, nil)
		b.WriteInt(v)
		if got, want := string(b.Bytes()), strconv.Itoa(v); got != want {
			t.Errorf("WriteInt(%d) wrote %q, want %q", v, got, want)
		}
	}
}

func TestWriteByteFillsThenFlushes(t *testing.T) {
	var b Buf
	b.Reset(make([]byte, 3), nil)
	for _, c := range []byte("abcd") {
		b.WriteByte(c)
	}
	if got := string(b.Bytes()); got != "abc" || !b.Full() {
		t.Errorf("without a flush writer: %q, full %v; want \"abc\", full", got, b.Full())
	}
	var out bytes.Buffer
	b.Reset(make([]byte, 3), &out)
	for _, c := range []byte("abcdefg") {
		b.WriteByte(c)
	}
	b.Flush()
	if out.String() != "abcdefg" || b.Full() {
		t.Errorf("with a flush writer: wrote %q, full %v; want \"abcdefg\", not full", out.String(), b.Full())
	}
}

func TestWriteUintPrintsAsStrconvDoes(t *testing.T) {
	var b Buf
	mem := make([]byte, 64)
	for _, v := range []uint64{0, 1, 9, 10, math.MaxUint32, math.MaxUint64, 1 << 63} {
		b.Reset(mem, nil)
		b.WriteUint(v)
		if got, want := string(b.Bytes()), strconv.FormatUint(v, 10); got != want {
			t.Errorf("WriteUint(%d) wrote %q, want %q", v, got, want)
		}
	}
}

func TestAvailIsWrittenInPlaceAndCommitted(t *testing.T) {
	var b Buf
	b.Reset(make([]byte, 8), nil)
	b.WriteString("ab")
	dst := b.Avail()
	if len(dst) != 0 || cap(dst) != 6 {
		t.Fatalf("Avail has len %d cap %d, want 0 and 6", len(dst), cap(dst))
	}
	dst = strconv.AppendInt(dst, 1234, 10)
	b.Commit(len(dst))
	if got := string(b.Bytes()); got != "ab1234" {
		t.Errorf("after Commit: %q, want \"ab1234\"", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("Commit past the buffer did not panic")
		}
	}()
	b.Commit(3)
}

// Free is the room left before the buffer fills, or before it flushes.
func TestFreeIsTheRoomLeft(t *testing.T) {
	var b Buf
	b.Reset(make([]byte, 8), nil)
	b.WriteString("abc")
	if b.Free() != 5 {
		t.Errorf("Free after 3 of 8 bytes is %d, want 5", b.Free())
	}
	b.WriteString("defgh")
	if b.Free() != 0 || b.Full() {
		t.Errorf("Free when exactly full is %d, full %v", b.Free(), b.Full())
	}
}
