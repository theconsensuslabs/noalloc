//go:build goexperiment.jsonv2

package json_test

import "encoding/json/jsontext"

// textValid is whether jsontext finds data valid, allowing what
// encoding/json allows.
func textValid(data []byte) (valid, ok bool) {
	return jsontext.Value(data).IsValid(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true)), true
}
