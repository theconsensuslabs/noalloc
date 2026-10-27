//go:build goexperiment.jsonv2

package json

// jsonInvalidUTF8 is what json.Marshal writes for a byte of invalid UTF-8 in
// a string: with the jsonv2 experiment, which Go 1.27 enables by default, the
// replacement character itself.
const jsonInvalidUTF8 = "\uFFFD"
