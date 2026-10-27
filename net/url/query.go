// AppendQueryEscaped adapts net/url's QueryEscape, which text/template's
// urlquery writes, from Go:
//
// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the GO_LICENSE file.

// Package url holds an append version of net/url's QueryEscape, which
// allocates only when dst must grow.
package url

// AppendQueryEscaped appends s as url.QueryEscape returns it, which is what
// text/template's urlquery writes.
//
//noalloc:append
func AppendQueryEscaped(dst []byte, s string) []byte {
	const hex = "0123456789ABCDEF"
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			continue
		}
		dst = append(dst, s[start:i]...)
		if c == ' ' {
			dst = append(dst, '+')
		} else {
			dst = append(dst, '%', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	return append(dst, s[start:]...)
}
