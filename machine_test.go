package noalloc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
