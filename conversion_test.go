package noalloc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// conversionKinds are the conversions and concatenations the compiler gives a
// fixed stack buffer when the result does not escape. The test's input is
// longer than each buffer, so one allocates unless its input fits (short, rune)
// or the compiler reads the bytes in place.
var conversionKinds = []struct {
	name, expr, result string
	// broken is the conversion across lines, brokenCode the same with code on
	// the operand's line, and wrapped holds a zero-copy []byte(string) inside it.
	broken, brokenCode, wrapped string
}{
	{"bytes", "string(b)", "string", "string(\nb,\n)", "string(\nb[:len(b)-1],\n)", "string(pick([]byte(s)))"},
	{"short", "string(b[:8])", "string", "string(\nb[:8],\n)", "string(\nb[:8],\n)", "string(pick([]byte(s))[:8])"},
	{"runes", "string(rs)", "string", "string(\nrs,\n)", "string(\nrs[:len(rs)-1],\n)", "string(runesOf([]byte(s)))"},
	{"rune", "string(r)", "string", "string(\nr,\n)", "string(\nr+1,\n)", "string(runeOf([]byte(s)))"},
	{"concat", "s + t", "string", "s +\nt", "s +\nt[:len(t)-1]", "s + stringOf([]byte(s))"},
	{"tobytes", "[]byte(s)", "[]byte", "[]byte(\ns,\n)", "[]byte(\ns[:len(s)-1],\n)", "[]byte(stringOf([]byte(s)))"},
	{"torunes", "[]rune(s)", "[]rune", "[]rune(\ns,\n)", "[]rune(\ns[:len(s)-1],\n)", "[]rune(stringOf([]byte(s)))"},
}

// conversionLayouts are the forms each kind is written in: every cell of the
// table runs in each.
var conversionLayouts = []struct {
	name string
	expr func(kind int) string
}{
	{"on one line", func(kind int) string { return conversionKinds[kind].expr }},
	{"across lines", func(kind int) string { return conversionKinds[kind].broken }},
	{"across lines with code on the operand's", func(kind int) string { return conversionKinds[kind].brokenCode }},
	{"around a zero-copy conversion", func(kind int) string { return conversionKinds[kind].wrapped }},
}

// conversionPairFirst and conversionPairSecond are conversions written so the
// second starts on the line the first closes on, each marked if it copies.
var (
	conversionPairFirst = []struct {
		expr   string
		copies bool
	}{
		{"count(string(\nb))", true},
		{"m[string(\nb)]", false},
		{"boolInt(string(\nb) == t)", false},
		{"count(string(\nb[:8]))", false},
		{"countBytes([]byte(\ns))", false},
	}
	conversionPairSecond = []struct {
		expr   string
		copies bool
	}{
		{"count(string(b[:40]))", true},
		{"m[string(b[:40])]", false},
		{"boolInt(string(b[:40]) == t)", false},
		{"count(string(b[:16]))", false},
		{"countBytes([]byte(s))", false},
	}
)

// Each shape's outcomes hold one letter per conversion kind, in order: A
// allocates and is a finding, C allocates nothing and is no finding, and - is a
// shape the kind's type can't take. Each holds in the module's own code and in a
// dependency's, where Check reads the compiled code alike.
//
// CONV is the conversion and COUNT a function taking its result that the
// compiler won't inline.
var conversionShapes = []struct{ name, body, outcomes string }{
	// The tag of a switch is read in place only when every case is a constant.
	{"switch tag, constant cases", "switch CONV {\ncase \"x\":\nn++\ncase \"y\":\nn += 2\n}", "CCACA--"},
	{"switch tag, variable cases", "switch CONV {\ncase t:\nn++\ncase u:\nn += 2\n}", "ACACA--"},
	{"switch tag, mixed cases", "switch CONV {\ncase \"x\":\nn++\ncase u:\nn += 2\n}", "ACACA--"},
	{"switch tag, parenthesized", "switch (CONV) {\ncase \"x\":\nn++\n}", "CCACA--"},
	{"switch on initialized variable", "switch v := CONV; v {\ncase \"x\":\nn++\n}", "ACACA--"},
	{"switch on variable", "v := CONV\nswitch v {\ncase \"x\":\nn++\n}", "ACACA--"},
	{"switch case comparison", "switch {\ncase CONV == t:\nn++\n}", "CCACA--"},

	// A comparison reads its operand in place only when it is written there.
	{"equal", "if CONV == t {\nn++\n}", "CCACA--"},
	{"not equal", "if CONV != t {\nn++\n}", "CCACA--"},
	{"less", "if CONV < t {\nn++\n}", "CCACA--"},
	{"equal constant", "if CONV == \"x\" {\nn++\n}", "CCACA--"},
	{"equal, parenthesized", "if (CONV) == t {\nn++\n}", "CCACA--"},
	{"compare variable", "v := CONV\nif v == t {\nn++\n}", "ACACA--"},
	{"compare variable twice", "v := CONV\nif v == t || v == u {\nn++\n}", "ACACA--"},
	{"compare through interface", "var i any = t\nif CONV == i {\nn++\n}", "ACACA--"},

	// A lookup key is read in place when written there, and a variable holding
	// it when the lookup is its one use and nothing writes memory first.
	{"lookup", "n += m[CONV]", "CCACA--"},
	{"lookup with ok", "if _, ok := m[CONV]; ok {\nn++\n}", "CCACA--"},
	{"lookup variable", "v := CONV\nn += m[v]", "CCACA--"},
	{"lookup variable, large values", "v := CONV\nn += int(big[v][0])", "ACACA--"},
	{"lookup variable twice", "v := CONV\nn += m[v] + m[v]", "ACACA--"},
	{"lookup variable after a write", "v := CONV\nb[0] = 'q'\nn += m[v]", "ACACA--"},
	{"lookup variable after a call", "v := CONV\nn += count(t)\nn += m[v]", "ACACA--"},
	{"lookup variable in a later block", "v := CONV\nif len(t) > 0 {\nn += m[v]\n}", "CCACA--"},
	{"lookup in a struct key", "n += structs[pair{CONV, t}]", "CCACA--"},
	{"lookup in an array key", "n += arrays[[2]string{CONV, t}]", "CCACA--"},

	{"range", "for range CONV {\nn++\n}", "ACACACA"},
	{"range with values", "for _, c := range CONV {\nn += int(c)\n}", "ACACACA"},
	{"range variable", "v := CONV\nfor _, c := range v {\nn += int(c)\n}", "ACACACA"},
	{"variable length", "v := CONV\nn += len(v)", "ACACACA"},
	{"variable index", "v := CONV\nn += int(v[0])", "ACACACA"},
	{"variable passed", "v := CONV\nn += COUNT(v)", "ACACACA"},
	{"passed", "n += COUNT(CONV)", "ACACACA"},
	{"length", "n += len(CONV)", "CCACACC"},
	{"index", "n += int((CONV)[0])", "ACACACA"},
	{"discarded", "_ = CONV", "ACACACA"},
	{"variable written", "v := CONV\nv[0] = 'q'\nn += int(v[0])", "-----AA"},
	{"operand of a concatenation", "if (\"a\" + CONV) == t {\nn++\n}", "ACACA--"},
}

// conversionBetween are statements between a string([]byte) held in a variable
// and its one use as a map key, each with its outcome as a table letter. None
// allocates alone. The compiler reads the bytes in place only when the statement
// leaves memory and calls nothing.
var conversionBetween = append(conversionBetweenOperators(), []conversionBetweenRow{
	{"nothing", "", "", "C"},
	{"arithmetic", "n += len(t)", "", "C"},
	{"read of a global", "n += global", "", "C"},
	{"read of a field", "n += len(globalPair.a)", "", "C"},
	{"read of an element", "n += int(b[0])", "", "C"},
	{"write to a global", "global = 1", "", "A"},
	{"another lookup", "n += m[t]", "", "A"},
	{"receive", "<-closedChan", "", "A"},
	{"range over a map", "for range m {\nn++\n}", "", "A"},
	{"string comparison", "if t == u {\nn++\n}", "", stringEqualOutcome()},
	{"type assertion", "if _, ok := anyValue.(string); ok {\nn++\n}", "", "C"},
	{"boxing a constant", "anyValue = \"x\"", "", "A"},
	{"call", "n += count(t)", "", "A"},
	{"copy", "copy(scratchBytes[:], t)", "", "A"},
	{"closure", "func() { n++ }()", "", "C"},
	{"atomic load", "n += int(atomic.LoadInt64(&counter))", "", "A"},
	{"make of a constant size", "_ = make([]byte, 8)", "", "A"},

	{"len of a string", "n += len(t)", "", "C"},
	{"len of a slice", "n += len(b)", "", "C"},
	{"len of an array", "n += len(scratchBytes)", "", "C"},
	{"len of a map", "n += len(m)", "", "C"},
	{"len of a chan", "n += len(closedChan)", "", "A"},
	{"cap of a slice", "n += cap(b)", "", "C"},
	{"cap of a chan", "n += cap(closedChan)", "", "A"},

	{"equal ints", "eq := global == n", "if eq {\nn++\n}", "C"},
	{"equal floats", "eq := globalFloat == globalFloat2", "if eq {\nn++\n}", "C"},
	{"equal strings", "eq := t == u", "if eq {\nn++\n}", stringEqualOutcome()},
	{"equal interfaces", "eq := anyValue == anyValue2", "if eq {\nn++\n}", "A"},
	{"equal structs", "eq := globalPair == globalPair2", "if eq {\nn++\n}", stringEqualOutcome()},
	{"equal arrays", "eq := scratchBytes == scratchBytes2", "if eq {\nn++\n}", "C"},
	{"read of a large struct", "large := globalLarge\nn += large[1]", "", "A"},
}...)

type conversionBetweenRow struct{ name, stmt, after, outcome string }

// stringEqualOutcome is the table letter for a comparison of strings between a
// conversion and its lookup: the compiler reads the bytes in place across it on
// arm64 and copies them on amd64.
func stringEqualOutcome() string {
	if runtime.GOARCH == "amd64" {
		return "A"
	}
	return "C"
}

// conversionBetweenOperators is a row for each binary operator over each scalar
// type, computed without a branch and used after the lookup. Complex division
// is a runtime call.
func conversionBetweenOperators() []conversionBetweenRow {
	var rows []conversionBetweenRow
	for _, operands := range []struct {
		name, x, y, sink string
		ops              []string
	}{
		{"int", "intA", "intB", "sinkInt", []string{"+", "-", "*", "/", "%", "&", "|", "^", "&^", "<<", ">>"}},
		{"int64", "int64A", "int64B", "sinkInt64", []string{"+", "-", "*", "/", "%", "&", "|", "^", "&^", "<<", ">>"}},
		{"float", "floatA", "floatB", "sinkFloat", []string{"+", "-", "*", "/"}},
		{"complex", "complexA", "complexB", "sinkComplex", []string{"+", "-", "*", "/"}},
	} {
		compare := []string{"==", "!="}
		if operands.name != "complex" {
			compare = append(compare, "<", "<=", ">", ">=")
		}
		for _, sink := range []struct {
			name string
			ops  []string
		}{{operands.sink, operands.ops}, {"sinkBool", compare}} {
			for _, op := range sink.ops {
				outcome := "C"
				if operands.name == "complex" && op == "/" {
					outcome = "A"
				}
				rows = append(rows, conversionBetweenRow{operands.name + " " + op,
					"q := " + operands.x + " " + op + " " + operands.y, sink.name + " = q", outcome})
			}
		}
	}
	return rows
}

func conversionCell(layout, kind, shape int) string {
	return fmt.Sprintf("L%dK%dS%d", layout, kind, shape)
}

// conversionFuncs is the source of one function per conversion in each shape and
// layout and per statement between a conversion and its lookup, each under the
// marker, and the names of those functions. A control beside each statement runs
// it alone.
func conversionFuncs(t *testing.T, marker string) (string, []string) {
	t.Helper()
	var src strings.Builder
	var names []string
	counts := map[string]string{"string": "count", "[]byte": "countBytes", "[]rune": "countRunes"}
	add := func(name, body string) {
		names = append(names, name)
		fmt.Fprintf(&src, "%s//go:noinline\nfunc %s(b []byte, s, t, u string, rs []rune, r rune) int {\nn := 0\n_, _, _, _, _, _ = b, s, t, u, rs, r\n%s\nreturn n\n}\n\n", marker, name, body)
	}
	for shapeIndex, shape := range conversionShapes {
		if len(shape.outcomes) != len(conversionKinds) {
			t.Fatalf("shape %q has %d outcomes for %d kinds", shape.name, len(shape.outcomes), len(conversionKinds))
		}
		for kindIndex, kind := range conversionKinds {
			if shape.outcomes[kindIndex] == '-' {
				continue
			}
			for layoutIndex, layout := range conversionLayouts {
				body := strings.NewReplacer("CONV", layout.expr(kindIndex), "COUNT", counts[kind.result]).Replace(shape.body)
				add(conversionCell(layoutIndex, kindIndex, shapeIndex), body)
			}
		}
	}
	for i, between := range conversionBetween {
		add(fmt.Sprintf("B%d", i), "v := string(b)\n"+between.stmt+"\nn += m[v]\n"+between.after)
		add(fmt.Sprintf("BC%d", i), between.stmt+"\n"+between.after)
	}
	for i, first := range conversionPairFirst {
		for j, second := range conversionPairSecond {
			add(fmt.Sprintf("P%dP%d", i, j), "n += "+first.expr+" + "+second.expr)
		}
	}
	return src.String(), names
}

const conversionHelpers = `
type pair struct{ a, b string }

var (
	m       = map[string]int{}
	big     = map[string][200]byte{}
	structs = map[pair]int{}
	arrays  = map[[2]string]int{}
)

//go:noinline
func count(s string) int { return len(s) }

//go:noinline
func countBytes(s []byte) int { return len(s) }

//go:noinline
func countRunes(s []rune) int { return len(s) }

//go:noinline
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

var (
	longString  = "qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
	globalFloat, globalFloat2 float64
	counter int64
	globalPair2 pair
	anyValue2 any = "y"
	scratchBytes2 [8]byte
	globalLarge [64]int
	intA, intB                 = 7, 3
	int64A, int64B     int64   = 7, 3
	floatA, floatB             = 7.0, 3.0
	complexA, complexB         = 7 + 1i, 3 + 2i
	sinkInt            int
	sinkInt64          int64
	sinkFloat          float64
	sinkComplex        complex128
	sinkBool           bool
	longRunes   = []rune(longString)
	global      int
	globalPair  pair
	anyValue    any = "x"
	closedChan  = func() chan int { c := make(chan int); close(c); return c }()
	scratchBytes [8]byte
)

//go:noinline
func pick(x []byte) []byte { return x }

//go:noinline
func runesOf(x []byte) []rune { return longRunes }

//go:noinline
func runeOf(x []byte) rune { return 'q' }

//go:noinline
func stringOf(x []byte) string { return longString }

`

func writeConversionFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// conversionProgram is a module with one root per conversion in each shape,
// and a main that prints how many allocations each makes per call.
func conversionProgram(t *testing.T) string {
	t.Helper()
	funcs, names := conversionFuncs(t, "//noalloc:root\n")
	var cells strings.Builder
	for _, name := range names {
		fmt.Fprintf(&cells, "\t{%q, %s},\n", name, name)
	}
	dir := t.TempDir()
	writeConversionFile(t, dir, "go.mod", "module conversions\n\ngo 1.27\n")
	writeConversionFile(t, dir, "main.go", `package main

import (
	"fmt"
	"sync/atomic"
	"testing"
)
`+conversionHelpers+funcs+fmt.Sprintf(`func main() {
	b := make([]byte, 64)
	for i := range b {
		b[i] = 'q'
	}
	s, t, u := string(b), string(b[:40]), string(b[:41])
	rs := []rune(s)
	for _, c := range []struct {
		name string
		f    func([]byte, string, string, string, []rune, rune) int
	}{
%s	} {
		fmt.Println(c.name, testing.AllocsPerRun(100, func() { c.f(b, s, t, u, rs, 'q') }))
	}
}
`, cells.String()))
	return dir
}

// conversionDependency is a module whose roots each call the same conversion
// in a module it depends on.
func conversionDependency(t *testing.T) string {
	t.Helper()
	funcs, names := conversionFuncs(t, "")
	var roots strings.Builder
	for _, name := range names {
		fmt.Fprintf(&roots, "//noalloc:root\nfunc %s(b []byte, s, t, u string, rs []rune, r rune) int {\nreturn dep.%s(b, s, t, u, rs, r)\n}\n\n", name, name)
	}
	dir := t.TempDir()
	writeConversionFile(t, dir, "go.mod", "module callers\n\ngo 1.27\n\nrequire conversionsdep v0.0.0\n\nreplace conversionsdep => ./dep\n")
	writeConversionFile(t, dir, "callers.go", "package callers\n\nimport \"conversionsdep\"\n\n"+strings.ReplaceAll(roots.String(), "dep.", "conversionsdep."))
	dep := filepath.Join(dir, "dep")
	writeConversionFile(t, dep, "go.mod", "module conversionsdep\n\ngo 1.27\n")
	writeConversionFile(t, dep, "dep.go", "package conversionsdep\n\nimport \"sync/atomic\"\n"+conversionHelpers+funcs)
	return dir
}

// TestConversionsPastTheirStackBufferAreFindings holds Check to the program's
// own measured allocations over every conversion in every shape, in the module's
// own code and in a dependency's. A conversion whose stack buffer does not bound
// its input allocates without escaping, so nothing in the escape report shows
// it. A compiler that changes what it reads in place fails here too.
func TestConversionsPastTheirStackBufferAreFindings(t *testing.T) {
	dir := conversionProgram(t)
	env := append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	run := exec.Command("go", "run", ".")
	run.Dir, run.Env = dir, env
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("go run: %v\n%s", err, out)
	}
	measured := map[string]float64{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, allocs, _ := strings.Cut(l, " ")
		if measured[name], err = strconv.ParseFloat(allocs, 64); err != nil {
			t.Fatalf("output line %q: %v", l, err)
		}
	}

	check := func(dir, module string) (map[string][]string, *Result) {
		res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: module, Env: env,
			Allow: strings.Split(Allowlist, "\n"), Root: dir})
		if err != nil {
			t.Fatal(err)
		}
		found := map[string][]string{}
		for _, f := range res.Findings {
			name := f.Func[strings.LastIndex(f.Func, ".")+1:]
			found[name] = append(found[name], f.Reasons...)
		}
		return found, res
	}
	found, res := check(dir, "conversions")
	foundInDependency, _ := check(conversionDependency(t), "callers")

	verify := func(at, name string, want byte) {
		t.Helper()
		allocs, ok := measured[name]
		if !ok {
			t.Errorf("%s: the program measured nothing", at)
			return
		}
		if (allocs > 0) != (want == 'A') {
			t.Errorf("%s: %v allocations per call, table says %c", at, allocs, want)
		}
		reasons := found[name]
		if (len(reasons) > 0) != (want == 'A') {
			t.Errorf("%s: findings %q, table says %c", at, reasons, want)
		}
		for _, r := range reasons {
			if !strings.Contains(r, "conversion") && !strings.Contains(r, "concatenation") {
				t.Errorf("%s: reason %q names neither a conversion nor a concatenation", at, r)
			}
		}
		if got := foundInDependency[name]; (len(got) > 0) != (want == 'A') {
			t.Errorf("%s in a dependency: findings %q, table says %c", at, got, want)
		}
	}

	cells := 0
	for shapeIndex, shape := range conversionShapes {
		for kindIndex, kind := range conversionKinds {
			want := shape.outcomes[kindIndex]
			if want == '-' {
				continue
			}
			for layoutIndex, layout := range conversionLayouts {
				cells++
				name := conversionCell(layoutIndex, kindIndex, shapeIndex)
				verify(fmt.Sprintf("%s %s in %s (%s)", kind.expr, layout.name, shape.name, name), name, want)
			}
		}
	}
	for i, between := range conversionBetween {
		cells++
		name := fmt.Sprintf("B%d", i)
		verify(fmt.Sprintf("a lookup after %s (%s)", between.name, name), name, between.outcome[0])
		control := fmt.Sprintf("BC%d", i)
		if measured[control] > 0 || len(found[control]) > 0 || len(foundInDependency[control]) > 0 {
			t.Errorf("control: %s alone allocates %v, with findings %q and in a dependency %q", between.name, measured[control], found[control], foundInDependency[control])
		}
	}
	// A pair's findings are at the line each conversion starts on: the function's
	// fourth line for the first and its fifth for the second.
	src, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	for i, first := range conversionPairFirst {
		for j, second := range conversionPairSecond {
			cells++
			name := fmt.Sprintf("P%dP%d", i, j)
			at := first.expr + " then " + second.expr
			start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "func "+name+"(") }) + 1
			var want []int
			copies := 0.0
			if first.copies {
				want = append(want, start+3)
				copies++
			}
			if second.copies {
				want = append(want, start+4)
				copies++
			}
			var got []int
			for _, f := range res.Findings {
				if strings.HasSuffix(f.Func, "."+name) {
					got = append(got, f.Line)
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s (%s): findings on lines %v, want %v", at, name, got, want)
			}
			if measured[name] != copies {
				t.Errorf("%s (%s): %v allocations per call, want %v", at, name, measured[name], copies)
			}
			if got := foundInDependency[name]; (len(got) > 0) != (copies > 0) {
				t.Errorf("%s (%s) in a dependency: findings %q", at, name, got)
			}
		}
	}
	if cells < 100 {
		t.Fatalf("control: only %d cells", cells)
	}
	for _, c := range res.CrossCheck.Calls {
		t.Errorf("compiled code allocates on a line with no finding: %s    %s", c, c.Source)
	}
}
