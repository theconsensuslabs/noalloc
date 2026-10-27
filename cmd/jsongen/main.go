// Command jsongen writes WriteJSON and ReadJSON methods for a package's
// types, which encode and decode JSON through
// github.com/theconsensuslabs/noalloc/encoding/json without allocating.
//
//	jsongen [-write T1,T2] [-read T3] [-o json_gen.go] [dir]
//
// Each type named after -write gets a WriteJSON method and each after -read
// a ReadJSON method, and so does every struct type of the package they hold.
// The output is what encoding/json writes and reads for the same types, for
// the types jsongen accepts; it refuses the rest, naming why:
//
//   - maps, interfaces, channels, functions, complex numbers, and generic
//     types, which a struct instance such as type Jobs List[Job] stands in
//     for;
//   - a type with MarshalJSON, UnmarshalJSON, or only MarshalText, whose
//     result is memory the method allocates; give it WriteJSON and ReadJSON
//     methods, or AppendText for writing;
//   - a type with AppendText and no MarshalText, which encoding/json writes
//     as text only with the jsonv2 experiment;
//   - for reading, slices of bytes, embedded pointers, and ",string" fields.
//
// Reading differs from encoding/json where encoding/json would allocate: a
// nil pointer, a slice past its capacity, and a string longer than the
// Reader's Arena can give memory for fail with Full; a time decodes as the
// same instant in UTC, without the offset it was written with.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	write := flag.String("write", "", "comma-separated types to write WriteJSON methods for")
	read := flag.String("read", "", "comma-separated types to write ReadJSON methods for")
	out := flag.String("o", "json_gen.go", "the file to write, in the package's directory")
	flag.Parse()
	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	src, err := Generate(dir, *out, split(*write), split(*read))
	if err != nil {
		fmt.Fprintln(os.Stderr, "jsongen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(dir, *out), src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "jsongen:", err)
		os.Exit(1)
	}
}

func split(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
