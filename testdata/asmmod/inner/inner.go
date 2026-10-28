// Package inner holds the code asmmod's functions with no Go body are
// linked to.
package inner

// Add allocates nothing.
//
//go:noinline
func Add(a, b int) int { return a + b }

// Alloc allocates an int.
//
//go:noinline
func Alloc() *int { return new(int) }

// Run calls f.
//
//go:noinline
func Run(f func()) { f() }
