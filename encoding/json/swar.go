package json

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// The tests below look at 8 bytes of a string at once. Each is nonzero when
// some byte matches, and never zero when one does. A borrow can flag a byte
// that does not match only above one that does, so the lowest flag, which
// first finds, is always a match. They find a zero byte and a byte less than
// n as Sean Eron Anderson's Bit Twiddling Hacks does,
// https://graphics.stanford.edu/~seander/bithacks.html#ZeroInWord, which is
// in the public domain.

const (
	ones  = 0x0101010101010101
	highs = 0x8080808080808080
)

// below is nonzero when a byte of x is less than n, which is at most 0x80.
func below(x, n uint64) uint64 { return (x - ones*n) &^ x & highs }

// equals is nonzero when a byte of x is c.
func equals(x, c uint64) uint64 {
	y := x ^ ones*c
	return (y - ones) &^ y & highs
}

// readStop is nonzero when a byte of x ends a run of a string's plain bytes
// as read: a quote, a backslash, a control character, or a byte of a
// multibyte rune.
func readStop(x uint64) uint64 { return below(x, 0x20) | equals(x, '"') | equals(x, '\\') | x&highs }

// escapeStop is nonzero when a byte of x is one AppendEscaped does not copy
// as it is: what readStop finds, and <, >, and &.
func escapeStop(x uint64) uint64 {
	return readStop(x) | equals(x, '<') | equals(x, '>') | equals(x, '&')
}

// word is the 8 bytes of b from i on, which must hold them.
func word(b []byte, i int) uint64 { return binary.LittleEndian.Uint64(b[i : i+8]) }

// wordAt is the 8 bytes of s from i on, which must hold them.
func wordAt(s string, i int) uint64 {
	return binary.LittleEndian.Uint64(unsafe.Slice(unsafe.StringData(s[i:]), 8))
}

// first is the index of the first byte stop flags, which is nonzero.
func first(stop uint64) int { return bits.TrailingZeros64(stop) >> 3 }
