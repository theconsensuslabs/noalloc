package noalloc

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
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
	cache    string                        // the directory of archiveCache, or "" for none
	pruned   bool                          // whether a write to cache has pruned it
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

// symbols is the functions of package path's archive.
func (m *machineCode) symbols(path string) map[string]textSym {
	m.load(path)
	return m.text[path]
}

// load reads the archive of package path once: its functions into m.text, and
// its lines into m's code reader when it has one that has not read them.
func (m *machineCode) load(path string) {
	t, haveText := m.text[path]
	var code *codeReader
	if m.code != nil && !m.code.read[path] {
		code = m.code
		m.code.read[path] = true
	}
	if haveText && code == nil {
		return
	}
	if !haveText {
		t = map[string]textSym{}
		m.text[path] = t
	}
	a, ok := m.archives[path]
	if !ok {
		return
	}
	if haveText {
		// Its functions are read already; a disassembly reads them again
		// into a map of its own.
		t = nil
	}
	if err := m.read(path, a, t, code); err != nil && m.err == nil {
		m.err = err
	}
}

// read reads archive a of package path: its functions into t, unless t is nil,
// and its lines into code, unless code is nil. A reading the archive cache holds
// is replayed; any other is disassembled, and saved to the cache.
func (m *machineCode) read(path, a string, t map[string]textSym, code *codeReader) error {
	c := m.archiveCache(path, a)
	if c.file != "" {
		if c.replay(path, t, code) {
			return nil
		}
		if t != nil {
			clear(t)
		}
	}
	w := c.create()
	parsed := t
	if parsed == nil {
		parsed = map[string]textSym{}
	}
	err := m.disassemble(a, func(r io.Reader) error { return m.parseFrom(path, r, parsed, code, w) })
	if err != nil {
		w.abort()
		return err
	}
	w.commit(parsed)
	return nil
}

// parse reads out, objdump's output for package path, into t.
func (m *machineCode) parse(path string, out []byte, t map[string]textSym) {
	var code *codeReader
	if m.code != nil && !m.code.read[path] {
		code = m.code
	}
	m.parseFrom(path, bytes.NewReader(out), t, code, nil)
}

// intern is sym's one copy.
func (m *machineCode) intern(sym string) string {
	if n, ok := m.names[sym]; ok {
		return n
	}
	sym = strings.Clone(sym)
	m.names[sym] = sym
	return sym
}

// parseFrom reads r, objdump's output for package path, into t, each
// function's code by its symbol, and notes in m the functions the compiler
// takes never to return. Its lines go to code, and what it finds to w, when
// they are not nil.
func (m *machineCode) parseFrom(path string, r io.Reader, t map[string]textSym, code *codeReader, w *archiveWriter) error {
	// An assembly file's own symbols, as cmpbody<1>, are named by the
	// package they are in.
	name := func(sym string) string {
		sym = machineName(sym)
		if strings.Contains(sym, "<") {
			sym = path + "." + sym
		}
		return m.intern(sym)
	}
	var scan *codeScan
	if code != nil || w != nil {
		scan = &codeScan{reader: code, path: path, out: w}
	}
	var cur string
	// called is the function the last call went to, until an instruction
	// after it says whether the compiler takes it to return.
	var called string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if scan != nil {
			scan.line(l)
		}
		if h := textHeader.FindStringSubmatch(l); h != nil {
			m.noteNoReturn(path, called, w)
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
				m.noteNoReturn(path, called, w)
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
// on which other archives came before it. w, when not nil, records it.
func (m *machineCode) noteNoReturn(path, called string, w *archiveWriter) {
	if called != "" && (path == "runtime" || symbolPackage(called) == path) {
		m.noReturn[called] = true
		w.record('N', called)
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
	list := toolCommand(m.env, "pack", "t", a)
	names, err := list.Output()
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(names)) {
		if name == "__.PKGDEF" {
			continue
		}
		x := toolCommand(m.env, "pack", "x", a, name)
		x.Dir = tmp
		if err := x.Run(); err != nil {
			return err
		}
		d := toolCommand(m.env, "objdump", filepath.Join(tmp, name))
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

// toolBinary is a binary go tool runs, and a hash of its contents.
type toolBinary struct{ path, id string }

// toolBinaries holds the toolBinary of each tool by the environment and the
// directory the go command runs in, which choose its toolchain.
var toolBinaries sync.Map

// tool is the binary go tool name runs under env, from the working directory, as
// go tool -n names it; or none, when it names none.
func tool(env []string, name string) toolBinary {
	if env == nil {
		env = os.Environ()
	}
	wd, _ := os.Getwd()
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%q", name, wd, env)
	key := string(h.Sum(nil))
	if b, ok := toolBinaries.Load(key); ok {
		return b.(toolBinary)
	}
	var b toolBinary
	cmd := exec.Command("go", "tool", "-n", name)
	cmd.Env = env
	if out, err := cmd.Output(); err == nil {
		path := strings.TrimSpace(string(out))
		if f, err := os.Open(path); err == nil {
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				b = toolBinary{path: path, id: hex.EncodeToString(h.Sum(nil))}
			}
			f.Close()
		}
	}
	got, _ := toolBinaries.LoadOrStore(key, b)
	return got.(toolBinary)
}

// toolCommand runs go tool name with args under env, from the binary go tool -n
// names, which skips starting the go command for each run.
func toolCommand(env []string, name string, args ...string) *exec.Cmd {
	cmd := exec.Command("go", append([]string{"tool", name}, args...)...)
	if b := tool(env, name); b.path != "" {
		cmd = exec.Command(b.path, args...)
	}
	cmd.Env = env
	return cmd
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

// codeScan is a place in one package's code, read for reader and out, either of
// which can be nil.
type codeScan struct {
	reader *codeReader
	path   string
	sym    string
	out    *archiveWriter
	last   string // the position last recorded to out
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
	if s.reader != nil {
		s.reader.addLine(s.path, pos)
	}
	if s.out != nil && pos != s.last {
		s.out.record('L', pos)
		s.last = pos
	}
	if !strings.Contains(instr, "runtime.") {
		return
	}
	m := runtimeCallee.FindStringSubmatch(instr)
	if m == nil || !allocatingRuntime.MatchString(m[1]) {
		return
	}
	if _, err := strconv.Atoi(lineText); err != nil {
		return
	}
	if s.reader != nil {
		s.reader.addCall(s.path, s.sym, file, lineText, m[1])
	}
	s.out.record('C', s.sym, file, lineText, m[1])
}

// addLine notes that the code of package path has code at pos, a file:line.
func (r *codeReader) addLine(path, pos string) {
	r.index.add(path + "\x00" + pos)
}

// addCall notes a call to runtime, a function that can allocate, by sym of
// package path at file:lineText.
func (r *codeReader) addCall(path, sym, file, lineText, runtime string) {
	r.spill.write(sym, file, lineText, runtime)
	if !conversionCalls.MatchString(runtime) {
		return
	}
	line, _ := strconv.Atoi(lineText)
	if testHookCodeCall != nil {
		var ok bool
		if line, ok = testHookCodeCall(sym, file, line, runtime); !ok {
			return
		}
	}
	r.index.add(callKey(path, file, line, runtime))
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
	m.load(path)
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

// archiveCacheBytes bounds the archive cache. A check that writes to it first
// removes the entries used longest ago until those left fit, so the directory
// holds at most this and what the checks running since have written.
const archiveCacheBytes = 256 << 20

// machineRules is ruleHash, which an archive's reading depends on.
var machineRules = sync.OnceValue(ruleHash)

// cacheDir is the directory checks under cfg keep what they work out in.
func cacheDir(cfg Config) (string, error) {
	if cfg.SummaryCache != "" {
		return cfg.SummaryCache, nil
	}
	if testSummaryCache != "" {
		return testSummaryCache, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "noalloc"), nil
}

// machineCacheDir is the directory of the archive cache of checks under cfg, or
// "" for none.
func machineCacheDir(cfg Config) string {
	dir, err := cacheDir(cfg)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "machine")
}

// archiveEntry is where the archive cache keeps the reading of one archive: what
// its code does and which of its lines hold code, never what is judged of it,
// which depends on the allowlist. A file holds the entry of one key.
type archiveEntry struct {
	m         *machineCode
	key, file string
}

// archiveHeader starts each file of the archive cache, followed by its key.
const archiveHeader = "noalloc archive reading"

// archiveCache is m's entry for archive a of package path, keyed by a's
// contents, the objdump that reads it, and the rules it is read by. It names no
// file when m keeps no cache, or go names no objdump.
func (m *machineCode) archiveCache(path, a string) archiveEntry {
	if m.cache == "" {
		return archiveEntry{}
	}
	od := tool(m.env, "objdump")
	if od.id == "" {
		return archiveEntry{}
	}
	f, err := os.Open(a)
	if err != nil {
		return archiveEntry{}
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return archiveEntry{}
	}
	k := sha256.New()
	fmt.Fprintf(k, "%s\n%s\n%s\n%x\n", machineRules(), od.id, path, h.Sum(nil))
	key := hex.EncodeToString(k.Sum(nil))
	return archiveEntry{m: m, key: key, file: filepath.Join(m.cache, key+".code")}
}

// replay reads e's file, the archive of package path: its functions into t,
// unless t is nil, its lines into code, unless code is nil, and its functions
// that never return into e.m. It reports whether the file held the whole entry.
// One that did not can have added some of it, which a reading of the archive
// adds again.
func (e archiveEntry) replay(path string, t map[string]textSym, code *codeReader) bool {
	f, err := os.Open(e.file)
	if err != nil {
		return false
	}
	defer f.Close()
	m := e.m
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || sc.Text() != archiveHeader+"\t"+e.key {
		return false
	}
	var cur string
	var s textSym
	flush := func() {
		if cur != "" {
			t[cur] = s
		}
	}
	for sc.Scan() {
		kind, rest, _ := strings.Cut(sc.Text(), "\t")
		switch kind {
		case "L":
			if code != nil {
				code.addLine(path, rest)
			}
		case "C":
			f := strings.Split(rest, "\t")
			if len(f) != 4 {
				return false
			}
			if code != nil {
				code.addCall(path, f[0], f[1], f[2], f[3])
			}
		case "N":
			m.noReturn[m.intern(rest)] = true
		case "T":
			if t == nil {
				continue
			}
			flush()
			name, flags, _ := strings.Cut(rest, "\t")
			cur = m.intern(name)
			s = textSym{indirect: strings.Contains(flags, "i"), vdso: strings.Contains(flags, "v"), ret: strings.Contains(flags, "r")}
		case "c", "j", "k":
			if t == nil {
				continue
			}
			if cur == "" {
				return false
			}
			to := m.intern(rest)
			switch kind {
			case "c":
				s.targets = append(s.targets, to)
			case "j":
				s.tails = append(s.tails, to)
			default:
				s.closures = append(s.closures, to)
			}
		case "E":
			if t != nil {
				flush()
			}
			now := time.Now()
			os.Chtimes(e.file, now, now)
			return true
		default:
			return false
		}
	}
	return false
}

// archiveWriter writes an entry of the archive cache as the archive is read,
// holding no more of it than a buffer.
type archiveWriter struct {
	e   archiveEntry
	f   *os.File
	out *bufio.Writer
	err error
}

// create starts writing e, or returns nil when e names no file or it cannot be
// written. The cache is only ever a shortcut: failing to write it leaves the
// check to read the archive again next time.
func (e archiveEntry) create() *archiveWriter {
	if e.file == "" {
		return nil
	}
	dir := filepath.Dir(e.file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	f, err := os.CreateTemp(dir, e.key+".*.tmp")
	if err != nil {
		return nil
	}
	w := &archiveWriter{e: e, f: f, out: bufio.NewWriterSize(f, 64<<10)}
	_, w.err = w.out.WriteString(archiveHeader + "\t" + e.key + "\n")
	return w
}

// record writes a line of kind and fields. A field a line cannot hold leaves
// the entry unwritten.
func (w *archiveWriter) record(kind byte, fields ...string) {
	if w == nil || w.err != nil {
		return
	}
	w.out.WriteByte(kind)
	for _, s := range fields {
		if strings.ContainsAny(s, "\t\n") {
			w.err = fmt.Errorf("a field of %c holds a tab or a newline", kind)
			return
		}
		w.out.WriteByte('\t')
		w.out.WriteString(s)
	}
	_, w.err = w.out.WriteString("\n")
}

// abort removes what w wrote.
func (w *archiveWriter) abort() {
	if w == nil {
		return
	}
	w.f.Close()
	os.Remove(w.f.Name())
}

// commit writes t, the archive's functions, and renames the entry into place,
// so that checks writing at once leave one whole file.
func (w *archiveWriter) commit(t map[string]textSym) {
	if w == nil {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(t)) {
		s := t[name]
		var flags []byte
		for _, f := range []struct {
			set bool
			c   byte
		}{{s.indirect, 'i'}, {s.vdso, 'v'}, {s.ret, 'r'}} {
			if f.set {
				flags = append(flags, f.c)
			}
		}
		w.record('T', name, string(flags))
		for _, list := range []struct {
			kind byte
			syms []string
		}{{'c', s.targets}, {'j', s.tails}, {'k', s.closures}} {
			for _, sym := range list.syms {
				w.record(list.kind, sym)
			}
		}
	}
	w.record('E')
	if w.err == nil {
		w.err = w.out.Flush()
	}
	if err := w.f.Close(); w.err == nil {
		w.err = err
	}
	if w.err == nil {
		w.err = os.Rename(w.f.Name(), w.e.file)
	}
	if w.err != nil {
		os.Remove(w.f.Name())
		return
	}
	if m := w.e.m; !m.pruned {
		m.pruned = true
		pruneArchiveCache(filepath.Dir(w.e.file), archiveCacheBytes)
	}
}

// pruneArchiveCache removes the entries of dir used longest ago until those left
// hold at most limit bytes, and any entry still unfinished after an hour.
func pruneArchiveCache(dir string, limit int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type entry struct {
		name string
		size int64
		used time.Time
	}
	var kept []entry
	var total int64
	for _, d := range entries {
		info, err := d.Info()
		if err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(d.Name(), ".tmp") && time.Since(info.ModTime()) > time.Hour:
			os.Remove(filepath.Join(dir, d.Name()))
		case strings.HasSuffix(d.Name(), ".code"):
			kept = append(kept, entry{d.Name(), info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	slices.SortFunc(kept, func(a, b entry) int { return a.used.Compare(b.used) })
	for _, e := range kept {
		if total <= limit {
			break
		}
		if os.Remove(filepath.Join(dir, e.name)) == nil {
			total -= e.size
		}
	}
}
