// The escapers here adapt html/template's htmlEscaper, attrEscaper, urlFilter,
// urlNormalizer, and urlEscaper, from Go:
//
// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the GO_LICENSE file.

// Package template holds append versions of html/template's escapers. Each
// writes the bytes its standard library counterpart returns for a string, and
// allocates only when dst must grow.
//
// The escapers take plain text. A value with a content type (template.HTML,
// template.URL, and so on) is the caller's to route: html/template passes some
// of those through or escapes them differently.
package template

import "strings"

// AppendHTML appends s escaped as html/template's htmlEscaper and attrEscaper
// escape plain text, for HTML text and quoted attribute values.
//
//noalloc:append
func AppendHTML(dst []byte, s string) []byte {
	// Every byte replaced is ASCII, and bytes of multibyte or invalid UTF-8 are
	// all at least 0x80, so the bytes can be scanned one at a time.
	start := 0
	for i := 0; i < len(s); i++ {
		var repl string
		switch s[i] {
		case 0:
			repl = "\uFFFD"
		case '"':
			repl = "&#34;"
		case '&':
			repl = "&amp;"
		case '\'':
			repl = "&#39;"
		case '+':
			repl = "&#43;"
		case '<':
			repl = "&lt;"
		case '>':
			repl = "&gt;"
		default:
			continue
		}
		dst = append(dst, s[start:i]...)
		dst = append(dst, repl...)
		start = i + 1
	}
	return append(dst, s[start:]...)
}

// filterFailsafe is what html/template writes for a value a filter rejects.
const filterFailsafe = "ZgotmplZ"

// AppendURLFiltered appends s as html/template's urlFilter returns it: s
// itself, unless s names a scheme other than http, https, or mailto.
//
//noalloc:append
func AppendURLFiltered(dst []byte, s string) []byte {
	if scheme, _, ok := strings.Cut(s, ":"); ok && !strings.Contains(scheme, "/") &&
		!strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https") && !strings.EqualFold(scheme, "mailto") {
		return append(append(dst, '#'), filterFailsafe...)
	}
	return append(dst, s...)
}

// AppendURLNormalized appends s as html/template's urlNormalizer returns it,
// for a URL up to its query: bytes a URL may not hold are percent-encoded, and
// reserved bytes and existing escapes are kept.
//
//noalloc:append
func AppendURLNormalized(dst []byte, s string) []byte {
	return appendURL(dst, s, true)
}

// AppendURLEscaped appends s as html/template's urlEscaper returns it, for a
// URL's query or fragment: everything but unreserved bytes is percent-encoded.
//
//noalloc:append
func AppendURLEscaped(dst []byte, s string) []byte {
	return appendURL(dst, s, false)
}

// appendURL is html/template's processURLOnto.
//
//noalloc:append
func appendURL(dst []byte, s string, norm bool) []byte {
	const hex = "0123456789abcdef"
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '!', '#', '$', '&', '*', '+', ',', '/', ':', ';', '=', '?', '@', '[', ']':
			if norm {
				continue
			}
		case '-', '.', '_', '~':
			continue
		case '%':
			if norm && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
				continue
			}
		default:
			if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
				continue
			}
		}
		dst = append(dst, s[start:i]...)
		dst = append(dst, '%', hex[c>>4], hex[c&0xf])
		start = i + 1
	}
	return append(dst, s[start:]...)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
