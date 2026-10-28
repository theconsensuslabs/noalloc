package noalloc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// checkWith checks the fixture with src appended and imports added to it.
func checkWith(t *testing.T, imports []string, src string) *Result {
	t.Helper()
	return checkFrom(t, imports, src, nil)
}

// checkFrom is checkWith with the packages under source checked from source.
func checkFrom(t *testing.T, imports []string, src string, source []string) *Result {
	t.Helper()
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var imp strings.Builder
	imp.WriteString("import (\n")
	for _, p := range imports {
		imp.WriteString("\t\"" + p + "\"\n")
	}
	over := strings.Replace(string(b), "import (\n", imp.String(), 1) + src
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		LineOnly: []string{"response"}, SourceModules: source,
		Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(over)}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// flagged reports whether res has a finding in fn whose line's tokens are src.
func flagged(res *Result, fn, src string) bool {
	return slices.ContainsFunc(res.Findings, func(f Finding) bool { return f.Func == fn && f.Source == src })
}

// TestAFlagSetWithTheErrorStopsOnIt checks that a flag a call returns with
// one value whenever its error is a failure, the error it was passed or one
// it makes, ends the call that tests the flag, as testing the error would:
// the failure is made once. A flag that can have the other value on a
// failure leaves it made on each turn.
func TestAFlagSetWithTheErrorStopsOnIt(t *testing.T) {
	res := checkWith(t, nil, `
var errSkip = errors.New("skip")

func lengthOf(x string) (int, error) {
	if x == "" {
		return 0, fmt.Errorf("empty at %q", x)
	}
	return len(x), nil
}

func lengthOrZero(x string) (int, error) {
	if x == "" {
		return 0, fmt.Errorf("empty at %q", x)
	}
	return len(x), nil
}

func ends(err error) (bool, error) {
	if err == errSkip {
		return true, nil
	}
	return err != nil, err
}

func endsUnlessShort(err error, n int) (bool, error) {
	return err != nil && n > 3, err
}

//noalloc:root
func StopOnFlag(xs []string) error {
	for _, x := range xs {
		_, err := lengthOf(x)
		if stop, err := ends(err); stop {
			return err
		}
	}
	return nil
}

func markRows(x string) (bool, error) {
	if x == "" {
		return true, fmt.Errorf("no rows at %q", x)
	}
	return x[0] == '#', nil
}

//noalloc:root
func WriteMarked(xs []string) error {
	for _, x := range xs {
		if marked, err := markRows(x); marked {
			return err
		}
	}
	return nil
}

func markOrSkip(x string) (bool, error) {
	if x == "" {
		return false, fmt.Errorf("no rows to skip at %q", x)
	}
	return x[0] == '#', nil
}

//noalloc:root
func SkipUnmarked(xs []string) error {
	for _, x := range xs {
		if marked, err := markOrSkip(x); marked {
			return err
		}
	}
	return nil
}

//noalloc:root
func GoOnPastFlag(xs []string) error {
	for i, x := range xs {
		_, err := lengthOrZero(x)
		if stop, err := endsUnlessShort(err, i); stop {
			return err
		}
	}
	return nil
}
`)
	for _, c := range []struct {
		fn, src string
		want    bool
	}{
		{"fixture.lengthOf", `return 0,fmt.Errorf("empty at %q",x)`, false},
		{"fixture.lengthOrZero", `return 0,fmt.Errorf("empty at %q",x)`, true},
		{"fixture.markRows", `return true,fmt.Errorf("no rows at %q",x)`, false},
		{"fixture.markOrSkip", `return false,fmt.Errorf("no rows to skip at %q",x)`, true},
	} {
		if got := flagged(res, c.fn, c.src); got != c.want {
			t.Errorf("%s: %s flagged %v, want %v", c.fn, c.src, got, c.want)
		}
	}
}

// TestAResultTheCallerStopsOnEndsTheCall checks that a failure turned into a
// result, a bool returned or a bool field of a returned struct, ends the call
// when every caller stops on that result: by leaving its loop in a function
// that runs once, however long the rest of it, or by returning a result its
// own callers stop on. A caller
// that goes on past the result, or a result a call can change before it is
// returned, leaves the failure made on each turn.
func TestAResultTheCallerStopsOnEndsTheCall(t *testing.T) {
	res := checkWith(t, nil, `
type outcome struct {
	n   int
	bad bool
}

func compileQuery(q string) (int, error) {
	if q == "" {
		return 0, fmt.Errorf("empty query %q", q)
	}
	return len(q), nil
}

func runQuery(q string) outcome {
	var out outcome
	n, err := compileQuery(q)
	if err != nil {
		out.bad = true
		return out
	}
	out.n = n
	return out
}

//noalloc:root
func WalkQueries(qs []string) int {
	out := runQuery(qs[0])
	total := out.n
	for i := 1; !out.bad && i < len(qs); i++ {
		next := runQuery(qs[i])
		if next.bad {
			break
		}
		total += next.n
		out = next
	}
	return total
}

func compileTerm(q string) (int, error) {
	if q == "" {
		return 0, fmt.Errorf("empty term %q", q)
	}
	return len(q), nil
}

func termLen(q string) (int, bool) {
	n, err := compileTerm(q)
	if err != nil {
		return 0, false
	}
	return n, true
}

func termsLen(qs []string) (int, bool) {
	total := 0
	for _, q := range qs {
		n, ok := termLen(q)
		if !ok {
			return 0, false
		}
		total += n
	}
	return total, true
}

//noalloc:root
func SumTerms(groups [][]string) int {
	total := 0
	for _, g := range groups {
		n, ok := termsLen(g)
		if !ok {
			return -1
		}
		total += n
	}
	return total
}

func compileSkipped(q string) (int, error) {
	if q == "" {
		return 0, fmt.Errorf("empty skipped %q", q)
	}
	return len(q), nil
}

func runSkipped(q string) outcome {
	var out outcome
	n, err := compileSkipped(q)
	if err != nil {
		out.bad = true
		return out
	}
	out.n = n
	return out
}

func compileCleared(q string) (int, error) {
	if q == "" {
		return 0, fmt.Errorf("empty cleared %q", q)
	}
	return len(q), nil
}

func clearBad(o *outcome) { o.bad = false }

func runCleared(q string) outcome {
	var out outcome
	n, err := compileCleared(q)
	if err != nil {
		out.bad = true
		clearBad(&out)
		return out
	}
	out.n = n
	return out
}

func compileLong(q string) (int, error) {
	if q == "" {
		return 0, fmt.Errorf("empty long %q", q)
	}
	return len(q), nil
}

func runLong(q string) outcome {
	var out outcome
	n, err := compileLong(q)
	if err != nil {
		out.bad = true
		return out
	}
	out.n = n
	return out
}

//noalloc:root
func WalkLong(qs []string) int {
	total := 0
	for _, q := range qs {
		out := runLong(q)
		if out.bad {
			break
		}
		total += out.n
	}
	if total > 0 {
		total++
	}
	if total > 1 {
		total++
	}
	if total > 2 {
		total++
	}
	if total > 3 {
		total++
	}
	if total > 4 {
		total++
	}
	if total > 5 {
		total++
	}
	if total > 6 {
		total++
	}
	if total > 7 {
		total++
	}
	if total > 8 {
		total++
	}
	if total > 9 {
		total++
	}
	if total > 10 {
		total++
	}
	if total > 11 {
		total++
	}
	if total > 12 {
		total++
	}
	if total > 13 {
		total++
	}
	return total
}

//noalloc:root
func WalkCleared(qs []string) int {
	total := 0
	for _, q := range qs {
		out := runCleared(q)
		if out.bad {
			break
		}
		total += out.n
	}
	return total
}

//noalloc:root
func SkipBadQueries(qs []string) int {
	total := 0
	for _, q := range qs {
		out := runSkipped(q)
		if out.bad {
			continue
		}
		total += out.n
	}
	return total
}
`)
	for _, c := range []struct {
		fn, src string
		want    bool
	}{
		{"fixture.compileQuery", `return 0,fmt.Errorf("empty query %q",q)`, false},
		{"fixture.compileTerm", `return 0,fmt.Errorf("empty term %q",q)`, false},
		{"fixture.compileSkipped", `return 0,fmt.Errorf("empty skipped %q",q)`, true},
		{"fixture.compileCleared", `return 0,fmt.Errorf("empty cleared %q",q)`, true},
		{"fixture.compileLong", `return 0,fmt.Errorf("empty long %q",q)`, false},
	} {
		if got := flagged(res, c.fn, c.src); got != c.want {
			t.Errorf("%s: %s flagged %v, want %v", c.fn, c.src, got, c.want)
		}
	}
}

// TestAConstantErrorIsASentinel checks that comparing an error with a
// constant one, which nothing allocates, decides a branch as comparing it
// with a package-level sentinel does: the failure is not the constant.
func TestAConstantErrorIsASentinel(t *testing.T) {
	res := checkWith(t, nil, `
type numErr int

func (e numErr) Error() string { return "number" }

const errBig = numErr(1)

func countDigits(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("no digits in %q", s)
	}
	if len(s) > 9 {
		return 0, errBig
	}
	return len(s), nil
}

//noalloc:root
func CountDigits(xs []string) (int, error) {
	t := 0
	for _, x := range xs {
		n, err := countDigits(x)
		if err != nil && err != errBig {
			return 0, err
		}
		t += n
	}
	return t, nil
}
`)
	if flagged(res, "fixture.countDigits", `return 0,fmt.Errorf("no digits in %q",s)`) {
		t.Error("the error compared with a constant one is taken to be dropped")
	}
}
