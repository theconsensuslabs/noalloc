package json

import (
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"

	istrconv "github.com/theconsensuslabs/noalloc/encoding/json/internal/strconv"
)

// A Reader reads JSON from memory its caller gives it, for the ReadJSON
// methods jsongen generates. It reads input through a buffer the caller
// owns, and a string of any length straight into an Arena. A number, which
// it needs whole, may be only as long as the buffer, and a key or text for
// UnmarshalText only as long as the Reader holds. It allocates nothing.
//
// It accepts what encoding/json accepts and decodes what encoding/json
// decodes, with the differences jsongen documents. Like encoding/json, a
// value that does not fit its field is skipped and the decode goes on; Err
// reports the first one.
//
//noalloc:fixed
type Reader struct {
	src      io.Reader
	buf      []byte
	pos, end int
	// off is the input offset of buf[0].
	off int64
	eof bool
	// Arena holds the strings the Reader decodes. The caller gives it memory,
	// and Reset leaves it as it is. An Arena with none fails a string with
	// Full.
	Arena Arena
	depth int
	// first is set by ObjectStart and ArrayStart until the next ObjectNext
	// or ArrayNext.
	first bool
	err   Error
	// failed is input that cannot be read further; typed is a value that did
	// not fit its field, after which reading goes on.
	failed, typed bool
	key           [keyBytes]byte
	// keyLen is the length of the key in key, or -1 for one longer than it.
	keyLen int
	// objects holds, a bit for each depth Skip has opened, whether that
	// depth is an object.
	objects [MaxDepth/64 + 1]uint64
}

// keyBytes is the longest object key a Reader holds whole, and the longest
// text it hands to UnmarshalText.
const keyBytes = 256

// MinBuffer is the shortest buffer Reset accepts.
const MinBuffer = 16

// Reset reads from src through buf, which must hold at least MinBuffer bytes
// and holds the longest number or key the input may have.
//
//noalloc:root
func (r *Reader) Reset(src io.Reader, buf []byte) {
	if len(buf) < MinBuffer {
		panic("json: Reader buffer shorter than MinBuffer")
	}
	r.reset(src, buf, 0)
}

// ResetBytes reads data, all of it in memory, which the Reader never writes.
//
//noalloc:root
func (r *Reader) ResetBytes(data []byte) {
	r.reset(nil, data, len(data))
	r.eof = true
}

func (r *Reader) reset(src io.Reader, buf []byte, end int) {
	r.src, r.buf, r.pos, r.end, r.off, r.eof = src, buf, 0, end, 0, false
	r.depth, r.first, r.err, r.failed, r.typed, r.keyLen = 0, false, Error{}, false, false, 0
}

// Err is the first failure since Reset: input that is not JSON, or ends
// early, or a value that did not fit its field.
//
//noalloc:root
func (r *Reader) Err() error {
	if r.failed || r.typed {
		return &r.err
	}
	return nil
}

// Finish ends a decode of one value: anything past it but space is invalid.
// It returns Err.
//
//noalloc:root
func (r *Reader) Finish() error {
	if r.peek() >= 0 {
		r.fail(Syntax, "")
	}
	return r.Err()
}

// Kind is the kind of value Peek finds next.
type Kind byte

const (
	ObjectKind Kind = '{'
	ArrayKind  Kind = '['
	StringKind Kind = '"'
	NumberKind Kind = '0'
	BoolKind   Kind = 't'
	NullKind   Kind = 'n'
	// NoKind is the end of the input, or a byte that starts no value.
	NoKind Kind = 0
)

// Peek is the kind of the next value, which it leaves unread, for a ReadJSON
// method written by hand.
//
//noalloc:root
func (r *Reader) Peek() Kind {
	switch c := r.peek(); {
	case c == '{', c == '[', c == '"', c == 'n':
		return Kind(c)
	case c == '-' || '0' <= c && c <= '9':
		return NumberKind
	case c == 't' || c == 'f':
		return BoolKind
	}
	return NoKind
}

// More reports whether another value follows, for input that holds a stream
// of them.
//
//noalloc:root
func (r *Reader) More() bool { return r.peek() >= 0 }

// fail records a failure past which the input is not read. It replaces a
// value that did not fit, as encoding/json reports invalid input over one.
func (r *Reader) fail(k ErrorKind, field string) {
	if !r.failed {
		r.err, r.failed = Error{Kind: k, Offset: r.off + int64(r.pos), Field: field}, true
	}
}

// mistype records a value that did not fit its field, unless one already
// did, and skips it.
func (r *Reader) mistype(k ErrorKind, field string) {
	if !r.failed && !r.typed {
		r.err, r.typed = Error{Kind: k, Offset: r.off + int64(r.pos), Field: field}, true
	}
	r.Skip()
}

// more reads more input into the buffer, first moving what is unread to its
// front. It reports false at the end of the input, on a read error, and when
// what is unread fills the buffer.
func (r *Reader) more() bool {
	if r.src == nil || r.eof || r.failed {
		return false
	}
	if r.pos > 0 {
		n := copy(r.buf, r.buf[r.pos:r.end])
		r.off += int64(r.pos)
		r.pos, r.end = 0, n
	}
	if r.end == len(r.buf) {
		return false
	}
	for {
		//noalloc:exempt response: the caller's reader, which may allocate as it reads.
		n, err := r.src.Read(r.buf[r.end:])
		r.end += n
		switch {
		case err == io.EOF:
			r.eof = true
			return n > 0
		case err != nil:
			r.fail(IO, "")
			r.err.Err = err
			return false
		case n > 0:
			return true
		}
	}
}

// ensure makes n bytes past pos readable when the input holds them.
func (r *Reader) ensure(n int) bool {
	for r.end-r.pos < n {
		if !r.more() {
			return false
		}
	}
	return true
}

// peek skips space and returns the next byte, or -1 at the end of the
// input.
func (r *Reader) peek() int {
	for {
		// The position is kept in a local while space is skipped, which the
		// compiler holds in a register rather than storing each step.
		buf, pos := r.buf[:r.end], r.pos
		for pos < len(buf) {
			if c := buf[pos]; c > ' ' || c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				r.pos = pos
				return int(c)
			}
			pos++
		}
		r.pos = pos
		if !r.more() {
			return -1
		}
	}
}

// ended fails at the end of the input, or does nothing past a failure.
func (r *Reader) ended() {
	r.fail(UnexpectedEOF, "")
}

// unexpected fails on c, or the end of the input at -1, where neither
// belongs.
func (r *Reader) unexpected(c int) {
	if c < 0 {
		r.ended()
	} else {
		r.fail(Syntax, "")
	}
}

func (r *Reader) push(object bool) bool {
	if r.depth++; r.depth > MaxDepth {
		r.fail(TooDeep, "")
		return false
	}
	w, bit := r.depth/64, uint64(1)<<(r.depth%64)
	if object {
		r.objects[w] |= bit
	} else {
		r.objects[w] &^= bit
	}
	return true
}

func (r *Reader) isObject() bool { return r.objects[r.depth/64]&(1<<(r.depth%64)) != 0 }

// ObjectStart begins reading an object into field, as ObjectNext goes on
// with. It reports false for null, which leaves the field as it was, and for
// a value of another kind, which it skips.
//
//noalloc:root
func (r *Reader) ObjectStart(field string) bool {
	switch c := r.peek(); c {
	case '{':
		r.pos++
		if !r.push(true) {
			return false
		}
		r.first = true
		return true
	case 'n':
		r.literal("null")
	default:
		r.typeOrSyntax(c, field)
	}
	return false
}

// ObjectNext reads up to the next key of the object ObjectStart began, which
// Key returns, and reports whether there is one. At the object's end it
// reads past the brace and reports false.
//
//noalloc:root
func (r *Reader) ObjectNext() bool {
	if r.failed {
		return false
	}
	c := r.peek()
	if c == '}' {
		r.pos++
		r.depth--
		r.first = false
		return false
	}
	if !r.first {
		if c != ',' {
			r.unexpected(c)
			return false
		}
		r.pos++
		c = r.peek()
	}
	r.first = false
	return r.member(c, true)
}

// member reads a key starting at c and the colon after it.
func (r *Reader) member(c int, keep bool) bool {
	if c != '"' {
		r.unexpected(c)
		return false
	}
	r.pos++
	sink := sinkDiscard
	if keep {
		sink, r.keyLen = sinkKey, 0
	}
	if !r.str(sink) {
		return false
	}
	if c := r.peek(); c != ':' {
		r.unexpected(c)
		return false
	}
	r.pos++
	return true
}

// Key is the key ObjectNext read, valid until the next read. A key longer
// than the Reader holds is returned as "\x00", which no field is named.
//
//noalloc:root
func (r *Reader) Key() string {
	if r.keyLen < 0 {
		return "\x00"
	}
	return unsafe.String(&r.key[0], r.keyLen)
}

// KeyFold reports whether key matches name as encoding/json matches a key to
// a field with no exact match: ignoring case.
//
//noalloc:root
func KeyFold(key, name string) bool { return strings.EqualFold(key, name) }

// ArrayStart begins reading an array into field, as ArrayNext goes on with.
// It reports false for null and for a value of another kind, as ObjectStart
// does.
//
//noalloc:root
func (r *Reader) ArrayStart(field string) bool {
	switch c := r.peek(); c {
	case '[':
		r.pos++
		if !r.push(false) {
			return false
		}
		r.first = true
		return true
	case 'n':
		r.literal("null")
	default:
		r.typeOrSyntax(c, field)
	}
	return false
}

// ArrayNext reads up to the next element of the array ArrayStart began and
// reports whether there is one. At the array's end it reads past the bracket
// and reports false.
//
//noalloc:root
func (r *Reader) ArrayNext() bool {
	if r.failed {
		return false
	}
	c := r.peek()
	if c == ']' {
		r.pos++
		r.depth--
		r.first = false
		return false
	}
	if !r.first {
		if c != ',' {
			r.unexpected(c)
			return false
		}
		r.pos++
	}
	r.first = false
	return true
}

// Null reads null and reports true when it is next, and otherwise reads
// nothing.
//
//noalloc:root
func (r *Reader) Null() bool {
	if r.peek() != 'n' {
		return false
	}
	return r.literal("null")
}

// Full fails the decode at a value with no memory to decode into: a slice
// at its capacity or a nil pointer.
//
//noalloc:root
func (r *Reader) Full(field string) {
	r.fail(Full, field)
}

// typeOrSyntax skips a value starting at c that does not fit field, or fails
// when c starts no value.
func (r *Reader) typeOrSyntax(c int, field string) {
	switch {
	case c == '{' || c == '[' || c == '"' || c == '-' || '0' <= c && c <= '9' || c == 't' || c == 'f' || c == 'n':
		r.mistype(Type, field)
	default:
		r.unexpected(c)
	}
}

// literal reads word, which the next byte starts.
func (r *Reader) literal(word string) bool {
	r.ensure(len(word))
	if r.end-r.pos < len(word) {
		if string(r.buf[r.pos:r.end]) == word[:r.end-r.pos] {
			r.ended()
		} else {
			r.fail(Syntax, "")
		}
		return false
	}
	if string(r.buf[r.pos:r.pos+len(word)]) != word {
		r.fail(Syntax, "")
		return false
	}
	r.pos += len(word)
	return true
}

// Where str writes a string's contents.
const (
	sinkDiscard = iota
	sinkKey
	sinkArena
)

// emit writes p, part of a string's contents, to sink.
func (r *Reader) emit(sink int, p []byte) bool {
	switch sink {
	case sinkKey:
		if r.keyLen >= 0 {
			if r.keyLen+len(p) > keyBytes {
				r.keyLen = -1
			} else {
				r.keyLen += copy(r.key[r.keyLen:], p)
			}
		}
	case sinkArena:
		if !r.Arena.write(p) {
			r.fail(Full, "")
			return false
		}
	}
	return true
}

// replacement is U+FFFD, which encoding/json decodes invalid UTF-8 and lone
// surrogates as.
var replacement = []byte("\uFFFD")

// str reads a string's contents, after its opening quote, through its
// closing one, unescaped into sink.
func (r *Reader) str(sink int) bool {
	for {
		// The run of bytes copied as they are: 8 at a time while none
		// stops it, and valid multibyte runes whole while the buffer holds
		// one.
		i := r.pos
		for {
			for i+8 <= r.end {
				if stop := readStop(word(r.buf, i)); stop != 0 {
					i += first(stop)
					break
				}
				i += 8
			}
			if i == r.end {
				break
			}
			c := r.buf[i]
			if c < utf8.RuneSelf {
				if c == '"' || c == '\\' || c < 0x20 {
					break
				}
				i++
				continue
			}
			if r.end-i < utf8.UTFMax && !r.eof && r.src != nil {
				break
			}
			if ru, size := utf8.DecodeRune(r.buf[i:r.end]); ru != utf8.RuneError || size > 1 {
				i += size
				continue
			}
			break
		}
		if !r.emit(sink, r.buf[r.pos:i]) {
			return false
		}
		r.pos = i
		if i == r.end {
			if !r.more() {
				r.ended()
				return false
			}
			continue
		}
		switch c := r.buf[i]; {
		case c == '"':
			r.pos++
			return true
		case c == '\\':
			if !r.escape(sink) {
				return false
			}
		case c < 0x20:
			r.fail(Syntax, "")
			return false
		default:
			r.ensure(utf8.UTFMax)
			ru, size := utf8.DecodeRune(r.buf[r.pos:r.end])
			p := r.buf[r.pos : r.pos+size]
			if ru == utf8.RuneError && size == 1 {
				p = replacement
			}
			if !r.emit(sink, p) {
				return false
			}
			r.pos += size
		}
	}
}

// escape reads the escape sequence at pos into sink.
func (r *Reader) escape(sink int) bool {
	if !r.ensure(2) {
		r.ended()
		return false
	}
	var out [utf8.UTFMax]byte
	var p []byte
	switch e := r.buf[r.pos+1]; e {
	case '"', '\\', '/':
		out[0], p = e, out[:1]
	case 'b':
		out[0], p = '\b', out[:1]
	case 'f':
		out[0], p = '\f', out[:1]
	case 'n':
		out[0], p = '\n', out[:1]
	case 'r':
		out[0], p = '\r', out[:1]
	case 't':
		out[0], p = '\t', out[:1]
	case 'u':
		ru, ok := r.hex4(2)
		if !ok {
			return false
		}
		n := 6
		if utf16Surrogate(ru) {
			ru2 := rune(-1)
			if r.ensure(12) && r.buf[r.pos+6] == '\\' && r.buf[r.pos+7] == 'u' {
				if ru2, ok = r.hex4(8); !ok {
					return false
				}
			}
			if combined := utf16Decode(ru, ru2); combined != utf8.RuneError {
				ru, n = combined, 12
			} else {
				ru = utf8.RuneError
			}
		}
		p = out[:utf8.EncodeRune(out[:], ru)]
		r.pos += n
		return r.emit(sink, p)
	default:
		r.fail(Syntax, "")
		return false
	}
	r.pos += 2
	return r.emit(sink, p)
}

// hex4 reads the four hex digits at pos+at.
func (r *Reader) hex4(at int) (rune, bool) {
	if !r.ensure(at + 4) {
		r.ended()
		return 0, false
	}
	var ru rune
	for _, c := range r.buf[r.pos+at : r.pos+at+4] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c -= 'a' - 10
		case 'A' <= c && c <= 'F':
			c -= 'A' - 10
		default:
			r.fail(Syntax, "")
			return 0, false
		}
		ru = ru<<4 | rune(c)
	}
	return ru, true
}

func utf16Surrogate(r rune) bool { return 0xd800 <= r && r < 0xe000 }

// utf16Decode is the rune a surrogate pair encodes, or RuneError.
func utf16Decode(r1, r2 rune) rune {
	if 0xd800 <= r1 && r1 < 0xdc00 && 0xdc00 <= r2 && r2 < 0xe000 {
		return (r1-0xd800)<<10 | (r2 - 0xdc00) + 0x10000
	}
	return utf8.RuneError
}

// String reads a string into dst, its bytes in the Reader's Arena. null
// leaves dst as it was.
//
//noalloc:root
func (r *Reader) String(dst *string, field string) {
	switch c := r.peek(); c {
	case '"':
		r.pos++
		r.Arena.begin()
		if r.str(sinkArena) {
			*dst = r.Arena.finish()
		}
	case 'n':
		r.literal("null")
	default:
		r.typeOrSyntax(c, field)
	}
}

// Text reads a string for a type's UnmarshalText, which jsongen calls as
// if b, ok := r.Text(field); ok { r.TextErr(v.UnmarshalText(b), field) }. The
// bytes are valid until the next read. null reports false, as does a string
// longer than the Reader holds, which fails with TooLong.
//
//noalloc:root
func (r *Reader) Text(field string) ([]byte, bool) {
	switch c := r.peek(); c {
	case '"':
		r.pos++
		r.keyLen = 0
		if !r.str(sinkKey) {
			return nil, false
		}
		if r.keyLen < 0 {
			r.fail(TooLong, field)
			return nil, false
		}
		return r.key[:r.keyLen:r.keyLen], true
	case 'n':
		r.literal("null")
	default:
		r.typeOrSyntax(c, field)
	}
	return nil, false
}

// TextErr records err from a type's UnmarshalText as a value that did not
// fit, which the decode goes past, as encoding/json does.
//
//noalloc:root
func (r *Reader) TextErr(err error, field string) {
	if err != nil && !r.failed && !r.typed {
		r.err, r.typed = Error{Kind: Method, Offset: r.off + int64(r.pos), Field: field, Err: err}, true
	}
}

// number finds the number at pos, which must be whole in the buffer, and
// returns its length, or 0 after failing.
func (r *Reader) number() int {
	k := 0
	for {
		for r.pos+k < r.end {
			c := r.buf[r.pos+k]
			if !('0' <= c && c <= '9' || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E') {
				break
			}
			k++
		}
		if r.pos+k < r.end || !r.more() {
			break
		}
	}
	if r.pos+k == r.end && !r.eof && r.src != nil && !r.failed {
		r.fail(TooLong, "")
		return 0
	}
	if !validNumber(unsafe.String(&r.buf[r.pos], k)) {
		if r.pos+k == r.end && (r.eof || r.src == nil) && numberPrefix(r.buf[r.pos:r.pos+k]) {
			r.ended()
		} else {
			r.fail(Syntax, "")
		}
		return 0
	}
	return k
}

// numberPrefix reports whether b could begin a number, for input that ends
// inside one.
func numberPrefix(b []byte) bool {
	n := len(b)
	return n > 0 && (b[n-1] == '-' || b[n-1] == '.' || b[n-1] == 'e' || b[n-1] == 'E' || b[n-1] == '+')
}

// Int reads a number into an int of bits bits, reporting false for null and
// for a number that does not fit, which encoding/json refuses: one with a
// fraction or exponent, or past the int's range.
//
//noalloc:root
func (r *Reader) Int(field string, bits int) (int64, bool) {
	k, ok := r.numberFor(field)
	if !ok {
		return 0, false
	}
	b := r.buf[r.pos : r.pos+k]
	neg := b[0] == '-'
	if neg {
		b = b[1:]
	}
	u, ok := parseDigits(b)
	limit := uint64(1) << (bits - 1)
	if !ok || !neg && u >= limit || neg && u > limit {
		r.mistype(Type, field)
		return 0, false
	}
	r.pos += k
	if neg {
		return -int64(u), true
	}
	return int64(u), true
}

// Uint reads a number into a uint of bits bits, as Int does.
//
//noalloc:root
func (r *Reader) Uint(field string, bits int) (uint64, bool) {
	k, ok := r.numberFor(field)
	if !ok {
		return 0, false
	}
	u, ok := parseDigits(r.buf[r.pos : r.pos+k])
	if !ok || bits < 64 && u >= 1<<bits {
		r.mistype(Type, field)
		return 0, false
	}
	r.pos += k
	return u, true
}

// parseDigits parses b, all digits, reporting false for anything else and
// for a value past a uint64.
func parseDigits(b []byte) (uint64, bool) {
	var u uint64
	for _, c := range b {
		d := uint64(c - '0')
		if c < '0' || c > '9' || u > (math.MaxUint64-d)/10 {
			return 0, false
		}
		u = u*10 + d
	}
	return u, len(b) > 0
}

// Float reads a number into a float of bits 32 or 64, reporting false for
// null and for a number past the float's range.
//
//noalloc:root
func (r *Reader) Float(field string, bits int) (float64, bool) {
	k, ok := r.numberFor(field)
	if !ok {
		return 0, false
	}
	f, err := istrconv.ParseFloat(unsafe.String(&r.buf[r.pos], k), bits)
	if err != nil {
		r.mistype(Type, field)
		return 0, false
	}
	r.pos += k
	return f, true
}

// numberFor finds a number for field, which is left at pos, or reads null
// or skips a value of another kind and reports false.
func (r *Reader) numberFor(field string) (int, bool) {
	switch c := r.peek(); {
	case c == '-' || '0' <= c && c <= '9':
		k := r.number()
		return k, k > 0
	case c == 'n':
		r.literal("null")
	default:
		r.typeOrSyntax(c, field)
	}
	return 0, false
}

// Bool reads true or false, reporting false for null and a value of another
// kind.
//
//noalloc:root
func (r *Reader) Bool(field string) (v, ok bool) {
	switch c := r.peek(); c {
	case 't':
		return true, r.literal("true")
	case 'f':
		return false, r.literal("false")
	case 'n':
		r.literal("null")
	default:
		r.typeOrSyntax(c, field)
	}
	return false, false
}

// Time reads a time as time.Time's UnmarshalJSON does, into dst, as the same
// instant in UTC. null leaves dst as it was.
//
//noalloc:root
func (r *Reader) Time(dst *time.Time, field string) {
	b, ok := r.Text(field)
	if !ok {
		return
	}
	t, ok := parseTime(b)
	if !ok {
		// Not the strict RFC 3339 form, which time.Time's UnmarshalJSON may
		// still accept through time.Parse.
		if !r.failed && !r.typed {
			r.err, r.typed = Error{Kind: Unsupported, Offset: r.off + int64(r.pos), Field: field}, true
		}
		return
	}
	*dst = t
}

// parseTime parses b, RFC 3339, as time.Time's UnmarshalJSON does, as the
// same instant in UTC: a time with an offset decodes without the location
// UnmarshalJSON would give it, which takes memory. It refuses what
// UnmarshalJSON accepts only by falling back on time.Parse, such as a
// one-digit hour. It follows the rules of parseRFC3339 in Go's
// time/format_rfc3339.go.
func parseTime(b []byte) (time.Time, bool) {
	num := func(p []byte) (int, bool) {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, len(p) > 0
	}
	// 2006-01-02T15:04:05[.frac](Z|±hh:mm)
	if len(b) < len("2006-01-02T15:04:05Z") || b[4] != '-' || b[7] != '-' || b[10] != 'T' || b[13] != ':' || b[16] != ':' {
		return time.Time{}, false
	}
	year, ok1 := num(b[0:4])
	month, ok2 := num(b[5:7])
	day, ok3 := num(b[8:10])
	hour, ok4 := num(b[11:13])
	minute, ok5 := num(b[14:16])
	sec, ok6 := num(b[17:19])
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) || month < 1 || month > 12 || day < 1 || day > daysIn(month, year) ||
		hour > 23 || minute > 59 || sec > 59 {
		return time.Time{}, false
	}
	rest := b[19:]
	nsec := 0
	if len(rest) >= 2 && rest[0] == '.' && '0' <= rest[1] && rest[1] <= '9' {
		n := 2
		for n < len(rest) && '0' <= rest[n] && rest[n] <= '9' {
			n++
		}
		// Digits past nanoseconds are read and dropped, as time.Parse drops them.
		digits := rest[1:min(n, 10)]
		nsec, _ = num(digits)
		for range 9 - len(digits) {
			nsec *= 10
		}
		rest = rest[n:]
	}
	offset := 0
	switch {
	case len(rest) == 1 && rest[0] == 'Z':
	case len(rest) == len("-07:00") && (rest[0] == '+' || rest[0] == '-') && rest[3] == ':':
		hh, ok1 := num(rest[1:3])
		mm, ok2 := num(rest[4:6])
		if !ok1 || !ok2 || hh > 23 || mm > 59 {
			return time.Time{}, false
		}
		offset = hh*3600 + mm*60
		if rest[0] == '-' {
			offset = -offset
		}
	default:
		return time.Time{}, false
	}
	days := daysFromCivil(year, month, day)
	return time.Unix(days*86400+int64(hour*3600+minute*60+sec-offset), int64(nsec)).UTC(), true
}

func daysIn(month, year int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// daysFromCivil is the days from 1970-01-01 to a date in the proleptic
// Gregorian calendar: Howard Hinnant's days_from_civil, from "chrono-Compatible
// Low-Level Date Algorithms", which is in the public domain.
func daysFromCivil(y, m, d int) int64 {
	if m <= 2 {
		y--
	}
	era := y / 400
	if y < 0 && y%400 != 0 {
		era--
	}
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return int64(era)*146097 + int64(doe) - 719468
}

// Skip reads past the next value, checking that it is JSON.
//
//noalloc:root
func (r *Reader) Skip() {
	base := r.depth
	need := true
	for !r.failed {
		if need {
			switch c := r.peek(); {
			case c == '{' || c == '[':
				r.pos++
				if !r.push(c == '{') {
					return
				}
				closer := int('}')
				if c == '[' {
					closer = ']'
				}
				switch c2 := r.peek(); {
				case c2 == closer:
					r.pos++
					r.depth--
					need = false
				case c == '{':
					if !r.member(c2, false) {
						return
					}
				}
			case c == '"':
				r.pos++
				if !r.str(sinkDiscard) {
					return
				}
				need = false
			case c == '-' || '0' <= c && c <= '9':
				k := r.number()
				if k == 0 {
					return
				}
				r.pos += k
				need = false
			case c == 't':
				need = !r.literal("true")
			case c == 'f':
				need = !r.literal("false")
			case c == 'n':
				need = !r.literal("null")
			default:
				r.unexpected(c)
				return
			}
			continue
		}
		if r.depth == base {
			return
		}
		obj := r.isObject()
		switch c := r.peek(); {
		case c == ',':
			r.pos++
			if obj && !r.member(r.peek(), false) {
				return
			}
			need = true
		case obj && c == '}' || !obj && c == ']':
			r.pos++
			r.depth--
		default:
			r.unexpected(c)
			return
		}
	}
}

// Empty is a slice of no elements that is not nil, which an empty JSON array
// decodes as into a nil slice, without allocating.
//
//noalloc:root
func Empty[T any]() []T {
	return unsafe.Slice((*T)(unsafe.Pointer(&emptyBase)), 0)
}

var emptyBase [2]uint64

// validNumber reports whether s is a JSON number.
func validNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	digits := func() int {
		start := i
		for i < len(s) && '0' <= s[i] && s[i] <= '9' {
			i++
		}
		return i - start
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case digits() == 0:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if digits() == 0 {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	return i == len(s)
}
