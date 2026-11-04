package noalloc

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// determinismModule is a module with what can reach a report in more than one
// order: calls through interfaces and function values with many callees, in the
// module and in a dependency, and a cycle between a dependency's functions, each
// way into it allocating for a reason of its own.
func determinismModule(t *testing.T) string {
	t.Helper()
	var impls, list strings.Builder
	const kinds = 8
	for i := range kinds {
		fmt.Fprintf(&impls, "type T%d struct{}\n\nfunc (T%d) Speak(n int) int { return len(make([]byte, n+%d)) }\n\n", i, i, i)
		fmt.Fprintf(&list, "T%d{}, ", i)
	}
	dir := t.TempDir()
	writeConversionFile(t, dir, "go.mod", "module callers\n\ngo 1.27\n\nrequire detdep v0.0.0\n\nreplace detdep => ./dep\n")
	writeConversionFile(t, dir, "callers.go", `package callers

import "detdep"

var local = detdep.Speakers

var funcs = []func(int) int{viaA, viaB, viaC, viaD, viaE, viaF}

func viaA(n int) int { return shared(n) + 1 }
func viaB(n int) int { return shared(n) + 2 }
func viaC(n int) int { return shared(n) + 3 }
func viaD(n int) int { return shared(n) + 4 }
func viaE(n int) int { return shared(n) + 5 }
func viaF(n int) int { return shared(n) + 6 }

func shared(n int) int { return len(make([]byte, n)) }

//noalloc:root
func Through(n int) int { return detdep.Talk(n) }

//noalloc:root
func Direct(n int) int { return local[n].Speak(n) }

//noalloc:root
func Funcs(n int) int { return funcs[n](n) }

//noalloc:root
func Around(n int) int { return detdep.Odd(n) }

//noalloc:root
func Inward(n int) int { return detdep.Even(n) }
`)
	writeConversionFile(t, filepath.Join(dir, "dep"), "go.mod", "module detdep\n\ngo 1.27\n")
	writeConversionFile(t, filepath.Join(dir, "dep"), "dep.go", `package detdep

type Speaker interface{ Speak(int) int }

`+impls.String()+`var Speakers = []Speaker{`+list.String()+`}

func Talk(n int) int { return Speakers[n].Speak(n) }

func Odd(n int) int {
	if n == 0 {
		return 0
	}
	return Even(n-1) + Speakers[n].Speak(n)
}

func Even(n int) int {
	if n == 0 {
		return len(make([]int, n))
	}
	return Odd(n - 1)
}

// Entered at First, Second finds Third's allocation with First still open.
// Entered at Second, First's own comes first.
func First(n int) int {
	x := 0
	if n > 0 {
		x = Second(n - 1)
	}
	return x + len(make([]byte, n))
}

func Second(n int) int {
	x := 0
	if n > 0 {
		x = First(n - 1)
	}
	return x + Third(n)
}

func Third(n int) int { return len(make([]byte, n)) }

// Handed reused memory, Grow still calls itself on memory that is not.
func Outer(b []byte, n int) []byte {
	b = append(b, 0)
	return Grow(b, n)
}

func Grow(b []byte, n int) []byte {
	if n > 0 {
		again(n)
	}
	return append(b, 1)
}

// Quiet would allocate, and is labeled not to. Calm allocates nothing but for
// it, whichever way around the cycle it is entered, and Loud allocates anyway.
func Quiet(n int) int { return len(make([]byte, n)) }

func Calm(n int) int {
	if n > 0 {
		return Calm2(n - 1)
	}
	return Quiet(n)
}

func Calm2(n int) int {
	if n > 0 {
		return Calm(n - 1)
	}
	return 0
}

func Loud(n int) int { return Quiet(n) + Third(n) }

func again(n int) {

	Grow(nil, n-1)
}
`)
	return dir
}

// checkedText is everything a check says of what it found, as text.
func checkedText(t *testing.T, res *Result) string {
	t.Helper()
	x, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	return Report(res) + "\n" + string(x)
}

// firstDifference shows where two texts first part.
func firstDifference(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range min(len(x), len(y)) {
		if x[i] != y[i] {
			return fmt.Sprintf("line %d:\n  %s\n  %s", i+1, x[i], y[i])
		}
	}
	return fmt.Sprintf("%d lines against %d", len(x), len(y))
}

// TestAReportIsTheSameEveryRun holds a check to one answer, in every word: run
// again with nothing cached and run again from its own cache, which walk maps in
// new orders each time. Eight callees give any one order a one in 40,320 chance
// of coming up twice.
func TestAReportIsTheSameEveryRun(t *testing.T) {
	dir := determinismModule(t)
	env := append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	check := func(cache string) (string, *Result) {
		res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Env: env, Root: dir,
			Allow: strings.Split(Allowlist, "\n"), SummaryCache: cache})
		if err != nil {
			t.Fatal(err)
		}
		return checkedText(t, res), res
	}
	cacheFile := func(cache string) string {
		files, err := filepath.Glob(filepath.Join(cache, "summaries-*.json"))
		if err != nil || len(files) != 1 {
			t.Fatalf("summary files %q: %v", files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first, res := check(t.TempDir())
	if len(res.Findings) < 5 {
		t.Fatalf("control: %d findings in %s", len(res.Findings), first)
	}
	var cachedFile string
	for run := range 3 {
		cache := t.TempDir()
		cold, _ := check(cache)
		if cold != first {
			t.Errorf("cold run %d differs from the first, at %s", run, firstDifference(first, cold))
		}
		if file := cacheFile(cache); cachedFile != "" && file != cachedFile {
			t.Errorf("cold run %d saved other summaries, at %s", run, firstDifference(cachedFile, file))
		} else {
			cachedFile = file
		}
		cached, _ := check(cache)
		if cached != cold {
			t.Errorf("run %d from its cache differs from its cold run, at %s", run, firstDifference(cold, cached))
		}
	}
}

// TestASummaryDoesNotDependOnWhatCameBefore holds each summary to one value
// however the names around it are ordered: alone, first, or last. A cache filled
// by one check and read by another mixes summaries worked out in different
// company, so an answer that depended on its company would change with the
// history of the cache.
func TestASummaryDoesNotDependOnWhatCameBefore(t *testing.T) {
	dir := determinismModule(t)
	env := append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	cfg := Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Env: env, Root: dir}
	pkgs, err := buildPackages(cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	archives := map[string]string{}
	for _, p := range pkgs {
		archives[p.path] = p.archive
	}
	names := []string{"detdep.Talk", "detdep.Odd", "detdep.Even", "(detdep.T0).Speak", "(detdep.T7).Speak",
		"detdep.First", "detdep.Second", "detdep.Third", "detdep.Outer", "detdep.Grow", "detdep.Loud", "detdep.Calm", "detdep.Calm2"}
	assumed := map[string]string{"detdep.Quiet": "measured"}
	calm := map[string]bool{"detdep.Calm": true, "detdep.Calm2": true}
	text := func(s summary) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	got, err := summarize(cfg, env, names, archives, assumed)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if got[n].Alloc[0] == calm[n] {
			t.Fatalf("control: %s allocating is %v: %s", n, got[n].Alloc[0], text(got[n]))
		}
		if relied := len(got[n].Assumed) > 0; relied != calm[n] {
			t.Errorf("%s relies on %q", n, got[n].Assumed)
		}
	}
	reversed := slices.Clone(names)
	slices.Reverse(reversed)
	orders := [][]string{reversed}
	for _, n := range names {
		orders = append(orders, []string{n})
	}
	for _, order := range orders {
		other, err := summarize(cfg, env, order, archives, assumed)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range order {
			if a, b := text(got[n]), text(other[n]); a != b {
				t.Errorf("%s worked out in the order %q differs:\n  %s\n  %s", n, order, a, b)
			}
		}
	}
}

// TestAJudgmentOfCompiledCodeDoesNotDependOnTheArchivesReadBeforeIt holds a
// function's verdict to the same answer whether or not another package's archive
// has been read, whose code calling it might say it never returns.
func TestAJudgmentOfCompiledCodeDoesNotDependOnTheArchivesReadBeforeIt(t *testing.T) {
	const own = `TEXT a.fails(SB) x.go
  x.go:5	0x20		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.mallocgc	
  x.go:6	0x24		d65f03c0		RET		
`
	const other = `TEXT b.caller(SB) y.go
  y.go:1	0x10		94000000		CALL 0(PC)		[0:4]R_CALLARM64:a.fails	
  y.go:1	0x14		00000000		?		
`
	read := func(paths ...string) *machineCode {
		m := newMachineCode(nil, nil)
		m.text["a"], m.text["b"], m.text["runtime"] = map[string]textSym{}, map[string]textSym{}, map[string]textSym{}
		m.parse("a", []byte(own), m.text["a"])
		for range paths {
			m.parse("b", []byte(other), m.text["b"])
		}
		return m
	}
	before, after := read().allocates("a.fails"), read("b").allocates("a.fails")
	if !before {
		t.Error("control: a function that reaches mallocgc allocates nothing")
	}
	if before != after {
		t.Errorf("a.fails allocates %v with only its own archive read and %v with another's read too", before, after)
	}
}

// TestAnInstanceHasOneNameHoweverItsTypesAreSpelled holds an instance's summary
// to one name across programs: the program names it after whichever of its calls
// it builds first, so a dependency's own call could rename what the module calls.
func TestAnInstanceHasOneNameHoweverItsTypesAreSpelled(t *testing.T) {
	name := func(arg string) string {
		t.Helper()
		src := "package p\n\nfunc F[T any](x T) T { return x }\n\nfunc G(x " + arg + ") { F(x) }\n"
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset, types.NewPackage("p", "p"), []*ast.File{f}, ssa.InstantiateGenerics)
		if err != nil {
			t.Fatal(err)
		}
		for fn := range ssautil.AllFunctions(pkg.Prog) {
			if fn.Origin() != nil {
				return summaryName(fn)
			}
		}
		t.Fatalf("no instance of F[%s]", arg)
		return ""
	}
	for _, spellings := range [][2]string{{"rune", "int32"}, {"byte", "uint8"}, {"any", "interface{}"},
		{"[]rune", "[]int32"}, {"map[string]byte", "map[string]uint8"}, {"func(any) rune", "func(interface{}) int32"}} {
		if a, b := name(spellings[0]), name(spellings[1]); a != b {
			t.Errorf("one instance has two names: %s and %s", a, b)
		}
	}
	if a, b := name("int32"), name("int64"); a == b {
		t.Errorf("control: two instances share the name %s", a)
	}
}
