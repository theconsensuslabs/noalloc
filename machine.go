package noalloc

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// machineCode answers what a function allocates from its compiled code: the
// functions it calls and jumps to, followed to the runtime's allocator. It
// reads the archives the linker reads, which hold a package's assembly as
// well as its compiled Go.
type machineCode struct {
	// assumed reports whether a symbol that would allocate is labeled to
	// allocate nothing, and enter and leave, when set, see each symbol as
	// it is judged.
	assumed      func(sym string) bool
	enter, leave func(sym string)
	env          []string
	archives     map[string]string             // package path -> archive
	text         map[string]map[string]textSym // package path -> symbol -> code
	alloc        memo[string]
	ret          memo[string]
	// noReturn holds the functions some call to is followed by no code to
	// run, as the compiler compiles a call it knows never returns.
	noReturn map[string]bool
	// whys holds, for each symbol that allocates, the first step found
	// toward what allocates, and the symbol to follow from it.
	whys map[string][2]string
}

// textSym is what one function's code does that can allocate: the symbols
// it calls or jumps to, whether it calls through a register, and whether it
// returns, or jumps on to a function that can.
type textSym struct {
	targets  []string
	indirect bool
	// vdso is whether it loads the address of a vDSO function, which the
	// runtime calls through a register and allocates nothing.
	vdso     bool
	ret      bool     // it has a return instruction
	tails    []string // the functions it jumps to, returning as they do
	closures []string // the closures it takes the address of
}

// newMachineCode reads code from archives, each package's from go list
// -export: the archives the build being checked compiles and links.
func newMachineCode(env []string, archives map[string]string) *machineCode {
	return &machineCode{env: env, archives: archives, text: map[string]map[string]textSym{}, whys: map[string][2]string{},
		noReturn: map[string]bool{}}
}

var (
	textHeader = regexp.MustCompile(`^TEXT (\S+)\(SB\)`)
	reloc      = regexp.MustCompile(`R_(?:CALL|JMP)[A-Z0-9]*:(\S+)`)
	vdsoSym    = regexp.MustCompile(`runtime\.vdso\w*Sym\b`)
	// closureRef is a function's code taking the address of a closure,
	// which it can call or hand to code that calls it, as the runtime's
	// systemstack does.
	closureRef = regexp.MustCompile(`R_(?:ADDR|PCREL|ARM64_PCREL)[A-Z0-9_]*:([^\s<+]+\.func\d+(?:\.\d+)*)(?:·f)?(?:\s|$)`)
	// branch is a call or jump in objdump's assembly column, with what it
	// goes to: a symbol, a place in the function, or a register.
	branch   = regexp.MustCompile(`^(CALL|BL|BLR|JMP|B|BR) (\S+)`)
	relative = regexp.MustCompile(`^-?\d+\(PC\)$|^0x[0-9a-f]+$`)
)

// symbols disassembles the archive of package path.
func (m *machineCode) symbols(path string) map[string]textSym {
	if t, ok := m.text[path]; ok {
		return t
	}
	t := map[string]textSym{}
	m.text[path] = t
	a, ok := m.archives[path]
	if !ok {
		return t
	}
	out, err := m.disassemble(a)
	if err != nil {
		return t
	}
	m.parse(path, out, t)
	return t
}

// parse reads out, objdump's output for package path, into t, each
// function's code by its symbol, and notes in m the functions the compiler
// takes never to return.
func (m *machineCode) parse(path string, out []byte, t map[string]textSym) {
	// An assembly file's own symbols, as cmpbody<1>, are named by the
	// package they are in.
	name := func(sym string) string {
		sym = machineName(sym)
		if strings.Contains(sym, "<") {
			return path + "." + sym
		}
		return sym
	}
	var cur string
	// called is the function the last call went to, until an instruction
	// after it says whether the compiler takes it to return.
	var called string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if h := textHeader.FindStringSubmatch(l); h != nil {
			if called != "" {
				m.noReturn[called] = true
			}
			called = ""
			cur = name(h[1])
			if _, ok := t[cur]; !ok {
				t[cur] = textSym{}
			}
			continue
		}
		if cur == "" {
			continue
		}
		op := mnemonic(l)
		if called != "" {
			// The compiler puts nothing to run after a call to a function
			// it knows never returns, as it knows runtime.throw.
			switch op {
			case "NOOP", "":
			case "?", "UNDEF", "BRK", "INT3", "UD2":
				m.noReturn[called], called = true, ""
			default:
				called = ""
			}
		}
		if op == "CALL" || op == "BL" {
			if r := reloc.FindStringSubmatch(l); r != nil {
				called = name(r[1])
			}
		}
		s := t[cur]
		switch {
		case op == "RET":
			s.ret = true
		case op == "JMP" || op == "B":
			// A jump to another function, not back to this one's start
			// after its stack grows, returns as that function does.
			to := ""
			if r := reloc.FindStringSubmatch(l); r != nil {
				to = name(r[1])
			} else if sym, _ := branchTo(l); sym != "" {
				to = name(sym)
			}
			if to != "" && to != cur {
				s.tails = append(s.tails, to)
			}
		}
		if vdsoSym.MatchString(l) {
			s.vdso = true
		}
		if r := reloc.FindStringSubmatch(l); r != nil {
			s.targets = append(s.targets, name(r[1]))
		} else if r := closureRef.FindStringSubmatch(l); r != nil {
			s.targets = append(s.targets, name(r[1]))
			s.closures = append(s.closures, name(r[1]))
		} else if to, indirect := branchTo(l); indirect {
			s.indirect = true
		} else if to != "" {
			s.targets = append(s.targets, name(to))
		}
		t[cur] = s
	}
}

// returns reports whether the code of sym can return to its caller: it has
// a return instruction, or jumps on to a function that can.
func (m *machineCode) returns(sym string) bool {
	return m.ret.get(sym, false, func() bool {
		s, ok := m.symbols(symbolPackage(sym))[sym]
		if !ok {
			s, ok = m.symbols("runtime")[sym]
		}
		if !ok || s.ret {
			return true
		}
		return slices.ContainsFunc(s.tails, m.returns)
	})
}

// objdumpFields are the tab-separated columns of l, a line of objdump's
// output: the position, address, encoding, and instruction, then any
// relocation.
func objdumpFields(l string) []string {
	var fields []string
	for _, f := range strings.Split(l, "\t") {
		if f = strings.TrimSpace(f); f != "" {
			fields = append(fields, f)
		}
	}
	return fields
}

// mnemonic is the instruction of l, a line of objdump's output.
func mnemonic(l string) string {
	fields := objdumpFields(l)
	if len(fields) < 4 {
		return ""
	}
	op, _, _ := strings.Cut(fields[3], " ")
	return op
}

// branchTo reads l, a line of objdump's output with no relocation, for a
// call or jump out of its place in the function: the symbol it names, or
// indirect when it goes to an address held in a register or memory.
func branchTo(l string) (sym string, indirect bool) {
	fields := objdumpFields(l)
	if len(fields) < 4 {
		return "", false
	}
	m := branch.FindStringSubmatch(fields[3])
	switch {
	case m == nil || relative.MatchString(m[2]):
		return "", false
	case strings.HasSuffix(m[2], "(SB)"):
		return strings.TrimSuffix(m[2], "(SB)"), false
	}
	return "", true
}

// disassemble returns the code of every object in archive a: objdump reads
// only an archive's compiled Go, and its assembly is in objects of its own.
func (m *machineCode) disassemble(a string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "noalloc-objects")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	list := exec.Command("go", "tool", "pack", "t", a)
	list.Env = m.env
	names, err := list.Output()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for _, name := range strings.Fields(string(names)) {
		if name == "__.PKGDEF" {
			continue
		}
		x := exec.Command("go", "tool", "pack", "x", a, name)
		x.Dir, x.Env = tmp, m.env
		if err := x.Run(); err != nil {
			return nil, err
		}
		d := exec.Command("go", "tool", "objdump", filepath.Join(tmp, name))
		d.Env = m.env
		b, err := d.Output()
		if err != nil {
			return nil, fmt.Errorf("disassembling %s of %s: %v", name, a, err)
		}
		out.Write(b)
	}
	return out.Bytes(), nil
}

// machineName is a symbol without the suffixes that name its ABI.
func machineName(sym string) string {
	for _, s := range []string{".abi0", ".abiinternal"} {
		sym = strings.TrimSuffix(sym, s)
	}
	return sym
}

// symbolPackage is the package path a symbol belongs to.
func symbolPackage(sym string) string {
	slash := strings.LastIndex(sym, "/")
	if i := strings.Index(sym[slash+1:], "."); i >= 0 {
		return sym[:slash+1+i]
	}
	return sym
}

// allocates reports whether the code of sym can allocate on the heap: it
// reaches the runtime's allocator, a call through a register, or code that
// cannot be found. Code that never returns, as a panic's or a fatal error's,
// ends the call it is part of, and what it allocates goes with it.
func (m *machineCode) allocates(sym string) bool {
	if m.enter != nil {
		m.enter(sym)
		defer m.leave(sym)
	}
	return m.reaches(sym) && (m.assumed == nil || !m.assumed(sym))
}

// reaches is allocates of sym by its code alone, with what it calls judged
// by allocates.
func (m *machineCode) reaches(sym string) bool {
	return m.alloc.get(sym, false, func() bool {
		pkg := symbolPackage(sym)
		if pkg == "runtime" {
			name := strings.TrimPrefix(sym, "runtime.")
			if allocator.MatchString(name) {
				return m.because(sym, sym+" is the runtime's allocator", "")
			}
			if callsC.MatchString(name) || growsStack.MatchString(name) {
				return false
			}
		}
		s, ok := m.symbols(pkg)[sym]
		if !ok {
			// A function the runtime provides under another package's
			// name, with go:linkname, is in the runtime's archive.
			s, ok = m.symbols("runtime")[sym]
		}
		if !ok {
			return m.because(sym, "no code for "+sym, "")
		}
		if !m.returns(sym) || m.noReturn[sym] {
			return false
		}
		if s.indirect && !s.vdso {
			return m.because(sym, sym+" calls through a register", "")
		}
		// Code that hands the runtime a closure to run is charged with the
		// closures it names; one it names none of came from elsewhere.
		if len(s.closures) == 0 && slices.ContainsFunc(s.targets, func(t string) bool {
			return symbolPackage(t) == "runtime" && callsC.MatchString(strings.TrimPrefix(t, "runtime.")) && !strings.HasPrefix(t, "runtime.asmcgocall")
		}) {
			return m.because(sym, sym+" hands the runtime a closure it does not name", "")
		}
		for _, t := range s.targets {
			if t != sym && m.allocates(t) {
				return m.because(sym, sym+" calls "+t, t)
			}
		}
		return false
	})
}

// callsC are the runtime functions that call through a register what they
// are handed: C code, which allocates nothing on Go's heap, or a closure,
// which the code that takes its address is taken to call.
var callsC = regexp.MustCompile(`^(asmcgocall(_no_g)?|systemstack|mcall)$`)

// growsStack are the runtime functions that grow a goroutine's stack, which
// takes memory from the runtime's stack pools, not the heap.
var growsStack = regexp.MustCompile(`^morestack(_noctxt)?$`)

// because notes why sym allocates, and the symbol to follow, and reports
// that it does.
func (m *machineCode) because(sym, step, next string) bool {
	if _, ok := m.whys[sym]; !ok {
		m.whys[sym] = [2]string{step, next}
	}
	return true
}

// chain is why sym allocates, from sym to the allocation.
func (m *machineCode) chain(sym string) []string {
	var steps []string
	for range 32 {
		w, ok := m.whys[sym]
		if !ok {
			break
		}
		steps = append(steps, w[0])
		if w[1] == "" {
			break
		}
		sym = w[1]
	}
	return steps
}

// allocator is the runtime's heap allocator, which is what allocating
// means: every other allocation reaches it.
var allocator = regexp.MustCompile(`^mallocgc`)
