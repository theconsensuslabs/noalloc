// Package gentest holds types jsongen generates methods for, which its tests
// hold to encoding/json's output and decoding.
package gentest

import (
	"encoding/hex"
	"errors"
	"time"
)

//go:generate go run github.com/theconsensuslabs/noalloc/cmd/jsongen -write Page,Written,Cold,Cookie,Feed,Shapes -read Page,Cold,Cookie,Feed

// ID is written as hex text, as a UUID is.
type ID [4]byte

// AppendText writes into the room b has, as an AppendText a Writer calls
// should: TextBegin's slice has room for any short text. Another caller may
// give it none.
func (id ID) AppendText(b []byte) ([]byte, error) {
	n := len(b)
	if cap(b)-n < 2*len(id) {
		//noalloc:exempt response: a caller that gives no room, which a Writer never is.
		b = append(b[:n:n], make([]byte, 2*len(id))...)[:n]
	}
	b = b[:n+2*len(id)]
	hex.Encode(b[n:], id[:])
	return b, nil
}

// MarshalText is what an encoder without AppendText calls.
func (id ID) MarshalText() ([]byte, error) { return id.AppendText(nil) }

var errID = errors.New("gentest: an ID is 8 hex digits")

func (id *ID) UnmarshalText(b []byte) error {
	if len(b) != 2*len(id) {
		return errID
	}
	if _, err := hex.Decode(id[:], b); err != nil {
		return errID
	}
	return nil
}

type Level int8

type Kind string

// Node is a tree, a type that holds itself.
type Node struct {
	Name     string  `json:"name"`
	Weight   float64 `json:"weight"`
	Children []Node  `json:"children,omitempty"`
}

type Base struct {
	ID      ID        `json:"id"`
	Created time.Time `json:"created"`
}

// Page is read and written.
type Page struct {
	Base
	Title  string   `json:"title"`
	Tags   []string `json:"tags,omitempty"`
	Count  int      `json:"count"`
	U8     uint8
	I16    int16   `json:"i16,omitempty"`
	U64    uint64  `json:"u64"`
	F32    float32 `json:"f32"`
	OK     bool    `json:"ok"`
	Level  Level   `json:"level,omitzero"`
	Kind   Kind    `json:"kind"`
	Next   *string `json:"next,omitempty"`
	Pair   [2]int  `json:"pair"`
	Root   Node    `json:"root"`
	Inline struct {
		A int `json:"a"`
		B string
	} `json:"inline"`
	Skipped string `json:"-"`
	//lint:ignore SA5008 a field named "-", which encoding/json allows
	Dash string `json:"-,"`
	//lint:ignore U1000 an unexported field the generator must skip
	hidden int
}

// Inner is reached through an embedded pointer.
type Inner struct {
	Depth int `json:"depth"`
}

// Written is only written: it holds what reading does not support.
type Written struct {
	*Inner
	Quoted   int      `json:"quoted,string"`
	QuotedS  string   `json:"quoteds,string"`
	Raw      []byte   `json:"raw"`
	Zero     Base     `json:"zero,omitzero"`
	Maybe    *float64 `json:"maybe,omitzero"`
	Optional []int    `json:"optional,omitempty"`
}

// Cold is used by one test alone, so its first encode and decode are the
// first the process makes.
type Cold struct {
	Name   string    `json:"name"`
	Values []int     `json:"values"`
	At     time.Time `json:"at"`
}

// Cookie is a small object, the size of a session cookie's payload.
type Cookie struct {
	UserID ID       `json:"u"`
	Exp    int64    `json:"e"`
	Kind   string   `json:"k"`
	Flags  []string `json:"f"`
}

type Author struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

type Item struct {
	Title  string   `json:"title"`
	URL    string   `json:"url"`
	Score  float64  `json:"score"`
	Posted int64    `json:"posted"`
	Tags   []string `json:"tags"`
	Author Author   `json:"author"`
}

// Feed is a list of items, the shape of a feed API reply.
type Feed []Item

// Octet is a named byte, whose slices encoding/json writes as base64 as it
// does []byte.
type Octet uint8

// Blob is a named slice of a named byte.
type Blob []Octet

// Hex is a byte written as text, whose slices encoding/json writes as an
// array of its texts rather than as base64.
type Hex uint8

func (h Hex) AppendText(b []byte) ([]byte, error) {
	const digits = "0123456789abcdef"
	n := len(b)
	if cap(b)-n < 2 {
		//noalloc:exempt response: a caller that gives no room, which a Writer never is.
		b = append(b[:n:n], 0, 0)[:n]
	}
	b = b[:n+2]
	b[n], b[n+1] = digits[h>>4], digits[h&0xf]
	return b, nil
}

func (h Hex) MarshalText() ([]byte, error) { return h.AppendText(nil) }

// PHex is Hex with its methods on the pointer.
type PHex uint8

func (h *PHex) AppendText(b []byte) ([]byte, error) { return Hex(*h).AppendText(b) }

func (h *PHex) MarshalText() ([]byte, error) { return h.AppendText(nil) }

// Label is text that holds any bytes, so its JSON reaches every escape.
type Label string

func (l Label) AppendText(b []byte) ([]byte, error) {
	n := len(b)
	if cap(b)-n < len(l) {
		//noalloc:exempt response: a caller that gives no room, which a Writer is only for a Label longer than any the fuzzer fills.
		b = append(b[:n:n], make([]byte, len(l))...)[:n]
	}
	b = b[:n+len(l)]
	copy(b[n:], l)
	return b, nil
}

func (l Label) MarshalText() ([]byte, error) { return []byte(l), nil }

type OctetPtr *Octet

type Str string

// Shapes is only written. It holds a field of each shape encoding/json
// treats by kind rather than by type: named bytes and their slices, bytes
// with text methods, named pointers, named strings quoted, and text that
// needs escaping.
type Shapes struct {
	Octets   []Octet  `json:"octets"`
	Blob     Blob     `json:"blob"`
	Bytes    []byte   `json:"bytes"`
	Array    [3]Octet `json:"array"`
	Hexes    []Hex    `json:"hexes"`
	PHexes   []PHex   `json:"phexes"`
	Ptr      *Octet   `json:"ptr"`
	Named    OctetPtr `json:"named"`
	Quoted   Str      `json:"quoted,string"`
	QuotedP  *Str     `json:"quotedp,string"`
	Label    Label    `json:"label"`
	Labels   []Label  `json:"labels"`
	At       time.Time
	Big      int64  `json:"big"`
	BigU     uint64 `json:"bigu"`
	Small    int8   `json:"small"`
	SmallInt int    `json:"smallint"`
}
