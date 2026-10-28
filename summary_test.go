package noalloc

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// summaryCacheDir is shared by the tests that check with summaries, so the
// standard library is summarized once for all of them.
var summaryCacheDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "noalloc-summaries")
	if err != nil {
		panic(err)
	}
	summaryCacheDir, testSummaryCache = dir, dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// checkSummarized checks the fixture with src appended, imports added, and
// calls out of it judged by summaries.
func checkSummarized(t *testing.T, imports []string, src string) *Result {
	t.Helper()
	return checkSummarizedWith(t, imports, src, nil)
}

// checkSummarizedWith is checkSummarized with allow as the allowlist.
func checkSummarizedWith(t *testing.T, imports []string, src string, allow []string) *Result {
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
		LineOnly: []string{"response"}, SummaryCache: summaryCacheDir, Allow: allow,
		Root: dir, Overlay: map[string][]byte{file: []byte(over)}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

const summarizedSrc = `
//noalloc:root
func Summarized(b []byte, s string, n int, xs []string) (int, error) {
	i := bytes.IndexByte(b, '/')
	if strings.Contains(s, "x") {
		i++
	}
	if slices.Contains(xs, s) {
		i++
	}
	i += time.Now().Second()
	_ = maphash.MakeSeed()
	_ = strconv.Itoa(n)
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return i + v, nil
}

//noalloc:root
func SummarizedDropped(xs []string) int {
	t := 0
	for _, x := range xs {
		v, _ := strconv.Atoi(x)
		t += v
	}
	return t
}
`

// TestASummaryJudgesACallOut checks calls out of the module by what the
// called function's code allocates: bytes.IndexByte, whose assembly calls
// nothing, strings.Contains, an instance of slices.Contains, time.Now,
// whose clock the runtime reads through C, and maphash.MakeSeed, whose
// random source is the runtime's by go:linkname, allocate nothing;
// strconv.Itoa can; and
// strconv.Atoi allocates only on a failure, which ends the call when its
// error is returned and repeats when it is dropped in a loop.
func TestASummaryJudgesACallOut(t *testing.T) {
	res := checkSummarized(t, []string{"hash/maphash", "slices", "strings", "time"}, summarizedSrc)
	for _, c := range []struct {
		fn, src, reason string
	}{
		{"fixture.Summarized", "i:=bytes.IndexByte(b,'/')", ""},
		{"fixture.Summarized", `if strings.Contains(s,"x"){`, ""},
		{"fixture.Summarized", "if slices.Contains(xs,s){", ""},
		{"fixture.Summarized", "i+=time.Now().Second()", ""},
		{"fixture.Summarized", "_=maphash.MakeSeed()", ""},
		{"fixture.Summarized", "_=strconv.Itoa(n)", "calls strconv.Itoa"},
		{"fixture.Summarized", "v,err:=strconv.Atoi(s)", ""},
		{"fixture.SummarizedDropped", "v,_:=strconv.Atoi(x)", "calls strconv.Atoi"},
	} {
		i := slices.IndexFunc(res.Findings, func(f Finding) bool { return f.Func == c.fn && f.Source == c.src })
		var got []string
		if i >= 0 {
			got = res.Findings[i].Reasons
		}
		if c.reason == "" && len(got) > 0 || c.reason != "" && !slices.Contains(got, c.reason) {
			t.Errorf("%s: %s: reasons %q, want %q", c.fn, c.src, got, c.reason)
		}
	}
}

// TestSummariesAreCached checks that a second check of the same code reads
// every summary from the cache the first one wrote, a function with none to
// be found included.
func TestSummariesAreCached(t *testing.T) {
	var computed [][]string
	testHookSummarize = func(names []string) { computed = append(computed, names) }
	defer func() { testHookSummarize = nil }()
	checkSummarized(t, []string{"hash/maphash", "slices", "strings", "time"}, summarizedSrc)
	checkSummarized(t, []string{"hash/maphash", "slices", "strings", "time"}, summarizedSrc)
	if len(computed) != 2 || len(computed[1]) != 0 {
		t.Errorf("summarized %q, want the second check to summarize nothing", computed)
	}
}

// TestTheSummaryKeyFollowsTheBuild checks that summaries are kept under a
// key that changes with what compiles the packages outside the module, the
// platform and the compiler's flags here, and not with the module's own code,
// which summaries leave out.
func TestTheSummaryKeyFollowsTheBuild(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	key := func(env []string, overlay map[string][]byte) string {
		t.Helper()
		cfg := Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Overlay: overlay}
		pkgs, err := buildPackages(cfg, append(os.Environ(), env...))
		if err != nil {
			t.Fatal(err)
		}
		return buildKey(cfg, pkgs, nil)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	base := key(nil, nil)
	if k := key(nil, map[string][]byte{file: append(src, "\nfunc edited() {}\n"...)}); k != base {
		t.Error("editing the module changed the key")
	}
	other := "amd64"
	if runtime.GOARCH == other {
		other = "arm64"
	}
	if k := key([]string{"GOOS=" + runtime.GOOS, "GOARCH=" + other}, nil); k == base {
		t.Error("another platform kept the key")
	}
	if k := key([]string{"GOFLAGS=-gcflags=all=-l"}, nil); k == base {
		t.Error("other compiler flags kept the key")
	}
}

// TestALabeledFunctionIsTakenNotToAllocate checks that functions the
// allowlist labels as allocating only to debug or to profile, by name or by a
// pattern, are taken to allocate nothing by the summaries that reach them,
// whether worked out from source, as strconv.Itoa's is, or from compiled
// code, as the runtime's are; that the result says a summary relied on each
// label; and that a label nothing reaches is reported unused.
func TestALabeledFunctionIsTakenNotToAllocate(t *testing.T) {
	src := `
//noalloc:root
func Labeled(n int) string {
	runtime.GC()
	return strconv.Itoa(n)
}
`
	reasons := func(res *Result) []string {
		i := slices.IndexFunc(res.Findings, func(f Finding) bool { return f.Func == "fixture.Labeled" })
		if i < 0 {
			return nil
		}
		var out []string
		for _, f := range res.Findings {
			if f.Func == "fixture.Labeled" {
				out = append(out, f.Reasons...)
			}
		}
		return out
	}
	if r := reasons(checkSummarizedWith(t, []string{"runtime"}, src, nil)); !slices.Contains(r, "calls runtime.GC") || !slices.Contains(r, "calls strconv.Itoa") {
		t.Fatalf("control: with no labels, reasons %q", r)
	}
	res := checkSummarizedWith(t, []string{"runtime"}, src, []string{
		"debug: strconv.Itoa # only to show a label taken by name",
		"debug: runtime.G* # and by a pattern",
		"profiling: fmt.Println",
	})
	if r := reasons(res); len(r) > 0 {
		t.Errorf("with both labeled, reasons %q", r)
	}
	want := []Assumption{{"profiling", "fmt.Println", false}, {"debug", "runtime.G*", true}, {"debug", "strconv.Itoa", true}}
	if !slices.Equal(res.Assumptions, want) {
		t.Errorf("assumptions %+v, want %+v", res.Assumptions, want)
	}
	if r := Report(res); !strings.Contains(r, "profiling: fmt.Println (nothing relied on it)\n") || !strings.Contains(r, "debug: strconv.Itoa\n") {
		t.Errorf("the report does not say which lines were relied on:\n%s", r)
	}
}

// TestASummaryCountsAnInlinedCallsBoxedResult checks a dependency's function
// that boxes the result of a call the compiler inlines into it: the compiler
// reports that escape at the inlined call, and it is the function's own. It
// also checks that a closure's allocation, which the compiler reports where
// the closure starts, is the function that makes it, not the closure's, and
// that a call the compiler inlines is judged by what the compiler reports
// at the call.
func TestASummaryCountsAnInlinedCallsBoxedResult(t *testing.T) {
	dir, err := filepath.Abs("testdata/inlcaller")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "example.com/inlcaller", Root: dir,
		SummaryCache: summaryCacheDir})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range res.Findings {
		got[f.Source] = f.Reasons
	}
	if !slices.Contains(got["sink=inldep.Boxed(n)"], "calls example.com/inldep.Boxed") {
		t.Errorf("inldep.Boxed, which boxes its inlined call's result, is taken to allocate nothing; findings %q", got)
	}
	if r := got["return inldep.Unboxed(n)"]; len(r) > 0 {
		t.Errorf("inldep.Unboxed allocates: %q", r)
	}
	if !slices.Contains(got["inldep.Install(n)"], "calls example.com/inldep.Install") {
		t.Errorf("inldep.Install, which makes a closure, is taken to allocate nothing; findings %q", got)
	}
	if r := got["return inldep.Hook(n)"]; len(r) > 0 {
		t.Errorf("inldep.Hook, which only runs a closure, allocates: %q", r)
	}
	// A call the compiler inlines is judged without the callee's own
	// escapes, which it reports at the call: inldep.Values's closure and
	// newCounter's counter stay on the stack of the code they are inlined
	// into, in the module and in inldep alike.
	for _, src := range []string{"for x:=range inldep.Values(xs){", "return inldep.Total(xs)", "return inldep.Count(b)"} {
		if r := got[src]; len(r) > 0 {
			t.Errorf("%s allocates: %q", src, r)
		}
	}
}

// TestAGenericCallInAGenericBodyIsSummarized checks a call, in a generic
// function or a method of a generic type, to a generic function outside the
// module instantiated with the caller's own type parameter: it is judged by
// that function's generic body, which holds for every type argument.
func TestAGenericCallInAGenericBodyIsSummarized(t *testing.T) {
	res := checkSummarized(t, []string{"slices"}, `
//noalloc:root
func HasAny[T comparable](xs []T, x T) bool {
	return slices.Contains(xs, x)
}

type tset[T comparable] struct{ xs []T }

//noalloc:root
func (s *tset[T]) Has(x T) bool {
	return slices.Contains(s.xs, x)
}
`)
	for _, f := range res.Findings {
		if strings.HasPrefix(f.Func, "fixture.HasAny") || strings.Contains(f.Func, "fixture.tset[") {
			t.Errorf("allocates: %s", f)
		}
	}
	if !slices.ContainsFunc(res.Roots, func(r string) bool { return strings.HasPrefix(r, "fixture.HasAny") }) {
		t.Errorf("HasAny is not a root; roots %q", res.Roots)
	}
}

// TestAGenericMethodsCallIsSummarizedFromItsBody checks bounded.Map's call,
// in a method of the generic type, to maphash.Comparable instantiated with
// the type's own parameter: its summary is worked out from Comparable's
// generic body, not taken to allocate for want of code.
func TestAGenericMethodsCallIsSummarizedFromItsBody(t *testing.T) {
	dir, err := filepath.Abs("bounded")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: boundedModule, Root: dir,
		SummaryCache: summaryCacheDir})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Findings, func(f Finding) bool {
		return strings.HasSuffix(f.Func, ".home") && slices.Contains(f.Reasons, "calls hash/maphash.Comparable")
	})
	if i < 0 {
		t.Fatal("no finding for Map.home's call to maphash.Comparable")
	}
	if why := res.Findings[i].Why["calls hash/maphash.Comparable"]; !strings.HasPrefix(why, "hash/maphash.Comparable calls ") {
		t.Errorf("maphash.Comparable[K] is not summarized from its body: %q", why)
	}
}

// TestAppendFunctionsKeepReusedMemory checks standard library functions
// shaped like append whose code grows what they append to through helpers
// shaped like it, or by making a longer slice and copying into it: called
// with reused memory, each may only grow it, and what is appended after
// them, or after one that can allocate elsewhere too, is still reused
// memory.
func TestAppendFunctionsKeepReusedMemory(t *testing.T) {
	res := checkSummarized(t, []string{"net/netip", "time"}, `
//noalloc:root
func AppendStd(sc *Scratch, ip netip.Addr, f float64, q string, tm time.Time) {
	sc.buf = ip.AppendTo(sc.buf)
	sc.buf = strconv.AppendFloat(sc.buf, f, 'f', 6, 64)
	sc.buf = strconv.AppendQuote(sc.buf, q)
	b := tm.AppendFormat(sc.buf, time.RFC1123)
	b = append(b, '.')
	sc.buf = b
}
`)
	for _, c := range []struct{ src, fn string }{
		{"sc.buf=ip.AppendTo(sc.buf)", "(net/netip.Addr).AppendTo"},
		{"sc.buf=strconv.AppendFloat(sc.buf,f,'f',6,64)", "strconv.AppendFloat"},
		{"sc.buf=strconv.AppendQuote(sc.buf,q)", "strconv.AppendQuote"},
	} {
		i := slices.IndexFunc(res.Findings, func(f Finding) bool { return f.Func == "fixture.AppendStd" && f.Source == c.src })
		var got []string
		if i >= 0 {
			got = res.Findings[i].Reasons
		}
		if !slices.Equal(got, []string{"calls " + c.fn + ", which may grow reused memory"}) {
			t.Errorf("%s: reasons %q, want only that it may grow reused memory", c.src, got)
		}
	}
	// AppendFormat can allocate elsewhere, naming a month out of range, and
	// still returns what it appends to.
	for _, f := range res.Findings {
		if f.Func == "fixture.AppendStd" && f.Source == "b=append(b,'.')" && slices.Contains(f.Reasons, "append to a slice outside the Scratch") {
			t.Errorf("what AppendFormat returns is not reused memory: %s", f)
		}
	}
}

// TestAMethodCallImplementedOutsideTheModuleIsSummarized checks a call of an
// interface method that no type of the module implements: it is judged by
// every implementation outside the module, so context.Context's Err, which
// none allocates in past a lock noalloc's allowlist takes as amortized, is no
// finding, and fmt.Stringer's String, which some build a string in, is one.
func TestAMethodCallImplementedOutsideTheModuleIsSummarized(t *testing.T) {
	res := checkSummarizedWith(t, []string{"context"}, `
//noalloc:root
func CtxErr(ctx context.Context) error {
	return ctx.Err()
}

//noalloc:root
func Describe(s fmt.Stringer) string {
	return s.String()
}
`, strings.Split(Allowlist, "\n"))
	got := map[string][]string{}
	for _, f := range res.Findings {
		got[f.Source] = f.Reasons
	}
	if r := got["return ctx.Err()"]; len(r) > 0 {
		t.Errorf("ctx.Err() allocates: %q", r)
	}
	if r := got["return s.String()"]; !slices.Contains(r, "calls fmt.Stringer.String") {
		t.Errorf("s.String() reasons %q, want that it calls fmt.Stringer.String", r)
	}
}

// TestAnEscapeReplayedFromAnotherDirectoryCounts checks an escape in code
// outside the module whose report the build cache replays from a compile
// another directory ran, which printed its path relative to that directory.
func TestAnEscapeReplayedFromAnotherDirectoryCounts(t *testing.T) {
	env := append(os.Environ(), "GOCACHE="+t.TempDir(), "GOFLAGS=", "GOTOOLCHAIN="+runtime.Version())
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-gcflags=crypto/internal/fips140/sha256=-m", "-o", os.DevNull, "crypto/sha256")
	build.Dir, build.Env = filepath.Dir(strings.TrimSpace(string(goroot))), env
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m)^[^/\s][^:]*sha256\.go:\d+:\d+: new\(Digest\) escapes to heap$`).Match(out) {
		t.Fatalf("control: the compile reported no escape at a relative path:\n%s", out)
	}
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	over := strings.Replace(string(b), "import (\n", "import (\n\t\"crypto/sha256\"\n\t\"hash\"\n", 1) + `
//noalloc:root
func NewHash() hash.Hash {
	return sha256.New()
}
`
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		SummaryCache: t.TempDir(), Env: env, Root: dir, Overlay: map[string][]byte{file: []byte(over)}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Findings, func(f Finding) bool {
		return f.Func == "fixture.NewHash" && slices.Contains(f.Reasons, "calls crypto/sha256.New")
	}) {
		t.Error("crypto/sha256.New, which allocates its digest, is taken to allocate nothing")
	}
}

// checkAsmmod checks testdata/asmmod with allow as the allowlist.
func checkAsmmod(t *testing.T, allow []string) *Result {
	t.Helper()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("asmmod's assembly is written for amd64 and arm64")
	}
	dir, err := filepath.Abs("testdata/asmmod")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "example.com/asmmod", Root: dir,
		SummaryCache: summaryCacheDir, Allow: allow})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestEveryKindOfCalleeIsJudgedAndCanBeOverridden checks a call to each kind
// of callee: the module's Go, its assembly, and its functions with no body
// bound by //go:linkname; Go and assembly outside the module; an interface
// method no type of the module implements; and a function value. Each that
// can allocate is a finding with no allowlist line and none with one, which
// is then reported relied on. The module's Go is checked where it is, so a
// line naming it changes nothing.
func TestEveryKindOfCalleeIsJudgedAndCanBeOverridden(t *testing.T) {
	cases := []struct {
		root, label string
		allocates   bool
		// overridable is whether a line naming the callee clears it.
		overridable bool
	}{
		{"ModuleGoAdd", "example.com/asmmod.goAdd", false, false},
		{"ModuleGoAlloc", "example.com/asmmod.goAlloc", true, false},
		{"ModuleAsmAdd", "example.com/asmmod.addAsm", false, true},
		{"ModuleAsmAlloc", "example.com/asmmod.allocAsm", true, true},
		{"LinkedAdd", "example.com/asmmod.addLinked", false, true},
		{"LinkedAlloc", "example.com/asmmod.allocLinked", true, true},
		{"OutsideGoAlloc", "strconv.Itoa", true, true},
		{"OutsideGoAdd", "strings.IndexByte", false, true},
		{"OutsideAsmAdd", "sync/atomic.AddInt32", false, true},
		{"InterfaceAlloc", "io.Writer.Write", true, true},
		{"InterfaceAdd", "hash.Hash32.Sum32", false, true},
		{"FuncValue", "f", true, true},
	}
	var allow []string
	for _, c := range cases {
		allow = append(allow, "measured: "+c.label)
	}
	plain, labeled := checkAsmmod(t, nil), checkAsmmod(t, allow)
	// reached is whether a finding is in the root or in what it calls of the
	// module's own Go.
	reached := func(res *Result, root string) []string {
		var out []string
		for _, f := range res.Findings {
			if slices.Contains(f.Path, "example.com/asmmod."+root) {
				out = append(out, f.String())
			}
		}
		return out
	}
	used := map[string]bool{}
	for _, a := range labeled.Assumptions {
		used[a.Func] = a.Used
	}
	for _, c := range cases {
		if got := reached(plain, c.root); len(got) > 0 != c.allocates {
			t.Errorf("%s with no allowlist line: findings %q, want a finding %v", c.root, got, c.allocates)
		}
		want := c.allocates && !c.overridable
		if got := reached(labeled, c.root); len(got) > 0 != want {
			t.Errorf("%s with %q allowlisted: findings %q, want a finding %v", c.root, c.label, got, want)
		}
		if got, want := used[c.label], c.allocates && c.overridable; got != want {
			t.Errorf("%s: %q reported relied on %v, want %v", c.root, c.label, got, want)
		}
	}
	// A closure handed to a function with no Go body can be called by it.
	if !slices.ContainsFunc(plain.Findings, func(f Finding) bool { return strings.HasPrefix(f.Func, "example.com/asmmod.LinkedRun$") }) {
		t.Errorf("the closure LinkedRun hands runLinked is not checked; findings %v", plain.Findings)
	}
}

// TestALineIsReliedOnOnlyByTheCheckThatReachesIt checks that an allowlist
// line one check of a build relied on is reported unused by another check
// of the same build, with the summaries the first saved, that reaches
// nothing it covers.
func TestALineIsReliedOnOnlyByTheCheckThatReachesIt(t *testing.T) {
	allow := []string{"measured: strconv.QuoteToASCII"}
	used := func(res *Result) bool {
		for _, a := range res.Assumptions {
			if a.Func == "strconv.QuoteToASCII" {
				return a.Used
			}
		}
		t.Fatalf("no assumption for strconv.QuoteToASCII in %+v", res.Assumptions)
		return false
	}
	if !used(checkSummarizedWith(t, nil, `
//noalloc:root
func Quoted(s string) string {
	return strconv.QuoteToASCII(s)
}
`, allow)) {
		t.Fatal("control: the check that calls strconv.QuoteToASCII does not rely on its line")
	}
	if used(checkSummarizedWith(t, nil, `
var quote = strconv.QuoteToASCII
`, allow)) {
		t.Error("a check that calls nothing the line covers relies on it")
	}
}

// TestChecksSavingAtOnceLeaveAWholeCache checks that saves of one build's
// summaries at once all succeed and leave a file that parses.
func TestChecksSavingAtOnceLeaveAWholeCache(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("a", 64)
	errs := make(chan error, 16)
	for i := range 16 {
		go func() {
			c, err := openSummaryCache(Config{SummaryCache: dir}, key)
			if err == nil {
				c.put(strconv.Itoa(i), summary{Shaped: true})
				err = c.save()
			}
			errs <- err
		}()
	}
	for range 16 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	c, err := openSummaryCache(Config{SummaryCache: dir}, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.file.Summaries) == 0 {
		t.Error("the cache file holds no summary")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.json.*")); len(left) > 0 {
		t.Errorf("temporary files left: %q", left)
	}
}

// TestEveryLineCoveringACallIsReliedOn checks that when two allowlist
// lines, a name and a pattern, cover one call that would allocate, each is
// reported relied on, in a call judged by a summary and in one judged
// where it is made.
func TestEveryLineCoveringACallIsReliedOn(t *testing.T) {
	allow := []string{"measured: strconv.QuoteToASCII", "measured: strconv.QuoteTo*",
		"measured: io.Writer.Write", "measured: io.Writer.*"}
	for range 8 {
		res := checkSummarizedWith(t, []string{"io"}, `
//noalloc:root
func QuotedTo(w io.Writer, s string) string {
	w.Write(nil)
	return strconv.QuoteToASCII(s)
}
`, allow)
		for _, a := range res.Assumptions {
			if !a.Used {
				t.Fatalf("%s: %s is not reported relied on", a.Kind, a.Func)
			}
		}
	}
}

// TestACgoPackageIsSummarized checks a module with a package that imports
// "C", whose compiled files import packages its source does not, as
// runtime/cgo and syscall: summarizing builds them all.
func TestACgoPackageIsSummarized(t *testing.T) {
	if out, err := exec.Command("go", "env", "CGO_ENABLED", "CC").Output(); err != nil {
		t.Fatal(err)
	} else if env := strings.Fields(string(out)); len(env) < 2 || env[0] != "1" {
		t.Skip("cgo is disabled")
	} else if _, err := exec.LookPath(env[1]); err != nil {
		t.Skipf("no C compiler: %v", err)
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module repro\n\ngo 1.27\n",
		"c/c.go": "package c\n\n// static int two(void) { return 2; }\nimport \"C\"\n\nfunc Two() int { return int(C.two()) }\n",
		"root/root.go": `package root

import (
	"repro/c"
	"strconv"
)

//noalloc:root
func Root(n int) int {
	return len(strconv.Itoa(n)) + c.Two()
}
`,
	}
	for name, src := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"./root"}, Module: "repro", SummaryCache: summaryCacheDir, Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Findings, func(f Finding) bool {
		return f.Func == "repro/root.Root" && slices.Contains(f.Reasons, "calls strconv.Itoa")
	}) {
		t.Errorf("strconv.Itoa is not judged by its summary: %v", res.Findings)
	}
}
