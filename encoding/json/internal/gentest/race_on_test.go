//go:build race

package gentest_test

// The race detector allocates for its own bookkeeping, so an allocation count
// taken under it measures the instrumentation rather than the code.
const raceEnabled = true
