// AppendJSValue adapts how html/template's jsValEscaper finishes a value, from
// Go:
//
// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the GO_LICENSE file.

package template

import "unicode/utf8"

// AppendJSValue appends b, a value's JSON as json.Marshal writes it, as
// html/template's jsValEscaper finishes it for a script: padded with spaces
// where it could run into a keyword, and with U+2028 and U+2029 escaped.
// dst and b must not overlap.
//
//noalloc:append
func AppendJSValue(dst, b []byte) []byte {
	if len(b) == 0 {
		return append(dst, " null "...)
	}
	first, _ := utf8.DecodeRune(b)
	last, _ := utf8.DecodeLastRune(b)
	pad := isJSIdentPart(first) || isJSIdentPart(last)
	if pad {
		dst = append(dst, ' ')
	}
	start := 0
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == '\u2028' || r == '\u2029' {
			dst = append(dst, b[start:i]...)
			dst = append(dst, `\u202`...)
			dst = append(dst, byte('0'+r-0x2020))
			start = i + size
		}
		i += size
	}
	dst = append(dst, b[start:]...)
	if pad {
		dst = append(dst, ' ')
	}
	return dst
}

func isJSIdentPart(r rune) bool {
	return r == '$' || r == '_' || '0' <= r && r <= '9' || 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z'
}
