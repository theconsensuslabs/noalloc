package noalloc

import (
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestMachineCodeFollowsCallsToTheAllocator checks what compiled code says
// about functions written in assembly and in Go: assembly that calls nothing
// allocates nothing, and code that reaches the runtime's allocator does.
func TestMachineCodeFollowsCallsToTheAllocator(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := buildPackages(Config{Dir: dir, Patterns: []string{"bytes", "strconv", "math/big"}}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	archives := map[string]string{}
	for _, p := range pkgs {
		archives[p.path] = p.archive
	}
	m := newMachineCode(os.Environ(), archives)
	for sym, want := range map[string]bool{
		"internal/bytealg.IndexByteString": false,
		"internal/bytealg.IndexByte":       false,
		"bytes.IndexByte":                  false,
		"math/big.nat.make":                true,
		"strconv.Itoa":                     true,
		"runtime.atomicwb":                 false,
		"sync/atomic.StorePointer":         false,
		"runtime.wbBufFlush":               true,
		"runtime.(*fixalloc).alloc":        true,
	} {
		if got := m.allocates(sym); got != want {
			s := m.symbols(symbolPackage(sym))[sym]
			t.Errorf("%s allocates %v, want %v; calls %q, indirect %v", sym, got, want, s.targets, s.indirect)
		}
	}
}

// TestABranchThroughARegisterIsIndirect checks the lines of objdump's output
// that call or jump through a register or memory, on arm64 and amd64, and
// that a branch within the function, or to a symbol it names, is not one.
func TestABranchThroughARegisterIsIndirect(t *testing.T) {
	for l, want := range map[string]bool{
		"  x.s:10\t0x1000\t\td63f0060\t\tCALL (R3)\t":                 true,
		"  x.s:11\t0x1004\t\td61f0060\t\tJMP (R3)\t":                  true,
		"  x.s:12\t0x1008\t\tffd0\t\tCALL AX\t":                       true,
		"  x.s:13\t0x100a\t\tff6008\t\tJMP 0x8(AX)\t":                 true,
		"  x.s:14\t0x100d\t\t14000002\t\tJMP 2(PC)\t":                 false,
		"  x.s:15\t0x1011\t\t54ffff61\t\tBNE -5(PC)\t":                false,
		"  x.s:16\t0x1015\t\t17fffffb\t\tB -5(PC)\t":                  false,
		"  x.s:17\t0x1019\t\taa0103e0\t\tMOVD R1, R0\t":               false,
		"  x.go:97\t0x61599\t\t17ffffed\t\tJMP bytes.IndexByte(SB)\t": false,
	} {
		if _, got := branchTo(l); got != want {
			t.Errorf("%q: through a register %v, want %v", l, got, want)
		}
	}
}

// TestAClosureTheRuntimeRunsIsCharged checks that code calling
// runtime.systemstack is charged with the closures it names, and is taken to
// allocate when it names none, since the closure it hands over came from
// elsewhere.
func TestAClosureTheRuntimeRunsIsCharged(t *testing.T) {
	m := newMachineCode(nil, nil)
	m.text["fixture"] = map[string]textSym{
		"fixture.named":       {targets: []string{"runtime.systemstack", "fixture.named.func1"}, closures: []string{"fixture.named.func1"}, ret: true},
		"fixture.named.func1": {ret: true},
		"fixture.handed":      {targets: []string{"runtime.systemstack"}, ret: true},
	}
	m.text["runtime"] = map[string]textSym{}
	if m.allocates("fixture.named") {
		t.Error("code naming the closure it hands systemstack, which allocates nothing, allocates")
	}
	if !m.allocates("fixture.handed") {
		t.Error("code handing systemstack a closure it does not name allocates nothing")
	}
}

// TestACallTheCompilerEndsNeverReturns checks that a function the compiler
// puts nothing to run after a call to is taken never to return, though its
// own code has a return the call before it never reaches, as runtime.throw's
// has after fatalthrow: what it allocates ends the call it is part of.
func TestACallTheCompilerEndsNeverReturns(t *testing.T) {
	m := newMachineCode(nil, nil)
	out := []byte(`TEXT fixture.fails(SB) x.go
  x.go:1	0x10		94000000		CALL 0(PC)		[0:4]R_CALLARM64:fixture.throw	
  x.go:1	0x14		00000000		?		
TEXT fixture.throw(SB) x.go
  x.go:5	0x20		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.mallocgc	
  x.go:6	0x24		d65f03c0		RET		
TEXT fixture.alloc(SB) x.go
  x.go:9	0x30		94000000		CALL 0(PC)		[0:4]R_CALLARM64:fixture.makes	
  x.go:9	0x34		d65f03c0		RET		
TEXT fixture.makes(SB) x.go
  x.go:12	0x40		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.mallocgc	
  x.go:13	0x44		d65f03c0		RET		
`)
	m.text["fixture"] = map[string]textSym{}
	m.text["runtime"] = map[string]textSym{}
	m.parse("fixture", out, m.text["fixture"])
	if m.allocates("fixture.throw") {
		t.Error("a function a call to is followed by no code allocates")
	}
	if m.allocates("fixture.fails") {
		t.Error("a call to a function that never returns allocates")
	}
	if !m.allocates("fixture.alloc") {
		t.Error("control: a call to a function that reaches mallocgc allocates nothing")
	}
}

// TestMachineCodeFollowsWhatAssemblyCalls checks three things assembly can
// call: a symbol of its own file, as cmpbody<1>, followed to what it does; a
// vDSO function through a register, which allocates nothing; and the
// runtime's stack growth, which takes no heap memory.
func TestMachineCodeFollowsWhatAssemblyCalls(t *testing.T) {
	m := newMachineCode(nil, nil)
	out := []byte(`TEXT body<1>(SB) x.s
  x.s:1	0x10		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.mallocgc	
  x.s:2	0x14		d65f03c0		RET		
TEXT fixture.Body(SB) x.s
  x.s:5	0x20		14000000		JMP 0(PC)		[0:4]R_CALLARM64:body<1>	
TEXT quiet<1>(SB) x.s
  x.s:7	0x30		d65f03c0		RET		
TEXT fixture.Quiet(SB) x.s
  x.s:9	0x40		14000000		JMP 0(PC)		[0:4]R_CALLARM64:quiet<1>	
TEXT fixture.Clock(SB) x.s
  x.s:11	0x50		90000002		ADRP 0(PC), R2		[0:8]R_ADDRARM64:runtime.vdsoClockgettimeSym	
  x.s:12	0x54		d63f0040		CALL (R2)		
  x.s:13	0x58		d65f03c0		RET		
TEXT fixture.Register(SB) x.s
  x.s:15	0x60		d63f0040		CALL (R2)		
  x.s:16	0x64		d65f03c0		RET		
TEXT fixture.Grows(SB) x.go
  x.go:18	0x70		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.morestack_noctxt	
  x.go:19	0x74		d65f03c0		RET		
`)
	m.text["fixture"] = map[string]textSym{}
	m.text["runtime"] = map[string]textSym{}
	m.parse("fixture", out, m.text["fixture"])
	for sym, want := range map[string]bool{"fixture.Body": true, "fixture.Quiet": false, "fixture.Clock": false,
		"fixture.Register": true, "fixture.Grows": false} {
		if got := m.allocates(sym); got != want {
			t.Errorf("%s allocates %v, want %v: %s", sym, got, want, strings.Join(m.chain(sym), " > "))
		}
	}
}

// runtimeFreeCall is a call to a function or method of a standard package
// whose imports stop short of the runtime.
type runtimeFreeCall struct{ pkg, name, expr string }

// runtimeFreeCalls lists a call to everything exported by each importable
// standard package go list -deps says needs no runtime, with the variables its
// arguments point at. A sync/atomic function whose argument it cannot write
// fails the test, so a new one cannot drop out of the table.
func runtimeFreeCalls(t *testing.T) ([]runtimeFreeCall, string) {
	t.Helper()
	list := exec.Command("go", "list", "-f", `{{.ImportPath}} {{.Deps}}`, "std")
	list.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	out, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, deps, _ := strings.Cut(l, " ")
		if path == "unsafe" || runtimePackage(path) || strings.Contains(path, "internal") || strings.HasPrefix(path, "vendor/") ||
			slices.Contains(strings.Fields(strings.Trim(deps, "[]")), "runtime") {
			continue
		}
		paths = append(paths, path)
	}
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedTypes, Env: list.Env}, paths...)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	var decls strings.Builder
	pointee := func(typ string) string {
		v, ok := vars[typ]
		if !ok {
			v = fmt.Sprintf("target%d", len(vars))
			vars[typ] = v
			fmt.Fprintf(&decls, "var %s %s\n", v, typ)
		}
		return v
	}
	args := func(sig *types.Signature) (string, bool) {
		var got []string
		for i := 0; i < sig.Params().Len(); i++ {
			typ := sig.Params().At(i).Type()
			name := typ.String()
			if sig.Variadic() && i == sig.Params().Len()-1 {
				return "", false
			}
			switch p, _ := typ.(*types.Pointer); {
			case name == "any" || name == "interface{}":
				got = append(got, "1")
			case name == "unsafe.Pointer":
				got = append(got, "unsafe.Pointer(&"+pointee("int64")+")")
			case p != nil && (name == "*unsafe.Pointer" || isBasic(p.Elem())):
				got = append(got, "&"+pointee(p.Elem().String()))
			case isBasic(typ):
				got = append(got, "*new("+name+")")
			default:
				if s, ok := typ.(*types.Slice); ok && isBasic(s.Elem()) {
					got = append(got, "nil")
					continue
				}
				return "", false
			}
		}
		return strings.Join(got, ", "), true
	}
	var calls []runtimeFreeCall
	for _, p := range pkgs {
		atomicPkg := p.PkgPath == "sync/atomic"
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			if !token.IsExported(name) {
				continue
			}
			switch o := scope.Lookup(name).(type) {
			case *types.Func:
				sig := o.Type().(*types.Signature)
				if sig.TypeParams().Len() > 0 {
					continue
				}
				if a, ok := args(sig); ok {
					calls = append(calls, runtimeFreeCall{p.PkgPath, p.Name + "." + name, fmt.Sprintf("%s.%s(%s)", p.Name, name, a)})
				} else if atomicPkg {
					t.Errorf("no arguments written for %s.%s", p.PkgPath, name)
				}
			case *types.TypeName:
				if _, isAlias := o.Type().(*types.Alias); isAlias {
					continue
				}
				named, ok := o.Type().(*types.Named)
				if !ok || !atomicPkg {
					continue
				}
				typ, written := types.Type(named), p.Name+"."+name
				if named.TypeParams().Len() > 0 {
					if typ, err = types.Instantiate(nil, named, []types.Type{types.Typ[types.Int64]}, false); err != nil {
						t.Fatal(err)
					}
					written += "[int64]"
				}
				methods := types.NewMethodSet(types.NewPointer(typ))
				for i := 0; i < methods.Len(); i++ {
					m := methods.At(i).Obj().(*types.Func)
					if !m.Exported() {
						continue
					}
					a, ok := args(m.Type().(*types.Signature))
					if !ok {
						t.Errorf("no arguments written for %s.%s", written, m.Name())
						continue
					}
					calls = append(calls, runtimeFreeCall{p.PkgPath, written + "." + m.Name(), fmt.Sprintf("%s.%s(%s)", pointee(written), m.Name(), a)})
				}
			}
		}
	}
	return calls, decls.String()
}

func isBasic(typ types.Type) bool {
	_, ok := typ.(*types.Basic)
	return ok && typ.String() != "unsafe.Pointer"
}

// TestFindingsDoNotDependOnWhatElseImportsTheRuntime holds a library that
// reaches the runtime only through assembly to the result of the same library
// with the runtime imported, for every function of every standard package
// whose imports stop short of it, called from a dependency and from the
// module's own code. Every sync/atomic call also has to allocate nothing when
// measured, and be reported clean in both.
func TestFindingsDoNotDependOnWhatElseImportsTheRuntime(t *testing.T) {
	calls, decls := runtimeFreeCalls(t)
	if !slices.ContainsFunc(calls, func(c runtimeFreeCall) bool { return c.name == "atomic.LoadInt64" }) {
		t.Fatalf("no sync/atomic.LoadInt64 among %d calls", len(calls))
	}
	imports := func(extra ...string) string {
		var paths []string
		for _, c := range calls {
			paths = append(paths, c.pkg)
		}
		paths = append(paths, "unsafe")
		paths = append(paths, extra...)
		slices.Sort(paths)
		var b strings.Builder
		for _, p := range slices.Compact(paths) {
			fmt.Fprintf(&b, "import %q\n", p)
		}
		return b.String()
	}
	var bodies, roots, measures strings.Builder
	for i, c := range calls {
		fmt.Fprintf(&bodies, "//go:noinline\nfunc F%d() int {\n%s\nreturn 0\n}\n\n", i, c.expr)
		fmt.Fprintf(&roots, "//noalloc:root\nfunc Dep%d() int { return dep.F%d() }\n\n", i, i)
		fmt.Fprintf(&roots, "//noalloc:root\nfunc Own%d() int {\n%s\nreturn 0\n}\n\n", i, c.expr)
		if c.pkg == "sync/atomic" {
			fmt.Fprintf(&measures, "fmt.Println(%q, testing.AllocsPerRun(100, func() { F%d() }))\n", c.expr, i)
		}
	}
	env := append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	blank := "var _ unsafe.Pointer\n"

	measure := t.TempDir()
	writeConversionFile(t, measure, "go.mod", "module measure\n\ngo 1.27\n")
	writeConversionFile(t, measure, "main.go", "package main\n\n"+imports("fmt", "testing")+"\n"+blank+decls+bodies.String()+
		"func main() {\n"+measures.String()+"}\n")
	run := exec.Command("go", "run", ".")
	run.Dir, run.Env = measure, env
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("go run: %v\n%s", err, out)
	}
	measured := 0
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		measured++
		if !strings.HasSuffix(l, " 0") {
			t.Errorf("sync/atomic call allocates: %s", l)
		}
	}
	if measured == 0 {
		t.Fatal("no sync/atomic call measured")
	}

	check := func(extra string) map[string][]string {
		dir := t.TempDir()
		writeConversionFile(t, dir, "go.mod", "module callers\n\ngo 1.27\n\nrequire libdep v0.0.0\n\nreplace libdep => ./dep\n")
		writeConversionFile(t, dir, "callers.go", "package callers\n\nimport dep \"libdep\"\n"+imports()+extra+"\n"+blank+decls+roots.String())
		writeConversionFile(t, filepath.Join(dir, "dep"), "go.mod", "module libdep\n\ngo 1.27\n")
		writeConversionFile(t, filepath.Join(dir, "dep"), "dep.go", "package libdep\n\n"+imports()+"\n"+blank+decls+bodies.String())
		res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Env: env, Root: dir,
			Allow: strings.Split(Allowlist, "\n"), SummaryCache: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		found := map[string][]string{}
		for _, f := range res.Findings {
			name := strings.TrimPrefix(f.Func, "callers.")
			found[name] = append(found[name], f.Reasons...)
		}
		return found
	}
	library := check("")
	withRuntime := check("import _ \"runtime\"\n")
	for i, c := range calls {
		for _, name := range []string{fmt.Sprintf("Dep%d", i), fmt.Sprintf("Own%d", i)} {
			if !slices.Equal(library[name], withRuntime[name]) {
				t.Errorf("%s (%s): library alone reports %q, with the runtime imported %q", c.expr, name, library[name], withRuntime[name])
			}
			if c.pkg == "sync/atomic" && len(library[name]) > 0 {
				t.Errorf("%s (%s): measured allocating nothing, reported %q", c.name, name, library[name])
			}
		}
	}
}

// TestDisassemblyFailureIsAnError holds a tool failure apart from a finding: a
// failed disassembly fails the check and leaves nothing in the summary cache
// for the next check to read as an allocation.
func TestDisassemblyFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeConversionFile(t, dir, "go.mod", "module callers\n\ngo 1.27\n")
	writeConversionFile(t, dir, "callers.go", "package callers\n\nimport \"bytes\"\n\n//noalloc:root\nfunc R(b []byte) int { return bytes.IndexByte(b, 1) }\n")
	cfg := Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Root: dir, Env: append(os.Environ(), "GOFLAGS=", "GOWORK=off"),
		Allow: strings.Split(Allowlist, "\n"), SummaryCache: t.TempDir()}

	testHookDisassemble = func(string) error { return errors.New("injected") }
	res, err := Check(cfg)
	testHookDisassemble = nil
	if err == nil {
		t.Errorf("a failed disassembly returned no error, and findings %+v", res.Findings)
	}
	res, err = Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) > 0 {
		t.Errorf("after a failed disassembly, a clean check reports %+v", res.Findings)
	}
}

// TestEveryPatternFormFindsTheSame holds each form Check takes its patterns in
// to the findings of "." for the same code, with and without an overlay.
func TestEveryPatternFormFindsTheSame(t *testing.T) {
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeConversionFile(t, dir, "go.mod", "module callers\n\ngo 1.27\n")
	first := `package callers

import (
	"sync/atomic"
	"unsafe"
)

var (
	counter int64
	target  unsafe.Pointer
	kept    *int64
)

//noalloc:root
func Load() int { return int(atomic.LoadInt64(&counter)) }

//noalloc:root
func Store() { atomic.StorePointer(&target, unsafe.Pointer(&counter)) }

//noalloc:root
func Escape() {
	v := int64(1)
	kept = &v
}

//noalloc:root
func Convert(b []byte) int { return count(string(b)) }

//noalloc:root
func Later(b []byte) int { return later(b) }
`
	writeConversionFile(t, dir, "a.go", first)
	second := "package callers\n\n//go:noinline\nfunc count(s string) int { return len(s) }\n\n//go:noinline\nfunc later(b []byte) int { return len(b) }\n"
	writeConversionFile(t, dir, "b.go", second)
	overlay := map[string][]byte{filepath.Join(dir, "b.go"): []byte(strings.Replace(second, "return len(b)", "return count(string(b))", 1))}

	check := func(patterns []string, overlay map[string][]byte) ([]string, error) {
		res, err := Check(Config{Dir: dir, Patterns: patterns, Module: "callers", Root: dir, Overlay: overlay, SummaryCache: t.TempDir(),
			Env: append(os.Environ(), "GOFLAGS=", "GOWORK=off"), Allow: strings.Split(Allowlist, "\n")})
		if err != nil {
			return nil, err
		}
		var got []string
		for _, f := range res.Findings {
			got = append(got, fmt.Sprintf("%s:%d %q", filepath.Base(f.File), f.Line, f.Reasons))
		}
		slices.Sort(got)
		return got, nil
	}
	for _, o := range []struct {
		name    string
		overlay map[string][]byte
		want    int
	}{{"on disk", nil, 2}, {"overlaid", overlay, 3}} {
		want, err := check([]string{"."}, o.overlay)
		if err != nil {
			t.Fatal(err)
		}
		if len(want) != o.want {
			t.Fatalf("%s: . finds %q, want %d findings", o.name, want, o.want)
		}
		for _, patterns := range [][]string{{"./..."}, {"callers"}, {"a.go", "b.go"}, {"b.go", "a.go"}} {
			got, err := check(patterns, o.overlay)
			if err != nil {
				t.Errorf("%s %q: %v", o.name, patterns, err)
				continue
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s %q: finds %q, . finds %q", o.name, patterns, got, want)
			}
		}
	}
	lone := t.TempDir()
	writeConversionFile(t, lone, "go.mod", "module callers\n\ngo 1.27\n")
	writeConversionFile(t, lone, "lone.go", "package callers\n\nimport \"sync/atomic\"\n\nvar counter int64\n\nvar kept *int64\n\n//noalloc:root\nfunc Load() int { return int(atomic.LoadInt64(&counter)) }\n\n//noalloc:root\nfunc Escape() {\n\tv := int64(1)\n\tkept = &v\n}\n")
	dir = lone
	want, err := check([]string{"."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := check([]string{"lone.go"}, nil)
	if err != nil {
		t.Errorf("one file: %v", err)
	} else if !slices.Equal(got, want) || len(want) != 1 {
		t.Errorf("one file finds %q, . finds %q, want one finding", got, want)
	}
}

// TestMachineCodeStopsAtTheAtomicWriteBarrierOnly holds the stop to the one
// symbol: what it calls allocates by any other route.
func TestMachineCodeStopsAtTheAtomicWriteBarrierOnly(t *testing.T) {
	m := newMachineCode(nil, nil)
	runtimeCode := []byte(`TEXT runtime.atomicwb(SB) x.go
  x.go:1	0x10		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.wbBufFlush	
  x.go:2	0x14		d65f03c0		RET		
TEXT runtime.wbBufFlush(SB) x.go
  x.go:4	0x20		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.(*fixalloc).alloc	
  x.go:5	0x24		d65f03c0		RET		
TEXT runtime.(*fixalloc).alloc(SB) x.go
  x.go:7	0x30		d63f0040		CALL (R2)		
  x.go:8	0x34		d65f03c0		RET		
`)
	fixture := []byte(`TEXT fixture.Barrier(SB) x.go
  x.go:1	0x10		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.atomicwb	
  x.go:2	0x14		d65f03c0		RET		
TEXT fixture.Flush(SB) x.go
  x.go:4	0x20		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.wbBufFlush	
  x.go:5	0x24		d65f03c0		RET		
TEXT fixture.Fixalloc(SB) x.go
  x.go:7	0x30		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.(*fixalloc).alloc	
  x.go:8	0x34		d65f03c0		RET		
TEXT fixture.Both(SB) x.go
  x.go:10	0x40		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.atomicwb	
  x.go:11	0x44		94000000		CALL 0(PC)		[0:4]R_CALLARM64:runtime.wbBufFlush	
  x.go:12	0x48		d65f03c0		RET		
`)
	m.text["fixture"], m.text["runtime"] = map[string]textSym{}, map[string]textSym{}
	m.parse("runtime", runtimeCode, m.text["runtime"])
	m.parse("fixture", fixture, m.text["fixture"])
	for sym, want := range map[string]bool{"fixture.Barrier": false, "fixture.Flush": true, "fixture.Fixalloc": true, "fixture.Both": true} {
		if got := m.allocates(sym); got != want {
			t.Errorf("%s allocates %v, want %v: %s", sym, got, want, strings.Join(m.chain(sym), " > "))
		}
	}
}
