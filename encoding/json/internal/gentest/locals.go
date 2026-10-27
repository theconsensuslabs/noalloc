package gentest

import (
	"errors"

	"github.com/theconsensuslabs/noalloc/encoding/json"
)

var errRoundTrip = errors.New("gentest: round trip failed")

// RoundTripLocals writes p and written, and reads p back into dst, through a
// Writer, a Reader, and an Arena held as locals, as a request handler holds
// them. Between them the two types reach every method generated code calls,
// so the check reports any local the API moves to the heap.
//
//noalloc:root
func RoundTripLocals(p, dst *Page, written *Written, mem, arenaMem []byte) error {
	var w json.Writer
	w.Reset(mem, nil)
	written.WriteJSON(&w)
	w.Reset(mem, nil)
	p.WriteJSON(&w)
	if w.Err() != nil {
		return errRoundTrip
	}
	var r json.Reader
	r.Arena.Reset(arenaMem)
	r.ResetBytes(w.Bytes())
	dst.ReadJSON(&r)
	if r.Finish() != nil {
		return errRoundTrip
	}
	return nil
}
