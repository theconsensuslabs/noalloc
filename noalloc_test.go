package noalloc

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestFixtureFlagsEachAllocationAndNothingElse checks the fixture: every
// line with a want comment is flagged for each reason it names, and no other
// line is, so error paths, panics, lazy setup, sync.Once, Scratch reuse,
// copies the compiler avoids, allowlisted calls, roots shaped like append,
// exemptions, functions VTA rules out, and unreached functions all go
// unflagged.
func TestFixtureFlagsEachAllocationAndNothingElse(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		LineOnly: []string{"response"},
		Allow:    strings.Split(Allowlist, "\n"), Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "fixture.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]string{}
	for i, l := range strings.Split(string(src), "\n") {
		if _, w, ok := strings.Cut(l, "// want: "); ok {
			want[i+1] = strings.Split(w, ", ")
		}
	}
	got := map[int]Finding{}
	for _, f := range res.Findings {
		got[f.Line] = f
	}
	for line, reasons := range want {
		f, ok := got[line]
		if !ok {
			t.Errorf("fixture.go:%d not flagged; want %q", line, reasons)
			continue
		}
		for _, w := range reasons {
			if !slices.ContainsFunc(f.Reasons, func(r string) bool { return strings.Contains(r, w) }) {
				t.Errorf("fixture.go:%d flagged for %q; want a reason containing %q", line, f.Reasons, w)
			}
		}
		for _, r := range f.Reasons {
			if !slices.ContainsFunc(reasons, func(w string) bool { return strings.Contains(r, w) }) {
				t.Errorf("fixture.go:%d also flagged for %q", line, r)
			}
		}
	}
	for line, f := range got {
		if _, ok := want[line]; !ok {
			t.Errorf("fixture.go:%d flagged but should not be: %s (path %s)", line, f, strings.Join(f.Path, " > "))
		}
	}
	if want := []string{"fixture.Root", "fixture.AppendDigits", "fixture.AppendTwice", "(fixture.digits).AppendTo", "fixture.GenRoot", "fixture.CompareLengths"}; !slices.Equal(res.Roots, want) {
		t.Errorf("roots %q, want %q", res.Roots, want)
	}
	var keys []string
	for _, e := range res.Exemptions {
		keys = append(keys, e.Key())
	}
	wantKeys := []string{
		"fixture.go: exempt: a trailing exemption",
		"fixture.go: exempt: an exemption standing alone covers the next line",
		"fixture.go: exempt: and stops the walk into what it calls",
		"fixture.go: exempt: response: a kind the config checks the calls of",
	}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("exemptions %q, want %q", keys, wantKeys)
	}
}

// TestBaselineCountsEachRepeat checks the baseline as a multiset: a reason
// two lines of one function share is listed twice, a third such line is new,
// and a listed line nothing matches is stale.
func TestBaselineCountsEachRepeat(t *testing.T) {
	l := func(fn, reason string) Line { return Line{Owner: "search", File: "a.go", Func: fn, Reason: reason} }
	base := []Line{l("f", "map write"), l("f", "map write"), l("g", "go statement")}
	found := []Line{l("f", "map write"), l("f", "map write"), l("f", "map write"), l("h", "make chan")}
	added, stale := diffLines(found, base)
	if want := []Line{l("f", "map write"), l("h", "make chan")}; !slices.Equal(added, want) {
		t.Errorf("added %v, want %v", added, want)
	}
	if want := []Line{l("g", "go statement")}; !slices.Equal(stale, want) {
		t.Errorf("stale %v, want %v", stale, want)
	}
}

func TestFoldShapesMakesOneMessagePerLine(t *testing.T) {
	for in, want := range map[string]string{
		"&Entry[go.shape.string,go.shape.[]uint8]{...} escapes to heap":                "&Entry[...]{...} escapes to heap",
		"&Cache[go.shape.struct { a [16]uint8 },go.shape.*uint8]{...} escapes to heap": "&Cache[...]{...} escapes to heap",
		"x escapes to heap": "x escapes to heap",
	} {
		if got := foldShapes(in); got != want {
			t.Errorf("foldShapes(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExemptionsReadTheOverlay checks that an exemption added by an overlay
// applies where the overlay puts it: a standalone one to the line after it,
// and a trailing one to its own line, which the overlay made longer than the
// line on disk.
func TestExemptionsReadTheOverlay(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const itoa = "\t_ = strconv.Itoa(n)       // want: calls strconv.Itoa\n"
	const impl = "func (impl) write(n int) { _ = strconv.Itoa(n) } // want: calls strconv.Itoa\n"
	over := strings.Replace(string(src), itoa, "\t//noalloc:exempt setup: an exemption the overlay adds\n"+itoa, 1)
	over = strings.Replace(over, impl, "func (impl) write(n int) { _ = strconv.Itoa(n); _ = n + n + n + n + n + n + n + n + n + n + n + n } "+
		"//noalloc:exempt cursor: a trailing one past the disk line's end\n", 1)
	if over == string(src) || strings.Count(over, "//noalloc:exempt ") != strings.Count(string(src), "//noalloc:exempt ")+2 {
		t.Fatal("the overlay changed nothing; the fixture lines it edits moved")
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		LineOnly: []string{"response"},
		Allow:    strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(over)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings {
		if f.Func == "fixture.calls" && slices.Contains(f.Reasons, "calls strconv.Itoa") {
			t.Errorf("the overlay's standalone exemption missed its line: %s", f)
		}
		if f.Func == "(fixture.impl).write" {
			t.Errorf("the overlay's trailing exemption missed its line: %s", f)
		}
	}
}

// TestAStrayDirectiveIsAnError checks that a //noalloc: comment the checker
// would not act on stops the check: a misspelled name, and each mark on a
// declaration that does not take it.
func TestAStrayDirectiveIsAnError(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, add, mark string }{
		{"misspelled root", "//noalloc:roots\nfunc Stray() {}\n", "//noalloc:roots"},
		{"misspelled exempt", "func Stray() {\n\t//noalloc:exemption cursor: x\n}\n", "//noalloc:exemption cursor: x"},
		{"fixed on a function", "//noalloc:fixed\nfunc Stray() {}\n", "//noalloc:fixed"},
		{"root on a type", "//noalloc:root\ntype Stray struct{}\n", "//noalloc:root"},
		{"append on a variable", "//noalloc:append\nvar Stray int\n", "//noalloc:append"},
		{"root inside a body", "func Stray() {\n\t//noalloc:root\n}\n", "//noalloc:root"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
				Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(string(src) + "\n" + c.add)}})
			if err == nil || !strings.Contains(err.Error(), "fixture.go:") || !strings.Contains(err.Error(), c.mark) {
				t.Errorf("err = %v; want it to name %q at its line", err, c.mark)
			}
		})
	}
}

// TestAFindingBelongsToTheFirstGroupThatReachesIt checks root groups: a
// function both groups reach is the first group's, and one only the second
// reaches is the second's.
func TestAFindingBelongsToTheFirstGroupThatReachesIt(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// A second root that calls newT, which Root reaches too, and Unreached,
	// which only it reaches.
	over := string(src) + "\n//noalloc:root\nfunc Second(n int) { global = newT(); _ = Unreached(n) }\n"
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		LineOnly: []string{"response"},
		Allow:    strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(over)},
		Groups: []Group{{"first", []string{"fixture.Root"}}, {"second", []string{"fixture.Second"}}}})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]string{}
	for _, f := range res.Findings {
		groups[f.Func] = f.Group
	}
	if g := groups["fixture.newT"]; g != "first" {
		t.Errorf("newT, which both roots reach, is in group %q; want first", g)
	}
	if g := groups["fixture.Unreached"]; g != "second" {
		t.Errorf("Unreached, which only Second reaches, is in group %q; want second", g)
	}
}

// TestMemoAnswersTheSameInAnyOrder asks "do all of a node's successors hold"
// over a graph where a and b reach each other, b also reaches d (true), and a
// also reaches c (false). Neither a nor b holds. Caching b's answer while a is
// still open would make b hold when a is asked first.
func TestMemoAnswersTheSameInAnyOrder(t *testing.T) {
	next := map[string][]string{"a": {"b", "c"}, "b": {"a", "d"}}
	leaf := map[string]bool{"c": false, "d": true}
	for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
		var m memo[string]
		var holds func(n string) bool
		holds = func(n string) bool {
			if v, ok := leaf[n]; ok {
				return v
			}
			return m.get(n, true, func() bool {
				all := true
				for _, s := range next[n] {
					all = holds(s) && all
				}
				return all
			})
		}
		for _, n := range order {
			if holds(n) {
				t.Errorf("asking %v: %s holds; want neither a nor b to", order, n)
			}
		}
	}
}

// TestALineAddedAboveAnAllocationKeepsItsKey checks that code added above an
// allocation leaves its baseline key alone: a declaration at the top of the
// file moves every line, a statement at the top of a function renumbers the
// SSA registers of the calls below it, and a closure at the top of another
// function renumbers the closures below it.
func TestALineAddedAboveAnAllocationKeepsItsKey(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	run := func(overlay map[string][]byte) []Line {
		t.Helper()
		res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
			LineOnly: []string{"response"},
			Allow:    strings.Split(Allowlist, "\n"), Root: dir, Overlay: overlay})
		if err != nil {
			t.Fatal(err)
		}
		var out []Line
		for _, f := range res.Findings {
			for _, r := range f.Reasons {
				out = append(out, Line{File: f.File, Func: f.Func, Reason: r, Source: f.Source, Number: f.Line})
			}
		}
		return out
	}
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(src), "\ntype Scratch struct {", "\nvar shifted int\n\ntype Scratch struct {", 1)
	edited = strings.Replace(edited, "func funcValues(n int) {\n", "func funcValues(n int) {\n\tn = n*3 + shifted\n", 1)
	edited = strings.Replace(edited, "\tf := pick(n)\n", "\t_ = func() int { return shifted }\n\tf := pick(n)\n", 1)
	if edited == string(src) {
		t.Fatal("control: the edit changed nothing")
	}
	before, after := run(nil), run(map[string][]byte{file: []byte(edited)})
	if before[0].Number == after[0].Number {
		t.Fatalf("control: the first finding stayed on line %d", before[0].Number)
	}
	added, stale := diffLines(after, before)
	for _, e := range added {
		t.Errorf("after the edit, a new key: %s", e.Key())
	}
	for _, e := range stale {
		t.Errorf("after the edit, a key gone: %s", e.Key())
	}
}

// A line that leaves one baseline and turns up in another is one move, not a
// new line and a stale one, and it keeps its owner; a line new to both stays
// new, and a stale one stale.
func TestALineThatChangesGroupIsAMove(t *testing.T) {
	e := func(owner, fn string) Line {
		return Line{Owner: owner, File: "a.go", Func: fn, Reason: "map write", Source: "m[k]=v"}
	}
	added := [][]Line{{e("storage", "f"), e("storage", "g")}, nil}
	stale := [][]Line{nil, {e("storage, moved", "f"), e("storage", "h")}}
	moved := moves(added, stale)
	if len(moved) != 1 || moved[0].From != 1 || moved[0].To != 0 || moved[0].Line.Func != "f" || moved[0].Line.Owner != "storage, moved" {
		t.Errorf("moves %+v, want f from 1 to 0 owned by storage, moved", moved)
	}
	if len(added[0]) != 1 || added[0][0].Func != "g" || len(stale[1]) != 1 || stale[1][0].Func != "h" {
		t.Errorf("left added %+v and stale %+v, want g and h", added, stale)
	}
}

// A statement whose text changed keeps its allocations: -restate pairs its
// new line with its stale one, same function and reason, and leaves the rest.
func TestAStatementWhoseTextChangedIsRestated(t *testing.T) {
	e := func(fn, reason, source string) Line {
		return Line{Owner: "storage", File: "a.go", Func: fn, Reason: reason, Source: source}
	}
	added := []Line{e("f", "map write", "h[k]=v"), e("f", "make chan", "make(chan int)"), e("g", "map write", "x")}
	stale := []Line{e("f", "map write", "h.Set(k,v)"), e("h", "map write", "y")}
	pairs := restated(&added, &stale)
	if len(pairs) != 1 || pairs[0][0].Source != "h.Set(k,v)" || pairs[0][1].Source != "h[k]=v" {
		t.Errorf("pairs %+v, want h.Set(k,v) restated as h[k]=v", pairs)
	}
	if len(added) != 2 || len(stale) != 1 || stale[0].Func != "h" {
		t.Errorf("left added %+v and stale %+v", added, stale)
	}
}

// TestAnExemptionReportsWhatItCovers checks that each exemption lists what it
// lets through: the allocations of a function only exempt lines call, under
// each of them, its own line's, and nothing, with Reached set, for one that
// exempts nothing.
func TestAnExemptionReportsWhatItCovers(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	add := `
//noalloc:root
func Covered(n int) []byte {
	//noalloc:exempt setup: covers a helper
	coveredHelper(n)
	//noalloc:exempt setup: covers nothing
	n++
	//noalloc:exempt setup: covers its own line
	return make([]byte, n)
}

//noalloc:root
func AlsoCovered(n int) {
	//noalloc:exempt setup: covers the same helper
	coveredHelper(n)
}

func coveredHelper(n int) { _ = strconv.Itoa(n) }
`
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(string(src) + add)}})
	if err != nil {
		t.Fatal(err)
	}
	byReason := map[string]Exemption{}
	for _, e := range res.Exemptions {
		byReason[e.Reason] = e
	}
	for _, f := range res.Findings {
		if f.Func == "fixture.coveredHelper" || f.Func == "fixture.Covered" {
			t.Errorf("an exempt allocation is a finding: %s", f)
		}
	}
	if e := byReason["setup: covers a helper"]; !e.Reached || !slices.Contains(e.Covers, "fixture.coveredHelper: calls strconv.Itoa") {
		t.Errorf("the helper's exemption covers %q, reached %v; want the helper's strconv.Itoa", e.Covers, e.Reached)
	}
	// Two exemptions over calls to one helper each cover it: either one alone
	// keeps its allocation from being a finding.
	if e := byReason["setup: covers the same helper"]; !slices.Contains(e.Covers, "fixture.coveredHelper: calls strconv.Itoa") {
		t.Errorf("the second exemption over the helper covers %q; want the helper's strconv.Itoa", e.Covers)
	}
	if e := byReason["setup: covers nothing"]; !e.Reached || len(e.Covers) != 0 {
		t.Errorf("the empty exemption covers %q, reached %v; want nothing, reached", e.Covers, e.Reached)
	}
	if e := byReason["setup: covers its own line"]; !e.Reached || len(e.Covers) == 0 {
		t.Errorf("the line's exemption covers %q, reached %v; want its make", e.Covers, e.Reached)
	} else {
		t.Logf("the line's exemption covers %q", e.Covers)
	}
}

// TestExemptionCoverageCountsFailuresAsFindingsDo checks that what an exempt
// call reaches is held to the failure rule findings are: a failure in a
// loop that swallows it is covered by the exemption, as it would be reported
// without it.
func TestExemptionCoverageCountsFailuresAsFindingsDo(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	add := `
//noalloc:root
func CoveredLoop(xs []string) int {
	//noalloc:exempt setup: covers a loop that swallows errors
	return coveredSwallow(xs)
}

func coveredSwallow(xs []string) int {
	total := 0
	for _, x := range xs {
		n, err := coveredParse(x)
		if err != nil {
			continue
		}
		total += n
	}
	return total
}

func coveredParse(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return len(s), nil
}
`
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(string(src) + add)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Exemptions {
		if e.Reason == "setup: covers a loop that swallows errors" {
			if !slices.Contains(e.Covers, "fixture.coveredParse: calls fmt.Errorf") {
				t.Errorf("the exemption covers %q; want coveredParse's fmt.Errorf, which a loop swallows", e.Covers)
			}
			return
		}
	}
	t.Fatal("no exemption covers the loop")
}

// TestARelativeDirFindsWhatAnAbsoluteOneDoes checks that Check finds the same
// allocations, reported against the same files, when Dir and Root are
// relative to the working directory.
func TestARelativeDirFindsWhatAnAbsoluteOneDoes(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	run := func(dir string) []string {
		t.Helper()
		res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
			LineOnly: []string{"response"},
			Allow:    strings.Split(Allowlist, "\n"), Root: dir})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range res.Findings {
			out = append(out, f.String())
		}
		return out
	}
	abs := run(dir)
	if !slices.ContainsFunc(abs, func(f string) bool { return strings.Contains(f, "escapes: ") }) {
		t.Fatal("control: the fixture has no escape")
	}
	for _, d := range []string{"testdata/fixture", "./testdata/../testdata/./fixture", dir + "/../fixture/."} {
		if got := run(d); !slices.Equal(abs, got) {
			t.Errorf("with Dir %s, findings\n%s\nwant\n%s", d, strings.Join(got, "\n"), strings.Join(abs, "\n"))
		}
	}
}

// repeats is a root whose lines each allocate twice for one reason: in an
// escape, a call, an inlined closure, a generic body compiled for two shapes,
// and an exempt line. A chain of concatenations, which compiles to one, and a
// map literal's entries, which fill one map, allocate once.
const repeats = `
var boxed1, boxed2 any

//go:noinline
func box2(a, b any) { boxed1, boxed2 = a, b }

//noalloc:root
func Repeats(x int) {
	box2(x, x)
	_, _ = strconv.Itoa(x), strconv.Itoa(x)
	_ = strconv.Itoa(x) + "!" + strconv.Itoa(x)
	s := strconv.Itoa(x) + "!"
	_ = s + "?"
	s += "(" + s + ")"
	func() { box2(x, x) }()
	boxTwice(x)
	boxTwice(int64(x))
	boxed1 = map[string]int{"a": x, "b": x}
	_, _ = strconv.Quote(s), strconv.Quote(s) //noalloc:exempt setup: two calls
}

//go:noinline
func boxTwice[T int | int64](v T) { box2(v, v) }
`

func checkRepeats(t *testing.T) (*Result, string) {
	t.Helper()
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		LineOnly: []string{"response"},
		Allow:    strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(string(src) + repeats)}})
	if err != nil {
		t.Fatal(err)
	}
	return res, dir
}

// TestEachAllocationOnALineIsCounted checks that a line allocating twice for
// one reason lists that reason twice, for an escape and for a call, and that a
// chain of concatenations lists one.
func TestEachAllocationOnALineIsCounted(t *testing.T) {
	res, _ := checkRepeats(t)
	got := map[string][]string{}
	for _, f := range res.Findings {
		if f.Func == "fixture.Repeats" || strings.HasPrefix(f.Func, "fixture.boxTwice") {
			got[f.Source] = append(got[f.Source], f.Reasons...)
		}
	}
	want := map[string][]string{
		"box2(x,x)":                                  {"escapes: x escapes to heap", "escapes: x escapes to heap"},
		"_,_=strconv.Itoa(x),strconv.Itoa(x)":        {"calls strconv.Itoa", "calls strconv.Itoa"},
		`_=strconv.Itoa(x)+"!"+strconv.Itoa(x)`:      {"calls strconv.Itoa", "calls strconv.Itoa", "string concatenation"},
		`s:=strconv.Itoa(x)+"!"`:                     {"calls strconv.Itoa", "string concatenation"},
		`_=s+"?"`:                                    {"string concatenation"},
		`s+="("+s+")"`:                               {"string concatenation"},
		`func(){box2(x,x)}()`:                        {"escapes: x escapes to heap in the inlined fixture.Repeats$1", "escapes: x escapes to heap in the inlined fixture.Repeats$1"},
		`func boxTwice[T int|int64](v T){box2(v,v)}`: {"escapes: v escapes to heap", "escapes: v escapes to heap"},
		`boxed1=map[string]int{"a":x,"b":x}`:         {"escapes: map[string]int{...} escapes to heap", "map write"},
	}
	for src, w := range want {
		g := slices.Sorted(slices.Values(got[src]))
		if !slices.Equal(g, w) {
			t.Errorf("%s: reasons %q, want %q", src, g, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("lines %q, want those of %q", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	i := slices.IndexFunc(res.Exemptions, func(e Exemption) bool { return e.Reason == "setup: two calls" })
	if i < 0 {
		t.Fatal("no exemption setup: two calls")
	}
	if c, w := res.Exemptions[i].Covers, []string{"fixture.Repeats: calls strconv.Quote", "fixture.Repeats: calls strconv.Quote"}; !slices.Equal(c, w) {
		t.Errorf("the exemption covers %q, want %q", c, w)
	}
}

// TestARestatementThatAddsAnAllocationIsDrift checks that a listed line
// reworded to allocate once more for the same reason is a new allocation, not
// only a restatement.
func TestARestatementThatAddsAnAllocationIsDrift(t *testing.T) {
	res, _ := checkRepeats(t)
	base := filepath.Join(t.TempDir(), "base.txt")
	r := Ratchet{Baselines: []Baseline{{"", base}}}
	var listed []string
	for _, l := range r.Lines(res, "") {
		if l.Func == "fixture.Repeats" && l.Source == "box2(x,x)" {
			continue
		}
		listed = append(listed, l.Key())
	}
	listed = append(listed, "fixture.go | fixture.Repeats | escapes: x escapes to heap | box2(x,nil)")
	writeFile(t, base, strings.Join(listed, "\n")+"\n")
	d, err := r.Compare(res)
	if err != nil {
		t.Fatal(err)
	}
	b := d.Baselines[0]
	if len(b.Restated) != 1 || len(b.Added) != 1 || b.Added[0].Source != "box2(x,x)" || len(b.Stale) != 0 || d.Clean() {
		t.Errorf("added %v, stale %v, restated %v, clean %v; want box2(x,x) added once and restated once",
			b.Added, b.Stale, b.Restated, d.Clean())
	}
}

// TestAConversionRangedOverIsNotACopy checks that converting a string to
// bytes to range over is not a copy, as the compiler makes none, and that
// converting bytes to a string to range over, or ranging over a conversion
// kept in a variable, is.
func TestAConversionRangedOverIsNotACopy(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	add := `
//noalloc:root
func RangeConverted(s string, b []byte) int {
	n := 0
	for _, c := range []byte(s) {
		n += int(c)
	}
	for range string(b) {
		n++
	}
	kept := []byte(s)
	for _, c := range kept {
		n += int(c)
	}
	return n + len(kept)
}
`
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(string(src) + add)}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range res.Findings {
		if f.Func == "fixture.RangeConverted" {
			got[f.Source] = true
		}
	}
	for src, want := range map[string]bool{"for _,c:=range[]byte(s){": false, "for range string(b){": true, "kept:=[]byte(s)": true} {
		if got[src] != want {
			t.Errorf("%s flagged %v, want %v", src, got[src], want)
		}
	}
	// The compiled code agrees: it makes no allocating call noalloc missed.
	for _, c := range res.CrossCheck.Calls {
		if strings.Contains(c.String(), "RangeConverted") {
			t.Errorf("the compiled code allocates where noalloc found nothing: %s", c)
		}
	}
}

// TestBoxingAValueTheRuntimeHoldsIsNotAnAllocation checks that boxing a
// value of a type with no size, a one-byte integer, or a bool into an
// interface, which the compiler reports as an escape but points at the
// runtime's memory, is not a finding, and that the compiled code agrees.
func TestBoxingAValueTheRuntimeHoldsIsNotAnAllocation(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	add := `
type emptyErr struct{}

func (emptyErr) Error() string { return "empty" }

type byteErr byte

func (byteErr) Error() string { return "byte" }

type wideErr uint16

func (wideErr) Error() string { return "wide" }

var boxedSink any
var errSink error
var strSink string

//noalloc:root
func BoxZeroSize(n int, b byte, ok bool, w uint16) {
	boxedSink = struct{}{}
	errSink = emptyErr{}
	errSink = byteErr(b)
	boxedSink = ok
	if n < 0 {
		boxedSink = n
	}
	errSink = wideErr(w)
	strSink = string(b)
}
`
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(string(src) + add)}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range res.Findings {
		if f.Func == "fixture.BoxZeroSize" {
			got[f.Source] = true
		}
	}
	for src, want := range map[string]bool{"boxedSink=struct{}{}": false, "errSink=emptyErr{}": false,
		"errSink=byteErr(b)": false, "boxedSink=ok": false, "boxedSink=n": true, "errSink=wideErr(w)": true,
		"strSink=string(b)": true} {
		if got[src] != want {
			t.Errorf("%s flagged %v, want %v", src, got[src], want)
		}
	}
	for _, c := range res.CrossCheck.Calls {
		if strings.Contains(c.String(), "BoxZeroSize") {
			t.Errorf("the compiled code allocates where noalloc found nothing: %s", c)
		}
	}
}

// TestAppendingAMakeIsNoMake checks that append(s, make([]T, n)...), which
// the compiler turns into growing s by n without making the slice, reports
// no make, and that the compiled code agrees.
func TestAppendingAMakeIsNoMake(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	add := `
var extended []byte

//noalloc:root
func Extend(sc *Scratch, n int) {
	sc.buf = append(sc.buf[:0], make([]byte, n)...)
	kept := make([]byte, n)
	extended = append(extended[:0], kept...)
}
`
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(string(src) + add)}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range res.Findings {
		if f.Func == "fixture.Extend" {
			got[f.Source] = f.Reasons
		}
	}
	if r := got["sc.buf=append(sc.buf[:0],make([]byte,n)...)"]; slices.ContainsFunc(r, func(r string) bool { return strings.HasPrefix(r, "make") }) {
		t.Errorf("appending a make reports a make: %q", r)
	}
	if r := got["kept:=make([]byte,n)"]; !slices.ContainsFunc(r, func(r string) bool { return strings.HasPrefix(r, "make") }) {
		t.Errorf("a make kept in a variable reports no make: %q", r)
	}
	for _, c := range res.CrossCheck.Calls {
		if strings.Contains(c.String(), "Extend") {
			t.Errorf("the compiled code allocates where noalloc found nothing: %s", c)
		}
	}
}

// TestGrowingByHandIsGrowing checks a function shaped like append that grows
// what it appends to by making a longer slice and copying into it: that make
// grows reused memory, as append does, and what is appended after it is
// still reused memory. The same make and copy elsewhere is a make.
func TestGrowingByHandIsGrowing(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	add := `
//noalloc:append
func AppendByHand(dst []byte, s string) []byte {
	if cap(dst)-len(dst) < len(s) {
		grown := make([]byte, len(dst), len(dst)+len(s))
		copy(grown, dst)
		dst = grown
	}
	return append(dst, s...)
}

var handSink []byte

//noalloc:root
func CopyByHand(b []byte) {
	c := make([]byte, len(b), 2*len(b))
	copy(c, b)
	handSink = c
}
`
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(string(src) + add)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings {
		if f.Func == "fixture.AppendByHand" {
			t.Errorf("growing by hand allocates: %s", f)
		}
	}
	if !slices.ContainsFunc(res.Findings, func(f Finding) bool {
		return f.Func == "fixture.CopyByHand" && slices.Contains(f.Reasons, "make with a size known only at run time")
	}) {
		t.Error("a make and copy outside a function shaped like append is no make")
	}
}

// TestSetupThroughAnAtomicIsSetup checks that a value made the first time an
// atomic field is found empty, and stored to it, is first-use setup, as one
// stored to a plain field is, and that a value made each time is not.
func TestSetupThroughAnAtomicIsSetup(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	over := strings.Replace(string(src), "import (\n", "import (\n\t\"sync/atomic\"\n", 1) + `
type lazyDone struct{ done atomic.Value }

//noalloc:root
func (l *lazyDone) Done() chan struct{} {
	d := l.done.Load()
	if d == nil {
		d = make(chan struct{})
		l.done.Store(d)
	}
	return d.(chan struct{})
}

var everyTime atomic.Value

//noalloc:root
func MakeEachTime() {
	everyTime.Store(make(chan struct{}))
}
`
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
		Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(over)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings {
		if f.Func == "(*fixture.lazyDone).Done" && slices.Contains(f.Reasons, "make chan") {
			t.Errorf("first-use setup through an atomic is a finding: %s", f)
		}
	}
	if !slices.ContainsFunc(res.Findings, func(f Finding) bool {
		return f.Func == "fixture.MakeEachTime" && slices.Contains(f.Reasons, "make chan")
	}) {
		t.Error("a channel made on every call is no finding")
	}
}
