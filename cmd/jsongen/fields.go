// fieldsOf, validName, and hasOption adapt typeFields, dominantField,
// isValidTag, and tagOptions.Contains from Go's encoding/json to go/types:
//
// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the GO_LICENSE file.

package main

import (
	"cmp"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"unicode"
)

// field is a struct field encoding/json encodes, found by the rules it
// documents for embedded structs and tags.
type field struct {
	name   string
	goName string
	tagged bool
	index  []int
	// path is the fields from the struct to this one, through embedded ones.
	path      []*types.Var
	typ       types.Type
	omitEmpty bool
	omitZero  bool
	quoted    bool
}

// fieldsOf is the fields of st that encode, in the order they do.
func fieldsOf(st *types.Struct) []field {
	type embedded struct {
		st    *types.Struct
		key   string
		index []int
		path  []*types.Var
	}
	var fields []field
	visited := map[string]bool{}
	next := []embedded{{st: st, key: "\x00top"}}
	count := map[string]int{}
	for len(next) > 0 {
		current := next
		next = nil
		levelCount := count
		count = map[string]int{}
		for _, at := range current {
			if visited[at.key] {
				continue
			}
			visited[at.key] = true
			for i := range at.st.NumFields() {
				sf := at.st.Field(i)
				ft := sf.Type()
				if sf.Embedded() {
					et := ft
					if p, ok := et.(*types.Pointer); ok {
						et = p.Elem()
					}
					if !sf.Exported() && !isStruct(et) {
						continue
					}
				} else if !sf.Exported() {
					continue
				}
				tag := reflect.StructTag(at.st.Tag(i)).Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !validName(name) {
					name = ""
				}
				index := append(slices.Clone(at.index), i)
				path := append(slices.Clone(at.path), sf)
				if p, ok := ft.(*types.Pointer); ok {
					if _, named := types.Unalias(ft).(*types.Named); !named {
						ft = p.Elem()
					}
				}
				if name == "" && sf.Embedded() && isStruct(ft) {
					// An untagged embedded struct's fields are promoted.
					key := types.TypeString(ft, nil)
					count[key]++
					if count[key] == 1 {
						next = append(next, embedded{ft.Underlying().(*types.Struct), key, index, path})
					}
					continue
				}
				fl := field{name: name, goName: sf.Name(), tagged: name != "", index: index, path: path, typ: sf.Type(),
					omitEmpty: hasOption(opts, "omitempty"), omitZero: hasOption(opts, "omitzero")}
				if fl.name == "" {
					fl.name = sf.Name()
				}
				if hasOption(opts, "string") {
					if b, ok := ft.Underlying().(*types.Basic); ok && b.Info()&(types.IsBoolean|types.IsInteger|types.IsFloat|types.IsString) != 0 {
						fl.quoted = true
					}
				}
				fields = append(fields, fl)
				if levelCount[at.key] > 1 {
					// The same struct embedded twice at this depth: a second
					// copy makes the name ambiguous, so neither encodes.
					fields = append(fields, fl)
				}
			}
		}
	}
	// Of the fields sharing a name, the shallowest wins, then a tagged one;
	// two left tied cancel out, as Go's embedding rules have them.
	slices.SortStableFunc(fields, func(a, b field) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.index), len(b.index)); c != 0 {
			return c
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return 1
		}
		return slices.Compare(a.index, b.index)
	})
	var out []field
	for i := 0; i < len(fields); {
		j := i + 1
		for j < len(fields) && fields[j].name == fields[i].name {
			j++
		}
		if j-i == 1 || len(fields[i].index) < len(fields[i+1].index) || fields[i].tagged != fields[i+1].tagged {
			out = append(out, fields[i])
		}
		i = j
	}
	slices.SortFunc(out, func(a, b field) int { return slices.Compare(a.index, b.index) })
	return out
}

func isStruct(t types.Type) bool {
	_, ok := t.Underlying().(*types.Struct)
	return ok
}

// validName reports whether a tag's name is one encoding/json uses.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c) && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

func hasOption(opts, name string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == name {
			return true
		}
	}
	return false
}
