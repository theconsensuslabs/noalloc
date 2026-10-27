//go:build !goexperiment.jsonv2

package json

// jsonInvalidUTF8 is what json.Marshal writes for a byte of invalid UTF-8 in
// a string.
const jsonInvalidUTF8 = `\ufffd`
