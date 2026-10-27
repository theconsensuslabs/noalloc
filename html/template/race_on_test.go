//go:build race

package template

// The race detector allocates for its own bookkeeping, so an allocation count
// taken under it measures the instrumentation rather than the code.
const raceEnabled = true
