// ShortestDigits is not in Go's source; it unpacks a float as ftoa.go's ftoa32
// and ftoa64 do, from Go:
//
// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package strconv

import "math/bits"

// ShortestDigits writes into buf the fewest decimal digits that read back as
// f, a finite float of bitSize 32 or 64 that is not zero, and returns how
// many it wrote and dp, where the decimal point goes: |f| is
// 0.d₁d₂…dₙ × 10^dp.
func ShortestDigits(buf *[32]byte, f float64, bitSize int) (nd, dp int) {
	var b uint64
	var expBits, mantBits, bias int
	if bitSize == 32 {
		b, expBits, mantBits, bias = uint64(float32bits(float32(f))), float32ExpBits, float32MantBits, float32Bias
	} else {
		b, expBits, mantBits, bias = float64bits(f), float64ExpBits, float64MantBits, float64Bias
	}
	exp := int(b>>mantBits) & (1<<expBits - 1)
	mant := b & (1<<mantBits - 1)
	if exp == 0 {
		exp++
	} else {
		mant |= 1 << mantBits
	}
	exp += bias
	s := 64 - bits.Len64(mant)
	m, e := mant<<s, exp-s
	var d uint64
	var p int
	if bitSize == 32 {
		d, p = shortFloat[float32](m, e-mantBits)
	} else {
		d, p = shortFloat[float64](m, e-mantBits)
	}
	dp, nd = setDigits(buf[:], d, p, numDigits(d))
	return nd, dp
}
