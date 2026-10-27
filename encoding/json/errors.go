package json

import "strconv"

// ErrorKind is what went wrong in an encode or decode.
type ErrorKind uint8

const (
	// Syntax is input that is not JSON.
	Syntax ErrorKind = iota + 1
	// UnexpectedEOF is input that ends inside a value.
	UnexpectedEOF
	// Type is a JSON value that does not fit the Go field it decodes into,
	// such as a string for an int or a number past the int's range. The
	// decode goes on past it, as encoding/json's does.
	Type
	// TooDeep is a value nested deeper than MaxDepth.
	TooDeep
	// TooLong is a number longer than the Reader's buffer, or text for an
	// UnmarshalText or a time longer than the Reader holds.
	TooLong
	// Full is memory the caller gave running out: a Writer with no flush
	// writer, an Arena whose More gave nothing, or a slice at its capacity.
	Full
	// Unsupported is a value JSON cannot hold, such as NaN, or one this
	// package does not decode, such as a time with a one-digit hour, which
	// time.Time's UnmarshalJSON accepts.
	Unsupported
	// IO is an error from the reader or writer the caller passed, in Err.
	IO
	// Method is an error from a type's own AppendText or UnmarshalText, in
	// Err.
	Method
)

// IntBits is the size of an int, which generated code reads an int at.
const IntBits = strconv.IntSize

// MaxDepth is the deepest a value may nest, in objects and arrays read or
// structs and slices written. It is encoding/json's limit for decoding.
const MaxDepth = 10000

// An Error is the first failure of an encode or decode. A Writer and a
// Reader each hold one, so returning it allocates nothing; it is valid until
// the next Reset.
type Error struct {
	Kind ErrorKind
	// Offset is where in the input a decode failed, in bytes. An encode
	// leaves it 0.
	Offset int64
	// Field is the Go field being decoded or encoded, as Type.Field.
	Field string
	// Err is the error behind IO and Method.
	Err error
}

var kindText = [...]string{
	0: "json: error", Syntax: "json: invalid JSON", UnexpectedEOF: "json: unexpected end of JSON input",
	Type: "json: value does not fit its field", TooDeep: "json: nested too deep", TooLong: "json: token longer than the buffer",
	Full: "json: memory full", Unsupported: "json: unsupported value", IO: "json: I/O error", Method: "json: method failed",
}

// Error is the message for e's kind, which allocates nothing. The field, the
// offset, and the error behind it are e's fields, and Unwrap returns the
// last.
func (e *Error) Error() string { return kindText[e.Kind] }

func (e *Error) Unwrap() error { return e.Err }
