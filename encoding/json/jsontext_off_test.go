//go:build !goexperiment.jsonv2

package json_test

// textValid reports false for ok: jsontext exists only with the jsonv2
// experiment.
func textValid([]byte) (valid, ok bool) { return false, false }
