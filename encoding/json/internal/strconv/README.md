A copy of Go 1.27.0's `src/internal/strconv`, without its tests and its
table generator, under Go's license in `LICENSE`. Its parsers return
`Error` constants rather than building a `*strconv.NumError`, so a failed
parse allocates nothing.

One change from Go's source, which `upstream.patch` holds: `atof.go`
assigns `ErrRange` to an `error` variable from a package variable,
`errRange`, so no constant is converted to an interface where noalloc's
escape analysis would count it.

CI runs `.github/vendor-check.sh`, which holds this directory to the
`src/internal/strconv` of the Go it runs: each file there, with
`upstream.patch` applied, byte for byte, and `LICENSE`. When a Go release
changes that source, copy the files again and apply the patch.

One addition: `shortest.go` exposes the shortest-digits algorithm as
`ShortestDigits`, which writes the digits into an array, for a caller that
lays them out without appending. It unpacks a float as `ftoa.go` does, and
carries Go's notice for that.

`staticcheck.conf` turns off, in this directory only, the three checks the
copied source trips: unused functions (U1000), an OR with zero (SA4016), and
a loop that could be an append (S1011), so the source stays as Go has it.
