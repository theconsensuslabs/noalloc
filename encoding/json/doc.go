// Package json encodes and decodes JSON without allocating, through the
// WriteJSON and ReadJSON methods the jsongen command generates for a
// package's types.
//
// A Writer writes into memory its caller gives it, flushing to an io.Writer
// as that fills. A Reader reads through a buffer its caller gives it, from an
// io.Reader or from memory, and decodes strings into an Arena of the caller's
// memory, which asks the caller for more when it fills. Nothing in either is
// set up on first use, so the first encode and decode allocate no more than
// any other: nothing.
//
// What a Writer writes decodes, with encoding/json, to the value
// json.Marshal's output decodes to, and is safe inside a script: <, >, &,
// U+2028, and U+2029 are escaped. A Reader accepts what encoding/json
// accepts and decodes what it decodes, except where encoding/json would
// allocate: a nil pointer, a slice at its capacity, or an Arena out of
// memory fails with Full, and a time with an offset decodes as the same
// instant in UTC.
package json
