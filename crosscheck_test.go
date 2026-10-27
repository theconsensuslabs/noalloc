package noalloc

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCrossCheckSeesTheFixturesAllocations runs the cross-check over the
// fixture. Nothing is left unexplained, and every line flagged for an
// allocation the compiled code makes through the runtime is one the check
// saw a call on and set aside only because it was flagged, so the call would
// be reported were the rule that flags it missing. The appends a comparison
// proves have room are set aside as reused memory.
func TestCrossCheckSeesTheFixturesAllocations(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	crossCheckFixture(t, dir)
}

// TestCrossCheckReadsAModuleThroughALink runs the same check on a copy of the
// fixture reached through a symbolic link, where the compiler and the loader
// name each file by a different path.
func TestCrossCheckReadsAModuleThroughALink(t *testing.T) {
	real := t.TempDir()
	for _, name := range []string{"go.mod", "fixture.go"} {
		b, err := os.ReadFile(filepath.Join("testdata", "fixture", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	crossCheckFixture(t, link)
}

func crossCheckFixture(t *testing.T, dir string) {
	t.Helper()
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		Allow: strings.Split(Allowlist, "\n"), Root: dir, LineOnly: []string{"response"}})
	if err != nil {
		t.Fatal(err)
	}
	x := res.CrossCheck
	for _, c := range x.Calls {
		t.Errorf("unexplained: %s    %s", c, c.Source)
	}
	for _, u := range x.Unmatched {
		t.Errorf("a compiled function with no SSA function: %s", u)
	}
	src, err := os.ReadFile(filepath.Join(dir, "fixture.go"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeReasons := []string{"map write", "append to a slice outside the Scratch", "append that may grow reused memory",
		"string concatenation", "make with a size known only at run time", "escapes", "go statement"}
	lines := strings.Split(string(src), "\n")
	// A finding in an inlined body is at the call, and its compiled code at
	// the body.
	inlined := map[int]bool{}
	for _, f := range res.Findings {
		if slices.ContainsFunc(f.Reasons, func(r string) bool { return strings.Contains(r, " in the inlined ") }) {
			inlined[f.Line] = true
		}
	}
	checked := 0
	for i, l := range lines {
		_, want, ok := strings.Cut(l, "// want: ")
		if !ok || inlined[i+1] {
			continue
		}
		for _, r := range runtimeReasons {
			if strings.Contains(want, r) {
				checked++
				if at := fmt.Sprintf("fixture.go:%d", i+1); !x.flagged[at] {
					t.Errorf("%s (%s): the compiled code makes no allocating runtime call the check saw", at, strings.TrimSpace(l))
				}
				break
			}
		}
	}
	if checked < 10 {
		t.Fatalf("control: only %d fixture lines want an allocation the runtime makes", checked)
	}
	if len(inlined) == 0 {
		t.Error("control: no finding in an inlined body")
	}
	if i := strings.Index(string(src), "put := func() { sink = boxed }"); i < 0 {
		t.Fatal("control: the fixture has no inlined closure")
	} else if at := fmt.Sprintf("fixture.go:%d", strings.Count(string(src)[:i], "\n")+1); !x.flagged[at] {
		t.Errorf("%s: the inlined closure's allocation is not set aside as found at its calls", at)
	}
	// The appends a comparison proves have room still compile a call that
	// grows the slice were it full.
	for _, want := range []string{"sc.buf = append(sc.buf, 'b', 'c')", "sc.buf = append(sc.buf, s...)"} {
		i := strings.Index(string(src), want)
		if i < 0 {
			t.Fatalf("control: the fixture has no line %q", want)
		}
		if at := fmt.Sprintf("fixture.go:%d", strings.Count(string(src)[:i], "\n")+1); !x.reused[at] {
			t.Errorf("%s (%s) is not set aside as reused memory a comparison shows has room", at, want)
		}
	}
	if x.Flagged != len(x.flagged) || x.Reused != len(x.reused) || x.Reused == 0 {
		t.Errorf("the counts a caller reads say %d flagged and %d reused lines, for %d and %d", x.Flagged, x.Reused, len(x.flagged), len(x.reused))
	}
}

func TestCompiledSymbolNamesTheFunctionAsSSANamesIt(t *testing.T) {
	for sym, want := range map[string]string{
		"p.F":                    "p.F",
		"p.F.func1":              "p.F$1",
		"p.F.func1.2":            "p.F$1$2",
		"p.(*T).M.func3":         "p.(*T).M$3",
		"p.(*T[go.shape.int]).M": "p.(*T).M",
		"p.Map[go.shape.string,go.shape.[]uint8]": "p.Map",
		"p.F.deferwrap1":              "p.F",
		"p.F.func2.gowrap1":           "p.F$2",
		"p.quote.func1#zkZmR3vA0mw=#": "p.quote$1",
		"p.(*T).M-fm":                 "",
		"p.init":                      "",
		"p.init.0":                    "",
		"p.init.func1":                "",
	} {
		if got := compiledSymbol(sym); got != want {
			t.Errorf("compiledSymbol(%q) = %q, want %q", sym, got, want)
		}
	}
}
