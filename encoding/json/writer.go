package json

import (
	"encoding/base64"
	"io"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/theconsensuslabs/noalloc/bounded"
)

// A Writer writes JSON into memory its caller gives it, for the WriteJSON
// methods jsongen generates. With a flush writer it writes out what it holds
// each time the memory fills, so an encoding of any length fits; without one,
// an encoding longer than the memory fails with Full. It allocates nothing.
//
// The zero Writer holds no memory; Reset gives it some.
//
//noalloc:fixed
type Writer struct {
	buf    bounded.Buf
	depth  int
	err    Error
	failed bool
	// scratch is where a float, a time, or an escape is formatted before it
	// is written, and text where a type's AppendText appends.
	scratch [scratchBytes]byte
	text    [textBytes]byte
}

// scratchBytes holds the longest float formatFloat writes and the longest
// time formatTime writes.
const scratchBytes = 64

// Reset makes mem the Writer's memory, emptied, writing out through flush
// when it fills, or never when flush is nil, and clears any error.
//
//noalloc:root
func (w *Writer) Reset(mem []byte, flush io.Writer) {
	w.buf.Reset(mem, flush)
	w.depth, w.err, w.failed = 0, Error{}, false
}

// Bytes is what the Writer holds since it last flushed.
//
//noalloc:root
func (w *Writer) Bytes() []byte { return w.buf.Bytes() }

// Flush writes out what the Writer holds, and returns Err.
//
//noalloc:root
func (w *Writer) Flush() error {
	w.buf.Flush()
	return w.Err()
}

// Err is the first failure since Reset: an unsupported value, memory full
// without a flush writer, or the flush writer's error.
//
//noalloc:root
func (w *Writer) Err() error {
	if !w.failed {
		switch {
		case w.buf.Err() != nil:
			w.fail(IO, "")
			w.err.Err = w.buf.Err()
		case w.buf.Full():
			w.fail(Full, "")
		default:
			return nil
		}
	}
	return &w.err
}

func (w *Writer) fail(k ErrorKind, field string) {
	if !w.failed {
		w.err, w.failed = Error{Kind: k, Field: field}, true
	}
}

// Enter counts a struct or slice being written, failing past MaxDepth, which
// only a value that holds itself reaches. It reports whether to go on.
//
//noalloc:root
func (w *Writer) Enter(field string) bool {
	if w.depth++; w.depth > MaxDepth {
		w.fail(TooDeep, field)
		return false
	}
	return true
}

// Leave ends what Enter began.
//
//noalloc:root
func (w *Writer) Leave() { w.depth-- }

// Raw writes s as it is: punctuation and quoted keys, which jsongen writes
// already escaped.
//
//noalloc:root
func (w *Writer) Raw(s string) { w.buf.WriteString(s) }

// String writes s as json.Marshal writes a string.
//
//noalloc:root
func (w *Writer) String(s string) { WriteString(&w.buf, s) }

// Int writes v.
//
//noalloc:root
func (w *Writer) Int(v int64) {
	if v < 0 {
		w.buf.WriteByte('-')
		// -v of the most negative int64 is itself, whose bits as a uint64 are
		// its magnitude.
		w.buf.WriteUint(uint64(-v))
		return
	}
	w.buf.WriteUint(uint64(v))
}

// Uint writes v.
//
//noalloc:root
func (w *Writer) Uint(v uint64) { w.buf.WriteUint(v) }

// Bool writes v.
//
//noalloc:root
func (w *Writer) Bool(v bool) {
	if v {
		w.buf.WriteString("true")
	} else {
		w.buf.WriteString("false")
	}
}

// Float writes f as json.Marshal writes a float of bits 32 or 64. An
// infinity or NaN, which JSON cannot hold, fails with Unsupported.
//
//noalloc:root
func (w *Writer) Float(f float64, bits int, field string) {
	n, ok := formatFloat((*[floatBytes]byte)(w.scratch[:floatBytes]), f, bits)
	if !ok {
		w.fail(Unsupported, field)
		return
	}
	w.writeScratch(n)
}

// Base64 writes b as json.Marshal writes a []byte: null for nil, and
// otherwise standard base64 in quotes.
//
//noalloc:root
func (w *Writer) Base64(b []byte) {
	if b == nil {
		w.buf.WriteString("null")
		return
	}
	w.buf.WriteByte('"')
	// Whole groups of three bytes at a time, as many as the memory holds, so
	// a slice longer than memory that flushes goes out in pieces.
	for len(b) > 0 {
		chunk := len(b)
		if most := 3 * (w.buf.Size() / 4); chunk > most && most > 0 {
			chunk = most
		}
		n := base64.StdEncoding.EncodedLen(chunk)
		if !w.buf.Room(n) {
			return
		}
		base64.StdEncoding.Encode(w.buf.Avail()[:n], b[:chunk])
		w.buf.Commit(n)
		b = b[chunk:]
	}
	w.buf.WriteByte('"')
}

// Bytes is b as a []byte over the same memory, for Base64 to write a slice
// of a named byte type: nil for nil.
//
//noalloc:root
func Bytes[E ~uint8](b []E) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(b))), len(b))
}

// Time writes t as time.Time's MarshalJSON does. A year outside 0 to 9999
// or an offset of a day or more, which MarshalJSON refuses, fails with
// Unsupported.
//
//noalloc:root
func (w *Writer) Time(t time.Time, field string) {
	n, ok := formatTime((*[scratchBytes]byte)(w.scratch[:]), t)
	if !ok {
		w.fail(Unsupported, field)
		return
	}
	w.buf.WriteByte('"')
	w.writeScratch(n)
	w.buf.WriteByte('"')
}

// formatTime writes t into buf as time.RFC3339Nano formats it, and returns
// its length, or false for a time MarshalJSON refuses.
func formatTime(buf *[scratchBytes]byte, t time.Time) (int, bool) {
	year, month, day := t.Date()
	hour, minute, sec := t.Clock()
	nsec := t.Nanosecond()
	_, offset := t.Zone()
	// The offset in minutes, truncated toward zero, as Format has it.
	zone := offset / 60
	if year < 0 || year > 9999 || zone <= -24*60 || zone >= 24*60 {
		return 0, false
	}
	two := func(at, v int) { buf[at], buf[at+1] = byte('0'+v/10), byte('0'+v%10) }
	two(0, year/100)
	two(2, year%100)
	buf[4] = '-'
	two(5, int(month))
	buf[7] = '-'
	two(8, day)
	buf[10] = 'T'
	two(11, hour)
	buf[13] = ':'
	two(14, minute)
	buf[16] = ':'
	two(17, sec)
	n := 19
	if nsec != 0 {
		// The fraction's digits, trailing zeros dropped.
		buf[n] = '.'
		for i := 9; i > 0; i-- {
			buf[n+i] = byte('0' + nsec%10)
			nsec /= 10
		}
		n += 10
		for buf[n-1] == '0' {
			n--
		}
	}
	// Z only for no offset at all: an offset of under a minute is +00:00.
	if offset == 0 {
		buf[n] = 'Z'
		return n + 1, true
	}
	buf[n] = '+'
	if zone < 0 {
		buf[n] = '-'
		zone = -zone
	}
	two(n+1, zone/60)
	buf[n+3] = ':'
	two(n+4, zone%60)
	return n + 6, true
}

// textBytes is the room TextBegin gives a type's AppendText.
const textBytes = 128

// TextBegin starts a value a type writes with its own AppendText, which
// jsongen calls as b, err := v.AppendText(w.TextBegin()) and then TextEnd: it
// is where AppendText appends, with room for textBytes.
//
//noalloc:root
func (w *Writer) TextBegin() []byte { return w.text[:0] }

// TextEnd writes what AppendText appended to TextBegin's slice as a JSON
// string. Text that outgrew the room, which AppendText had to allocate for,
// fails with Full.
//
//noalloc:root
func (w *Writer) TextEnd(b []byte, err error, field string) {
	if err != nil {
		w.fail(Method, field)
		w.err.Err = err
		return
	}
	if len(b) > 0 && unsafe.SliceData(b) != &w.text[0] {
		w.fail(Full, field)
		return
	}
	w.buf.WriteByte('"')
	w.writeEscapedBytes(b)
	w.buf.WriteByte('"')
}

// writeScratch writes the first n bytes of scratch. It copies them in rather
// than handing the buffer a slice of the Writer, which would move a Writer
// held in a local to the heap.
func (w *Writer) writeScratch(n int) {
	if !w.buf.Room(n) {
		return
	}
	copy(w.buf.Avail()[:n], w.scratch[:n])
	w.buf.Commit(n)
}

// writeEscapedBytes writes p as WriteEscaped writes a string, a byte at a
// time, for p in the Writer's own memory, as writeScratch does.
func (w *Writer) writeEscapedBytes(p []byte) {
	for i := 0; i < len(p); {
		if c := p[i]; c < utf8.RuneSelf {
			if esc := asciiEscapes[c]; esc != "" {
				w.buf.WriteString(esc)
			} else {
				w.buf.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(p[i:])
		if esc := runeEscape(r, size); esc != "" {
			w.buf.WriteString(esc)
		} else {
			for _, c := range p[i : i+size] {
				w.buf.WriteByte(c)
			}
		}
		i += size
	}
}

// QuotedString writes s as json.Marshal writes a string field tagged
// ",string": its JSON, as a JSON string. The string's JSON is s with
// WriteEscaped's escapes, and each of those is escaped again.
//
//noalloc:root
func (w *Writer) QuotedString(s string) {
	w.buf.WriteString(`"\"`)
	start := 0
	for i := 0; i < len(s); {
		var esc string
		size := 1
		if c := s[i]; c < utf8.RuneSelf {
			esc = asciiEscapes[c]
		} else {
			var r rune
			r, size = utf8.DecodeRuneInString(s[i:])
			esc = runeEscape(r, size)
		}
		if esc != "" {
			w.buf.WriteString(s[start:i])
			w.quoteEscape(esc)
			start = i + size
		}
		i += size
	}
	w.buf.WriteString(s[start:])
	w.buf.WriteString(`\""`)
}

// quoteEscape writes esc, an escape WriteEscaped writes, escaped again: its
// backslash doubled and a quote or backslash after it escaped.
func (w *Writer) quoteEscape(esc string) {
	if esc[0] != '\\' {
		w.buf.WriteString(esc)
		return
	}
	w.buf.WriteString(`\\`)
	if esc[1] == '"' || esc[1] == '\\' {
		w.buf.WriteByte('\\')
	}
	w.buf.WriteString(esc[1:])
}
