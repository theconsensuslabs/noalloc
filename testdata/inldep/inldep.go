// Package inldep is a dependency whose function boxes the result of a call
// the compiler inlines into it.
package inldep

func twice(n int) int { return n * 2 }

// Boxed boxes what twice returns.
//
//go:noinline
func Boxed(n int) any { return twice(n) }

// Unboxed returns what twice returns.
//
//go:noinline
func Unboxed(n int) int { return twice(n) }

var hook func(int) int

// Install keeps a closure over n, which allocates the closure.
//
//go:noinline
func Install(n int) { hook = func(m int) int { return m + n } }

// Hook runs the closure Install kept, which allocates nothing.
//
//go:noinline
func Hook(m int) int { return hook(m) }

// Values returns a closure over xs, which a caller that inlines it can keep
// on its stack.
func Values(xs []int) func(func(int) bool) {
	return func(yield func(int) bool) {
		for _, x := range xs {
			if !yield(x) {
				return
			}
		}
	}
}

// Total ranges over Values, inlined.
//
//go:noinline
func Total(xs []int) int {
	t := 0
	for x := range Values(xs) {
		t += x
	}
	return t
}

type counter struct{ n int }

// newCounter returns a counter, which escapes unless the call is inlined.
func newCounter() *counter { return new(counter) }

// Count keeps the counter newCounter, inlined, makes on its stack.
//
//go:noinline
func Count(b []byte) int {
	c := newCounter()
	c.n = len(b)
	return c.n
}
