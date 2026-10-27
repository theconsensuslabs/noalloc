// AppendEscaped and WriteEscaped adapt appendString, and formatFloat the
// format floatEncoder chooses, from Go's encoding/json:
//
// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in internal/strconv/LICENSE.

package json

import (
	"math"
	"unicode/utf8"

	"github.com/theconsensuslabs/noalloc/bounded"
	istrconv "github.com/theconsensuslabs/noalloc/encoding/json/internal/strconv"
)

// asciiEscapes is what the string escapers write for each ASCII byte they
// do not copy as it is, and "" for each they copy.
var asciiEscapes = [utf8.RuneSelf]string{
	0x00: `\u0000`, 0x01: `\u0001`, 0x02: `\u0002`, 0x03: `\u0003`, 0x04: `\u0004`, 0x05: `\u0005`, 0x06: `\u0006`,
	0x07: `\u0007`, '\b': `\b`, '\t': `\t`, '\n': `\n`, 0x0b: `\u000b`, '\f': `\f`, '\r': `\r`, 0x0e: `\u000e`,
	0x0f: `\u000f`, 0x10: `\u0010`, 0x11: `\u0011`, 0x12: `\u0012`, 0x13: `\u0013`, 0x14: `\u0014`, 0x15: `\u0015`,
	0x16: `\u0016`, 0x17: `\u0017`, 0x18: `\u0018`, 0x19: `\u0019`, 0x1a: `\u001a`, 0x1b: `\u001b`, 0x1c: `\u001c`,
	0x1d: `\u001d`, 0x1e: `\u001e`, 0x1f: `\u001f`,
	'"': `\"`, '\\': `\\`, '<': `\u003c`, '>': `\u003e`, '&': `\u0026`,
}

// runeEscape is what the string escapers write for the multibyte rune r of
// size bytes, or "" for one they copy as it is.
func runeEscape(r rune, size int) string {
	switch {
	case r == utf8.RuneError && size == 1:
		return jsonInvalidUTF8
	case r == '\u2028':
		return `\u2028`
	case r == '\u2029':
		return `\u2029`
	}
	return ""
}

// AppendString appends s as json.Marshal encodes a string: quoted, with
// <, >, and & escaped for HTML, U+2028 and U+2029 escaped, and each byte of
// invalid UTF-8 as U+FFFD, written as encoding/json writes it.
//
//noalloc:append
func AppendString(dst []byte, s string) []byte {
	return append(AppendEscaped(append(dst, '"'), s), '"')
}

// AppendEscaped appends what AppendString writes between the quotes.
//
//noalloc:append
func AppendEscaped(dst []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); {
		if i+8 <= len(s) {
			stop := escapeStop(wordAt(s, i))
			if stop == 0 {
				i += 8
				continue
			}
			i += first(stop)
		}
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
			dst = append(dst, s[start:i]...)
			dst = append(dst, esc...)
			start = i + size
		}
		i += size
	}
	return append(dst, s[start:]...)
}

// WriteString writes to b what AppendString appends.
//
//noalloc:root
func WriteString(b *bounded.Buf, s string) {
	b.WriteByte('"')
	WriteEscaped(b, s)
	b.WriteByte('"')
}

// WriteEscaped writes to b what AppendEscaped appends.
//
//noalloc:root
func WriteEscaped(b *bounded.Buf, s string) {
	start := 0
	for i := 0; i < len(s); {
		if i+8 <= len(s) {
			stop := escapeStop(wordAt(s, i))
			if stop == 0 {
				i += 8
				continue
			}
			i += first(stop)
		}
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
			b.WriteString(s[start:i])
			b.WriteString(esc)
			start = i + size
		}
		i += size
	}
	b.WriteString(s[start:])
}

// EscapedLen is the length of what AppendEscaped appends for s, which is at
// most 6*len(s).
//
//noalloc:root
func EscapedLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if i+8 <= len(s) {
			stop := escapeStop(wordAt(s, i))
			if stop == 0 {
				n += 8
				i += 8
				continue
			}
			n += first(stop)
			i += first(stop)
		}
		if c := s[i]; c < utf8.RuneSelf {
			n += max(len(asciiEscapes[c]), 1)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if esc := runeEscape(r, size); esc != "" {
			n += len(esc)
		} else {
			n += size
		}
		i += size
	}
	return n
}

// AppendFloat appends f as json.Marshal encodes a float of the given bits,
// 32 or 64. It reports false, appending nothing, for an infinity or NaN, which
// json.Marshal refuses.
//
//noalloc:append
func AppendFloat(dst []byte, f float64, bits int) ([]byte, bool) {
	var buf [floatBytes]byte
	n, ok := formatFloat(&buf, f, bits)
	if !ok {
		return dst, false
	}
	return append(dst, buf[:n]...), true
}

// floatBytes holds the longest float formatFloat writes.
const floatBytes = 32

// formatFloat writes f into buf as AppendFloat appends it, and returns its
// length, or false for an infinity or NaN.
func formatFloat(buf *[floatBytes]byte, f float64, bits int) (int, bool) {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	n := 0
	if math.Signbit(f) {
		buf[0] = '-'
		n = 1
	}
	abs := math.Abs(f)
	if abs == 0 {
		buf[n] = '0'
		return n + 1, true
	}
	var digits [32]byte
	nd, dp := istrconv.ShortestDigits(&digits, f, bits)
	if bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
		// d.ddde±x, as strconv's 'e' format writes it, with the exponent's
		// leading zero dropped, as encoding/json drops it.
		buf[n] = digits[0]
		n++
		if nd > 1 {
			buf[n] = '.'
			n++
			n += copy(buf[n:], digits[1:nd])
		}
		buf[n] = 'e'
		exp := dp - 1
		if exp < 0 {
			buf[n+1] = '-'
			exp = -exp
		} else {
			buf[n+1] = '+'
		}
		n += 2
		switch {
		case exp >= 100:
			buf[n], buf[n+1], buf[n+2] = byte('0'+exp/100), byte('0'+exp/10%10), byte('0'+exp%10)
			n += 3
		case exp >= 10 || buf[n-1] == '+':
			buf[n], buf[n+1] = byte('0'+exp/10), byte('0'+exp%10)
			n += 2
		default:
			buf[n] = byte('0' + exp)
			n++
		}
		return n, true
	}
	// ddd.ddd, as strconv's 'f' format writes it.
	switch {
	case dp <= 0:
		buf[n], buf[n+1] = '0', '.'
		n += 2
		for range -dp {
			buf[n] = '0'
			n++
		}
		n += copy(buf[n:], digits[:nd])
	case dp >= nd:
		n += copy(buf[n:], digits[:nd])
		for range dp - nd {
			buf[n] = '0'
			n++
		}
	default:
		n += copy(buf[n:], digits[:dp])
		buf[n] = '.'
		n++
		n += copy(buf[n:], digits[dp:nd])
	}
	return n, true
}
