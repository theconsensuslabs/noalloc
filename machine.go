package noalloc

import (
	"bufio"
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	assumed  func(sym string) bool
	enter    func(sym string)
	leave    func(sym string, allocates bool)
	env      []string
	archives map[string]string             // package path -> archive
	err      error                         // the first archive that could not be read
	text     map[string]map[string]textSym // package path -> symbol -> code
	code     *codeReader                   // which lines of which packages hold code
	names    map[string]string             // each symbol's one copy
	alloc    memo[string]
	ret      memo[string]
	// noReturn holds the functions some call to is followed by no code to
	// run, as the compiler compiles a call it knows never returns.
	noReturn map[string]bool
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

// testHookDisassemble, when set, can fail a disassembly of an archive.
var testHookDisassemble func(archive string) error

// newMachineCode reads code from archives, each package's from go list
// -export: the archives the build being checked compiles and links.
func newMachineCode(env []string, archives map[string]string) *machineCode {
	return &machineCode{env: env, archives: archives, text: map[string]map[string]textSym{}, noReturn: map[string]bool{},
		names: map[string]string{}}
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
	read := func(r io.Reader) error { return m.parseFrom(path, r, t) }
	if err := m.disassemble(a, read); err != nil && m.err == nil {
		m.err = err
	}
	if m.code != nil {
		m.code.read[path] = true
	}
	return t
}

// parse reads out, objdump's output for package path, into t.
func (m *machineCode) parse(path string, out []byte, t map[string]textSym) {
	m.parseFrom(path, bytes.NewReader(out), t)
}

// parseFrom reads r, objdump's output for package path, into t, each
// function's code by its symbol, and notes in m the functions the compiler
// takes never to return.
func (m *machineCode) parseFrom(path string, r io.Reader, t map[string]textSym) error {
	// An assembly file's own symbols, as cmpbody<1>, are named by the
	// package they are in.
	name := func(sym string) string {
		sym = machineName(sym)
		if strings.Contains(sym, "<") {
			sym = path + "." + sym
		}
		if n, ok := m.names[sym]; ok {
			return n
		}
		sym = strings.Clone(sym)
		m.names[sym] = sym
		return sym
	}
	var code *codeScan
	if m.code != nil && !m.code.read[path] {
		code = &codeScan{reader: m.code, path: path}
	}
	var cur string
	// called is the function the last call went to, until an instruction
	// after it says whether the compiler takes it to return.
	var called string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if code != nil {
			code.line(l)
		}
		if h := textHeader.FindStringSubmatch(l); h != nil {
			m.noteNoReturn(path, called)
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
				m.noteNoReturn(path, called)
				called = ""
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
	return sc.Err()
}

// noteNoReturn notes that code of package path follows its call to called with
// nothing to run. Only called's own package and the runtime count, whose
// archives a judgment of called has always read, so the answer does not depend
// on which other archives came before it.
func (m *machineCode) noteNoReturn(path, called string) {
	if called != "" && (path == "runtime" || symbolPackage(called) == path) {
		m.noReturn[called] = true
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

// disassemble hands read the code of each object in archive a, as it is
// written: objdump reads only an archive's compiled Go, and its assembly is in
// objects of its own.
func (m *machineCode) disassemble(a string, read func(io.Reader) error) error {
	if testHookDisassemble != nil {
		if err := testHookDisassemble(a); err != nil {
			return err
		}
	}
	tmp, err := os.MkdirTemp("", "noalloc-objects")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	list := exec.Command("go", "tool", "pack", "t", a)
	list.Env = m.env
	names, err := list.Output()
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(names)) {
		if name == "__.PKGDEF" {
			continue
		}
		x := exec.Command("go", "tool", "pack", "x", a, name)
		x.Dir, x.Env = tmp, m.env
		if err := x.Run(); err != nil {
			return err
		}
		d := exec.Command("go", "tool", "objdump", filepath.Join(tmp, name))
		d.Env = m.env
		out, err := d.StdoutPipe()
		if err != nil {
			return err
		}
		if err := d.Start(); err != nil {
			return err
		}
		readErr := read(out)
		io.Copy(io.Discard, out)
		if err := d.Wait(); err != nil {
			return fmt.Errorf("disassembling %s of %s: %v", name, a, err)
		}
		if readErr != nil {
			return fmt.Errorf("reading the code of %s of %s: %v", name, a, readErr)
		}
		os.Remove(filepath.Join(tmp, name))
	}
	return nil
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
	}
	v := m.reaches(sym)
	if m.leave != nil {
		m.leave(sym, v)
	}
	return v && (m.assumed == nil || !m.assumed(sym))
}

// reaches is allocates of sym by its code alone, with what it calls judged
// by allocates.
func (m *machineCode) reaches(sym string) bool {
	return m.alloc.get(sym, false, func() bool {
		found := false
		m.scan(sym, func(string, string) bool {
			found = true
			return false
		})
		return found
	})
}

// scan calls yield with each reason the code of sym allocates, in the order of
// that code, and the symbol to follow from it, until yield returns false.
func (m *machineCode) scan(sym string, yield func(step, next string) bool) {
	pkg := symbolPackage(sym)
	if pkg == "runtime" {
		name := strings.TrimPrefix(sym, "runtime.")
		if allocator.MatchString(name) {
			yield(sym+" is the runtime's allocator", "")
			return
		}
		if callsC.MatchString(name) || growsStack.MatchString(name) || name == atomicWriteBarrier {
			return
		}
	}
	runtime := m.symbols("runtime")
	s, ok := m.symbols(pkg)[sym]
	if !ok {
		// A function the runtime provides under another package's
		// name, with go:linkname, is in the runtime's archive.
		s, ok = runtime[sym]
	}
	if !ok {
		yield("no code for "+sym, "")
		return
	}
	if !m.returns(sym) || m.noReturn[sym] {
		return
	}
	if s.indirect && !s.vdso {
		yield(sym+" calls through a register", "")
		return
	}
	// Code that hands the runtime a closure to run is charged with the
	// closures it names; one it names none of came from elsewhere.
	if len(s.closures) == 0 && slices.ContainsFunc(s.targets, func(t string) bool {
		return symbolPackage(t) == "runtime" && callsC.MatchString(strings.TrimPrefix(t, "runtime.")) && !strings.HasPrefix(t, "runtime.asmcgocall")
	}) {
		yield(sym+" hands the runtime a closure it does not name", "")
		return
	}
	for _, t := range s.targets {
		if t != sym && m.allocates(t) && !yield(sym+" calls "+t, t) {
			return
		}
	}
}

// callsC are the runtime functions that call through a register what they
// are handed: C code, which allocates nothing on Go's heap, or a closure,
// which the code that takes its address is taken to call.
var callsC = regexp.MustCompile(`^(asmcgocall(_no_g)?|systemstack|mcall)$`)

// growsStack are the runtime functions that grow a goroutine's stack, which
// takes memory from the runtime's stack pools, not the heap.
var growsStack = regexp.MustCompile(`^morestack(_noctxt)?$`)

// atomicWriteBarrier is the write barrier of sync/atomic's pointer stores, as
// gcWriteBarrier is of the compiler's. Its flush feeds the collector and takes
// no memory from the heap for the caller.
const atomicWriteBarrier = "atomicwb"

// chain is why sym allocates, from sym to the allocation: the first way in
// the order of the code, which does not depend on what was judged before it.
func (m *machineCode) chain(sym string) []string {
	steps, _ := m.explain(sym, map[string]bool{})
	return steps[:min(len(steps), machineChainSteps)]
}

// machineChainSteps bounds how many steps a chain is written with.
const machineChainSteps = 32

// explain is the steps from sym to an allocation that pass through no symbol
// in seen, and whether there are any.
func (m *machineCode) explain(sym string, seen map[string]bool) ([]string, bool) {
	if seen[sym] {
		return nil, false
	}
	seen[sym] = true
	var out []string
	found := false
	m.scan(sym, func(step, next string) bool {
		if next == "" {
			out, found = []string{step}, true
			return false
		}
		if rest, ok := m.explain(next, seen); ok {
			out, found = append([]string{step}, rest...), true
			return false
		}
		return true
	})
	return out, found
}

// allocator is the runtime's heap allocator, which is what allocating
// means: every other allocation reaches it.
var allocator = regexp.MustCompile(`^mallocgc`)

// codeIndexWords sizes codeIndex. At a million lines of code one lookup in about
// 75,000 answers yes wrongly.
const codeIndexWords = 1 << 20

// codeIndex is a set of a fixed size: a line it holds it always says it holds,
// and one it does not it can, rarely, say it does. A wrong yes to "does this line
// call the runtime" costs a finding too many.
type codeIndex []uint64

func (x codeIndex) probe(key string, visit func(word int, bit uint64) bool) bool {
	f := fnv.New64a()
	f.Write([]byte(key))
	h := f.Sum64()
	h1, h2 := h&0xffffffff, h>>32|1
	for i := range uint64(4) {
		at := h1 + i*h2
		if !visit(int((at>>6)%uint64(len(x))), 1<<(at&63)) {
			return false
		}
	}
	return true
}

func (x codeIndex) add(key string) {
	x.probe(key, func(word int, bit uint64) bool {
		x[word] |= bit
		return true
	})
}

func (x codeIndex) has(key string) bool {
	return x.probe(key, func(word int, bit uint64) bool { return x[word]&bit != 0 })
}

// codeReader reads which lines of each package's compiled code hold code, and
// which call a runtime function that makes a conversion. However much code it
// reads it holds a fixed amount: the index, and a buffer on its way to a file of
// those calls, which is read back once.
type codeReader struct {
	index codeIndex
	read  map[string]bool
	spill *spillFile
}

func newCodeReader() *codeReader {
	return &codeReader{index: make(codeIndex, codeIndexWords), read: map[string]bool{}, spill: &spillFile{}}
}

// codeScan is a codeReader's place in one package's code.
type codeScan struct {
	reader *codeReader
	path   string
	sym    string
}

// conversionCalls are the runtime functions that make a conversion or a
// concatenation.
var conversionCalls = regexp.MustCompile(`^(slicebytetostring|slicerunetostring|stringtoslicebyte|stringtoslicerune|intstring|concatstring.*|concatbyte.*)$`)

var runtimeCallee = regexp.MustCompile(`R_CALL[A-Z0-9]*:runtime\.([A-Za-z0-9_]+)`)

// testHookCodeCall, when set, sees each conversion's call as it enters the
// index: it says the line to index it at, or none.
var testHookCodeCall func(sym, file string, line int, runtime string) (int, bool)

// line reads l, a line of objdump's output.
func (s *codeScan) line(l string) {
	if rest, ok := strings.CutPrefix(l, "TEXT "); ok {
		s.sym, _, _ = strings.Cut(rest, "(SB)")
		s.sym = machineName(s.sym)
		return
	}
	pos, instr, ok := strings.Cut(strings.TrimLeft(l, " "), "\t")
	if !ok || s.sym == "" {
		return
	}
	file, lineText, ok := strings.Cut(pos, ":")
	if !ok {
		return
	}
	s.reader.index.add(s.path + "\x00" + pos)
	if !strings.Contains(instr, "runtime.") {
		return
	}
	m := runtimeCallee.FindStringSubmatch(instr)
	if m == nil || !allocatingRuntime.MatchString(m[1]) {
		return
	}
	line, err := strconv.Atoi(lineText)
	if err != nil {
		return
	}
	s.reader.spill.write(s.sym, file, lineText, m[1])
	if !conversionCalls.MatchString(m[1]) {
		return
	}
	if testHookCodeCall != nil {
		if line, ok = testHookCodeCall(s.sym, file, line, m[1]); !ok {
			return
		}
	}
	s.reader.index.add(callKey(s.path, file, line, m[1]))
}

func callKey(path, file string, line int, runtime string) string {
	return lineKey(path, file, line) + "\x00" + runtime
}

func lineKey(path, file string, line int) string {
	return path + "\x00" + file + ":" + strconv.Itoa(line)
}

// spillFile holds records of tab-separated fields on disk, past a fixed buffer,
// until they are read back.
type spillFile struct {
	file *os.File
	out  *bufio.Writer
	err  error
}

// write adds a record.
func (f *spillFile) write(fields ...string) {
	if f.err != nil {
		return
	}
	if f.file == nil {
		if f.file, f.err = os.CreateTemp("", "noalloc-spill"); f.err != nil {
			return
		}
		f.out = bufio.NewWriterSize(f.file, 64<<10)
	}
	for i, s := range fields {
		if i > 0 {
			f.out.WriteByte('\t')
		}
		f.out.WriteString(s)
	}
	_, f.err = f.out.WriteString("\n")
}

// each calls yield with each record written so far, whose fields are valid until
// it returns.
func (f *spillFile) each(yield func(fields []string)) error {
	if f.err != nil || f.file == nil {
		return f.err
	}
	if err := f.out.Flush(); err != nil {
		return err
	}
	if _, err := f.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	defer f.file.Seek(0, io.SeekEnd)
	sc := bufio.NewScanner(f.file)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		yield(strings.Split(sc.Text(), "\t"))
	}
	return sc.Err()
}

// close removes the file.
func (f *spillFile) close() {
	if f.file != nil {
		f.file.Close()
		os.Remove(f.file.Name())
		f.file = nil
	}
}

// lines has the code reader read the archive of package path, when nothing
// has yet.
func (m *machineCode) lines(path string) {
	if m.code.read[path] {
		return
	}
	m.code.read[path] = true
	a, ok := m.archives[path]
	if !ok {
		return
	}
	read := func(r io.Reader) error {
		code := &codeScan{reader: m.code, path: path}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			code.line(sc.Text())
		}
		return sc.Err()
	}
	if err := m.disassemble(a, read); err != nil && m.err == nil {
		m.err = err
	}
}

// withoutCopy reports whether the code of package path compiles the function on
// lines body of file, and no call to runtime is on lines from to to: the
// conversion written there is compiled without it. Code the package did not
// compile, as a generic body's is when only another package instantiates it, says
// nothing.
func (m *machineCode) withoutCopy(path, file string, from, to int, body [2]int, runtime string) bool {
	m.lines(path)
	for line := from; line <= to; line++ {
		if m.code.index.has(callKey(path, file, line, runtime)) {
			return false
		}
	}
	for line := body[0]; line <= body[1]; line++ {
		if m.code.index.has(lineKey(path, file, line)) {
			return true
		}
	}
	return false
}
