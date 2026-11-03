// Package errors has the name of the standard package and none of its meaning.
package errors

// Is reports whether err is not target.
func Is(err, target error) bool { return err != target }
