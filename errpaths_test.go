package noalloc

import (
	"fmt"
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
		if name, path, aliased := strings.Cut(p, " "); aliased {
			imp.WriteString("\t" + name + " \"" + path + "\"\n")
		} else {
			imp.WriteString("\t\"" + p + "\"\n")
		}
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

// errorTestShared is what the cases of the errors.Is tests use.
const errorTestShared = `
var errDrop = errors.New("drop")

type numErr int

func (e numErr) Error() string { return "number" }

const errConst = numErr(1)

func targetOf() error { return errDrop }

type matcher struct{}

func (matcher) Is(err, target error) bool { return err == target }

type plainErr struct{ s string }

func (e *plainErr) Error() string { return e.s }

type unwrapErr struct{ err error }

func (e *unwrapErr) Error() string { return "unwrap" }
func (e *unwrapErr) Unwrap() error { return e.err }

type isErr struct{ s string }

func (e *isErr) Error() string        { return e.s }
func (e *isErr) Is(target error) bool { return target == errDrop }

func wrapFormat() string { return "%q: %w" }

func sentinelOf(s string) error {
	if s == "" {
		return errDrop
	}
	return nil
}

func apply(f func() error) error { return f() }
`

// TestErrorsIsDecidesABranchUnlessTheFailureCanWrap checks each way of testing
// an error against each way of making one. A failure taken to end the call is
// not reported. errors.Is decides a branch as a comparison with the sentinel
// does only when no failure of the call can wrap it, since a wrapped sentinel
// that the branch drops is allocated on a path that goes on. Every other
// target, and a function that is not the standard errors.Is, decides nothing.
func TestErrorsIsDecidesABranchUnlessTheFailureCanWrap(t *testing.T) {
	const fails = "if err != nil {\n\t\t\treturn 0, err\n\t\t}\n"
	const drop = "{\n\t\t\tcontinue\n\t\t}\n" + fails
	const keep = "{\n\t\t\t" + fails + "\t\t\ttotal += n\n\t\t}\n"
	tests := []struct {
		name, src string
		// seesWrap is whether the test is true of a wrapped sentinel.
		seesWrap bool
		// undecided is whether the test leaves every failure dropped.
		undecided bool
	}{
		{name: "Eq", src: "if err == errDrop " + drop},
		{name: "Ne", src: "if err != errDrop " + keep},
		{name: "Is", src: "if errors.Is(err, errDrop) " + drop, seesWrap: true},
		{name: "NotIs", src: "if !errors.Is(err, errDrop) " + keep, seesWrap: true},
		{name: "NonNilNotIs", src: "if err != nil && !errors.Is(err, errDrop) {\n\t\t\treturn 0, err\n\t\t}\n", seesWrap: true},
		{name: "HeldIs", src: "match := errors.Is(err, errDrop)\n\t\tif match " + drop, seesWrap: true},
		{name: "HeldNotIs", src: "other := !errors.Is(err, errDrop)\n\t\tif other " + keep, seesWrap: true},
		{name: "AliasIs", src: "if stderrors.Is(err, errDrop) " + drop, seesWrap: true},
		{name: "ConstIs", src: "if errors.Is(err, errConst) " + drop, seesWrap: true},
		{name: "CallTarget", src: "if errors.Is(err, targetOf()) " + drop, undecided: true},
		{name: "NilTarget", src: "if errors.Is(err, nil) " + drop, undecided: true},
		{name: "OutsideTarget", src: "if errors.Is(err, strconv.ErrSyntax) " + drop, undecided: true},
		{name: "ShadowedIs", src: "errors := matcher{}\n\t\tif errors.Is(err, errDrop) " + drop, undecided: true},
		{name: "OtherPackageIs", src: "if localerrors.Is(err, errDrop) " + drop, undecided: true},
	}
	makes := []struct {
		name, body string
		wraps      bool
	}{
		{name: "Errorf", body: `return 0, fmt.Errorf("FAIL %q", s)`},
		{name: "New", body: `return 0, &plainErr{"FAIL" + s}`},
		{name: "ErrorfW", body: `return 0, fmt.Errorf("FAIL %q: %w", s, errDrop)`, wraps: true},
		{name: "ErrorfWInner", body: "if err := sentinelOf(s); err != nil {\n\t\treturn 0, fmt.Errorf(\"FAIL %q: %w\", s, err)\n\t}\n\treturn 0, nil", wraps: true},
		{name: "UnknownFormat", body: `return 0, fmt.Errorf(wrapFormat(), "FAIL"+s, errDrop)`, wraps: true},
		{name: "Join", body: "if s == \"\" {\n\t\treturn 0, fmt.Errorf(\"FAIL %q\", s)\n\t}\n\treturn 0, errors.Join(errDrop, errDrop)", wraps: true},
		{name: "UnwrapType", body: `return 0, &unwrapErr{errors.New("FAIL")}`, wraps: true},
		{name: "IsType", body: `return 0, &isErr{"FAIL" + s}`, wraps: true},
		{name: "Closure", body: "if s == \"\" {\n\t\treturn 0, fmt.Errorf(\"FAIL %q\", s)\n\t}\n\treturn 0, apply(func() error { return fmt.Errorf(\"%q: %w\", s, errDrop) })", wraps: true},
		{name: "HandedOutside", body: "if s == \"\" {\n\t\treturn 0, fmt.Errorf(\"FAIL %q\", s)\n\t}\n\tvar once sync.Once\n\tvar err error\n\tonce.Do(func() { err = fmt.Errorf(\"%q: %w\", s, errDrop) })\n\treturn 0, err", wraps: true},
		{name: "OutsideWrap", body: `return 0, os.NewSyscallError("FAIL", errDrop)`, wraps: true},
		{name: "OutsideUnwrap", body: "if s == \"\" {\n\t\treturn 0, fmt.Errorf(\"FAIL %q\", s)\n\t}\n\treturn 0, errors.Unwrap(errDrop)"},
	}
	var src strings.Builder
	src.WriteString(errorTestShared)
	for _, tc := range tests {
		for _, m := range makes {
			fmt.Fprintf(&src, `
func make%[1]s%[2]s(s string) (int, error) {
	if len(s) > 1 {
		return len(s), nil
	}
	%[3]s
}

//noalloc:root
func Root%[1]s%[2]s(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := make%[1]s%[2]s(x)
		%[4]s
		total += n
	}
	return total, nil
}
`, tc.name, m.name, m.body, tc.src)
		}
	}
	res := checkWith(t, []string{"os", "stderrors errors", "localerrors fixture/errors"}, src.String())
	for _, tc := range tests {
		for _, m := range makes {
			want := tc.undecided || tc.seesWrap && m.wraps
			name := "fixture.make" + tc.name + m.name
			got := slices.ContainsFunc(res.Findings, func(f Finding) bool {
				return (f.Func == name || strings.HasPrefix(f.Func, name+"$")) && strings.Contains(f.Source, "FAIL")
			})
			if got != want {
				t.Errorf("%s with %s: failure reported %v, want %v", tc.name, m.name, got, want)
			}
		}
	}
}

// TestErrorsIsFollowsTheCallsThatMakeTheError checks what errors.Is looks past
// to find a wrap: an argument a helper carries, every method an interface call
// can reach, and recursion. A parameter's failures come from callers it cannot
// see, so a test of one decides nothing. An allocation on the arm that handles
// the sentinel stays reported.
func TestErrorsIsFollowsTheCallsThatMakeTheError(t *testing.T) {
	res := checkWith(t, nil, errorTestShared+`
func madeWrapped(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("carried %q: %w", s, errDrop)
	}
	return len(s), nil
}

func pass(err error) error { return err }

//noalloc:root
func PassWrapped(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := madeWrapped(x)
		err = pass(err)
		if errors.Is(err, errDrop) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func madePlain(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("plain carried %q", s)
	}
	return len(s), nil
}

//noalloc:root
func PassPlain(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := madePlain(x)
		err = pass(err)
		if errors.Is(err, errDrop) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func wrapIt(err error) error {
	if err != nil {
		return fmt.Errorf("wrapped: %w", err)
	}
	return nil
}

func madeThenWrapped(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("then wrapped %q", s)
	}
	return len(s), nil
}

//noalloc:root
func WrapInHelper(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := madeThenWrapped(x)
		err = wrapIt(err)
		if errors.Is(err, errDrop) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

type maker interface{ Make(s string) (int, error) }

type plainMaker struct{}

func (plainMaker) Make(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("plain maker %q", s)
	}
	return len(s), nil
}

type wrapMaker struct{}

func (wrapMaker) Make(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("wrap maker %q: %w", s, errDrop)
	}
	return len(s), nil
}

//noalloc:root
func ThroughInterface(m maker, xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := m.Make(x)
		if errors.Is(err, errDrop) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func recurseA(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return recurseB(s[1:])
}

func recurseB(s string) (int, error) {
	if s == "x" {
		return 0, fmt.Errorf("recursed %q: %w", s, errDrop)
	}
	return recurseA(s)
}

//noalloc:root
func Recursive(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := recurseA(x)
		if errors.Is(err, errDrop) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func recursePlainA(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return recursePlainB(s[1:])
}

func recursePlainB(s string) (int, error) {
	if s == "x" {
		return 0, fmt.Errorf("recursed plain %q", s)
	}
	return recursePlainA(s)
}

//noalloc:root
func RecursivePlain(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := recursePlainA(x)
		if errors.Is(err, errDrop) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func dropIt(err error) error {
	if errors.Is(err, errDrop) {
		return nil
	}
	return err
}

func madeForParameter(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("parameter %q", s)
	}
	return len(s), nil
}

//noalloc:root
func TestedInHelper(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := madeForParameter(x)
		if err := dropIt(err); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func madeForArm(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("arm %q", s)
	}
	return len(s), nil
}

//noalloc:root
func AllocatesOnTheArm(xs []string) (string, error) {
	out := ""
	for _, x := range xs {
		_, err := madeForArm(x)
		if errors.Is(err, errDrop) {
			out = out + x
			continue
		}
		if err != nil {
			return "", err
		}
	}
	return out, nil
}
`)
	for _, c := range []struct {
		fn   string
		want bool
	}{
		{"fixture.madeWrapped", true},
		{"fixture.madePlain", false},
		{"fixture.madeThenWrapped", true},
		{"(fixture.plainMaker).Make", true},
		{"(fixture.wrapMaker).Make", true},
		{"fixture.recurseB", true},
		{"fixture.recursePlainB", false},
		{"fixture.madeForParameter", true},
		{"fixture.madeForArm", false},
		{"fixture.AllocatesOnTheArm", true},
	} {
		got := slices.ContainsFunc(res.Findings, func(f Finding) bool { return f.Func == c.fn })
		if got != c.want {
			t.Errorf("%s: reported %v, want %v", c.fn, got, c.want)
		}
	}
	if t.Failed() {
		for _, f := range res.Findings {
			t.Log(f.Func, f.Source)
		}
	}
}
