package noalloc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// compiledCodeKinds is a function of a dependency for each kind of conversion
// the cross-check of summaries meets, how a root calls it, and whether the root
// has a finding.
var compiledCodeKinds = []struct {
	name, call, body string
	finding          bool
}{
	{name: "Copies", call: "codedep.Copies(b)", finding: true, body: `
func Copies(b []byte) int { return count(string(b)) }`},
	{name: "Compare", call: "codedep.Compare(b, s)", body: `
func Compare(b []byte, s string) int {
	if string(b) == s {
		return 1
	}
	return 0
}`},
	{name: "Length", call: "codedep.Length(b)", body: `
func Length(b []byte) int { return len(string(b)) }`},
	{name: "Lookup", call: "codedep.Lookup(b, m)", body: `
func Lookup(b []byte, m map[string]int) int { return m[string(b)] }`},
	{name: "AcrossLines", call: "codedep.AcrossLines(b, m)", body: `
func AcrossLines(b []byte, m map[string]int) int {
	return m[string(
		b,
	)]
}`},
	{name: "Generic", call: "codedep.Generic(b, m)", body: `
func Generic[T any](b []byte, m map[string]T) int {
	if _, ok := m[string(b)]; ok {
		return 1
	}
	return 0
}`},
	{name: "Inlined", call: "codedep.Inlined(b)", body: `
func Inlined(b []byte) int { return length(b) }

func length(b []byte) int { return len(string(b)) }`},
	{name: "StackBuffer", call: "codedep.StackBuffer(b)", body: `
func StackBuffer(b []byte) int { return count(string(b[:8])) }`},
	{name: "StackBufferAcrossLines", call: "codedep.StackBufferAcrossLines(b)", body: `
func StackBufferAcrossLines(b []byte) int {
	return count(string(
		b[:8],
	))
}`},
	{name: "ConcatAcrossLines", call: "codedep.ConcatAcrossLines(b)", body: `
func ConcatAcrossLines(b []byte) int {
	s := string(b[:4])
	return count(s +
		"ab" +
		s)
}`},
	{name: "Rune", call: "codedep.Rune(r)", body: `
func Rune(r rune) int { return count(string(r)) }`},
	// A store fixed to true runs only otherArm's first arm, as in strconv's
	// formatBits with its append argument. The compiled code holds the other arm's
	// copy, which a caller that does not fix store reaches.
	{name: "OtherArm", call: "codedep.OtherArm(b, m)", body: `
func OtherArm(b []byte, m map[string]int) int { return otherArm(b, m, 3, true) }

//go:noinline
func otherArm(b []byte, m map[string]int, n int, store bool) int {
	if store {
		return m[string(b)]
	}
	sink = string(b[:n])
	return 0
}

var sink string`},
	{name: "BothArms", call: `codedep.BothArms(b, m, s == "")`, finding: true, body: `
func BothArms(b []byte, m map[string]int, store bool) int { return otherArm(b, m, 3, store) }`},
	{name: "FailurePath", call: "codedep.FailurePath(b, m)", body: `
func FailurePath(b []byte, m map[string]int) int {
	if len(b) > 1<<20 {
		panic("too long: " + string(b))
	}
	return m[string(b)]
}`},
	{name: "Escapes", call: "codedep.Escapes(b, m)", finding: true, body: `
func Escapes(b []byte, m map[string]int) int {
	kept = new(int)
	return m[string(b)]
}

var kept *int`},
	{name: "MakesASlice", call: "codedep.MakesASlice(b, m)", finding: true, body: `
func MakesASlice(b []byte, m map[string]int) int {
	kept2 = make([]byte, len(b))
	return m[string(b)]
}

var kept2 []byte`},
	{name: "Grows", call: "codedep.Grows(b, m)", finding: true, body: `
func Grows(b []byte, m map[string]int) int {
	kept3 = append(kept3, 1)
	return m[string(b)]
}

var kept3 []byte`},
	// The allowlist labels labeled, which is inlined into its caller.
	{name: "Labeled", call: "codedep.Labeled(b, m)", body: `
func Labeled(b []byte, m map[string]int) int { return labeled(b) + m[string(b)] }

func labeled(b []byte) int { return count(string(b)) }`},
	{name: "StdIndexRune", call: "strings.IndexRune(s, r)"},
	// A finding for its append, with formatBits' other arm quiet.
	{name: "StdFormatBits", call: "len(strconv.AppendInt(b[:0], 7, 10))", finding: true},
}

// compiledCodeModule is a module with a root for each of compiledCodeKinds,
// named for it.
func compiledCodeModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var roots, bodies strings.Builder
	for _, k := range compiledCodeKinds {
		fmt.Fprintf(&roots, "//noalloc:root\nfunc %s(b []byte, r rune, s string, m map[string]int) int { return %s }\n\n", k.name, k.call)
		bodies.WriteString(k.body + "\n")
	}
	writeConversionFile(t, dir, "go.mod", "module callers\n\ngo 1.27\n\nrequire codedep v0.0.0\n\nreplace codedep => ./dep\n")
	writeConversionFile(t, dir, "callers.go", "package callers\n\nimport (\n\t\"strconv\"\n\t\"strings\"\n\n\t\"codedep\"\n)\n\nvar _, _ = strconv.Itoa, strings.ToUpper\n\n"+roots.String())
	dep := filepath.Join(dir, "dep")
	writeConversionFile(t, dep, "go.mod", "module codedep\n\ngo 1.27\n")
	writeConversionFile(t, dep, "dep.go", "package codedep\n\n//go:noinline\nfunc count(s string) int { return len(s) }\n"+bodies.String())
	return dir
}

// checkCompiledCode checks the module of compiledCodeModule.
func checkCompiledCode(dir, cache string) (*Result, error) {
	return Check(Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Env: append(os.Environ(), "GOFLAGS=", "GOWORK=off"),
		Allow: append(strings.Split(Allowlist, "\n"), "measured: codedep.labeled"), Root: dir, SummaryCache: cache})
}

// TestCompiledCodeOfDependenciesIsCrossChecked holds a dependency's conversions
// to its compiled code as read a second way. The check is quiet over each kind of
// conversion that makes no copy, and stops over each way a reading could wrongly
// say an allocation is not there: the call lost, on a line off by one, the rule
// answering without looking, or the source misread.
func TestCompiledCodeOfDependenciesIsCrossChecked(t *testing.T) {
	dir := compiledCodeModule(t)
	asked := 0
	testHookSummariesChecked = func(n int) { asked = n }
	defer func() { testHookSummariesChecked = nil }()
	res, err := checkCompiledCode(dir, t.TempDir())
	if err != nil {
		t.Fatalf("clean code: %v", err)
	}
	if asked == 0 {
		t.Fatal("control: the cross-check asked nothing")
	}
	t.Logf("%d questions asked", asked)
	found := map[string]bool{}
	for _, f := range res.Findings {
		found[strings.TrimPrefix(f.Func, "callers.")] = true
	}
	for _, k := range compiledCodeKinds {
		if found[k.name] != k.finding {
			t.Errorf("%s has a finding: %v, want %v, in %s", k.name, found[k.name], k.finding, Report(res))
		}
	}

	inDep := func(file string) bool { return file == "dep.go" }
	for _, fault := range []struct {
		name    string
		call    func(sym, file string, line int, runtime string) (int, bool)
		rule    func(in *ssa.Convert) bool
		builtin func(in ssa.Instruction) bool
		fn, rt  string
	}{
		{name: "the reader loses the call", fn: "Copies", rt: "slicebytetostring", call: func(_, file string, line int, _ string) (int, bool) { return line, !inDep(file) }},
		{name: "the reader puts the call on the next line", fn: "Copies", rt: "slicebytetostring", call: func(_, file string, line int, _ string) (int, bool) {
			if inDep(file) {
				return line + 1, true
			}
			return line, true
		}},
		{name: "the rule answers without looking", fn: "Copies", rt: "slicebytetostring", rule: func(in *ssa.Convert) bool { return in.Parent().Name() == "Copies" }},
		{name: "the source's reading misses an append", fn: "Grows", rt: "growslice", builtin: func(in ssa.Instruction) bool { return in.Parent().Name() == "Grows" }},
	} {
		t.Run(fault.name, func(t *testing.T) {
			testHookCodeCall, testHookCompiledCode, testHookMissesBuiltin = fault.call, fault.rule, fault.builtin
			defer func() { testHookCodeCall, testHookCompiledCode, testHookMissesBuiltin = nil, nil, nil }()
			res, err := checkCompiledCode(dir, t.TempDir())
			var disagree *CompiledCodeError
			if !errors.As(err, &disagree) {
				t.Fatalf("no disagreement reported: %v, with findings %v", err, res)
			}
			hit := false
			for _, c := range disagree.Calls {
				hit = hit || c.File == "dep.go" && strings.HasSuffix(c.Func, "codedep."+fault.fn) && c.Runtime == fault.rt
			}
			if !hit {
				t.Errorf("%s calling %s is not among %v", fault.fn, fault.rt, disagree.Calls)
			}
		})
	}
}

// TestADisagreementIsNotCached holds summaries with a disagreement out of the
// cache: the check that found it stops, and so does the next.
func TestADisagreementIsNotCached(t *testing.T) {
	dir := compiledCodeModule(t)
	cache := t.TempDir()
	testHookCodeCall = func(_, file string, line int, _ string) (int, bool) { return line, file != "dep.go" }
	defer func() { testHookCodeCall = nil }()
	for run := range 2 {
		var disagree *CompiledCodeError
		if _, err := checkCompiledCode(dir, cache); !errors.As(err, &disagree) {
			t.Fatalf("run %d: no disagreement reported: %v", run, err)
		}
	}
}

// TestAnArchiveShowsTheCallsTheCompilerWrites compares the two ways noalloc reads
// compiled code: objdump over a package's archive, and the compiler's own assembly
// of the same package.
func TestAnArchiveShowsTheCallsTheCompilerWrites(t *testing.T) {
	dir := compiledCodeModule(t)
	env := append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	pkgs, err := buildPackages(Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Env: env, Root: dir}, env)
	if err != nil {
		t.Fatal(err)
	}
	archives := map[string]string{}
	for _, p := range pkgs {
		archives[p.path] = p.archive
	}
	for _, pkg := range []string{"codedep", "internal/strconv", "strings"} {
		t.Run(pkg, func(t *testing.T) {
			m := newMachineCode(env, archives)
			m.code = newCodeReader()
			defer m.code.spill.close()
			m.lines(pkg)
			fromArchive := map[string]bool{}
			if err := m.code.spill.each(func(f []string) { fromArchive[f[1]+":"+f[2]+" "+f[3]] = true }); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("go", "build", "-o", os.DevNull, "-gcflags="+pkg+"=-S", ".")
			cmd.Dir, cmd.Env = dir, env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			a := &asmReader{}
			defer a.close()
			for _, l := range strings.Split(string(out), "\n") {
				a.line([]byte(l))
			}
			fromCompiler := map[string]bool{}
			if err := a.each(func(rc runtimeCall) {
				if allocatingRuntime.MatchString(rc.fn) {
					fromCompiler[filepath.Base(rc.file)+":"+strconv.Itoa(rc.line)+" "+rc.fn] = true
				}
			}, nil); err != nil {
				t.Fatal(err)
			}
			if len(fromCompiler) == 0 {
				t.Fatal("control: no conversions in the compiler's assembly")
			}
			for c := range fromCompiler {
				if !fromArchive[c] {
					t.Errorf("the compiler wrote a call the archive does not show: %s", c)
				}
			}
			for c := range fromArchive {
				if !fromCompiler[c] {
					t.Errorf("the archive shows a call the compiler did not write: %s", c)
				}
			}
			t.Logf("%d calls", len(fromCompiler))
		})
	}
}

// objdumpLines is objdump's output for n functions, each a conversion's call and
// a few other instructions.
func objdumpLines(n int) []byte {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "TEXT p.f%d(SB) /x/p.go\n", i)
		for j := range 8 {
			fmt.Fprintf(&b, "  p.go:%d\t0x%x\t\t94000000\t\tMOVD R0, R1\t\n", i*8+j, i*64+j)
		}
		fmt.Fprintf(&b, "  p.go:%d\t0x%x\t\t94000000\t\tCALL 0(PC)\t\t[0:4]R_CALLARM64:runtime.slicebytetostring\t\n", i*8, i*64+9)
		fmt.Fprintf(&b, "  p.go:%d\t0x%x\t\td65f03c0\t\tRET\t\t\n", i*8+1, i*64+10)
	}
	return []byte(b.String())
}

// compilerLines is the compiler's -S output for n functions in the same form.
func compilerLines(n int) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf("p.f%d STEXT size=64 args=0x18 locals=0x18 funcid=0x0 align=0x0", i))
		for j := range 8 {
			out = append(out, fmt.Sprintf("\t0x%04x %05d (/x/p.go:%d)\tMOVD\tR0, R1", i*64+j, i*64+j, i*8+j))
		}
		out = append(out, fmt.Sprintf("\t0x%04x %05d (/x/p.go:%d)\tCALL\truntime.slicebytetostring(SB)", i*64+9, i*64+9, i*8))
		out = append(out, fmt.Sprintf("\t0x%04x %05d (/x/p.go:%d)\tCALL\truntime.newobject(SB)", i*64+10, i*64+10, i*8+1))
	}
	return out
}

// heapHeld is the bytes reachable on the heap.
func heapHeld() int64 {
	var ms runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// TestTheCodeReadersHoldAFixedAmount holds each reader of compiled code to the
// same held memory at 1x, 4x, and 16x the code, counted in bytes on the heap, and
// checks that what it read is all still there to read back. The control is a
// reader that keeps every line, which the measure has to catch.
func TestTheCodeReadersHoldAFixedAmount(t *testing.T) {
	const base = 2000
	const slack = 256 << 10
	readers := []struct {
		name  string
		grows bool
		input func(n int) []string
		read  func(t *testing.T, lines []string) (keep any, calls func() int, done func())
	}{
		{name: "the code reader", input: func(n int) []string { return strings.Split(string(objdumpLines(n)), "\n") },
			read: func(t *testing.T, lines []string) (any, func() int, func()) {
				c := newCodeReader()
				s := &codeScan{reader: c, path: "p"}
				for _, l := range lines {
					s.line(l)
				}
				return c, func() int {
					calls := 0
					if err := c.spill.each(func(f []string) { calls++ }); err != nil {
						t.Fatal(err)
					}
					if !c.index.has(callKey("p", "p.go", 8, "slicebytetostring")) || !c.index.has(lineKey("p", "p.go", 9)) {
						t.Error("control: the index lost what it read")
					}
					return calls
				}, c.spill.close
			}},
		{name: "the assembly reader", input: compilerLines,
			read: func(t *testing.T, lines []string) (any, func() int, func()) {
				a := &asmReader{}
				for _, l := range lines {
					a.line([]byte(l))
				}
				return a, func() int {
					calls := 0
					if err := a.each(func(rc runtimeCall) {
						if rc.fn == "slicebytetostring" {
							calls++
						}
					}, nil); err != nil {
						t.Fatal(err)
					}
					return calls
				}, a.close
			}},
		{name: "control: a reader that keeps its lines", grows: true, input: compilerLines,
			read: func(t *testing.T, lines []string) (any, func() int, func()) {
				var kept []string
				for _, l := range lines {
					kept = append(kept, strings.Clone(l))
				}
				return &kept, func() int { return len(kept) / 11 }, func() {}
			}},
	}
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			var held [3]int64
			for i, scale := range []int{1, 4, 16} {
				lines := r.input(base * scale)
				before := heapHeld()
				keep, calls, done := r.read(t, lines)
				held[i] = heapHeld() - before
				if got := calls(); got != base*scale {
					t.Errorf("at %dx, %d calls to read back, want %d", scale, got, base*scale)
				}
				runtime.KeepAlive(lines)
				runtime.KeepAlive(keep)
				done()
			}
			t.Logf("bytes held at 1x, 4x, and 16x: %v", held)
			if grew := held[2] > held[0]+slack; grew != r.grows {
				t.Errorf("bytes held at 1x, 4x, and 16x: %v, growing past %d bytes: %v, want %v", held, slack, grew, r.grows)
			}
		})
	}
}
