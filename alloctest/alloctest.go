// Package alloctest fails a test when a function allocates on the heap. It
// confirms at run time what noalloc proves statically, for code noalloc cannot
// see into, such as assembly or functions on its allowlist.
package alloctest

import "testing"

// runs is how many calls of f testing.AllocsPerRun averages over.
const runs = 100

// Zero fails t when calling f allocates, naming the call what. Under the race
// detector, which allocates for its own bookkeeping, it skips t instead.
func Zero(t testing.TB, what string, f func()) {
	t.Helper()
	if race {
		t.Skip("the race detector allocates for its own bookkeeping")
	}
	if n := testing.AllocsPerRun(runs, f); n != 0 {
		t.Errorf("%s allocates %.1f times per call", what, n)
	}
}
