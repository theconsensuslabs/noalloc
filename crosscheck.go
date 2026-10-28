package noalloc

import (
	"fmt"
	"go/types"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// CrossCheck holds the compiled code of the reached functions to what Check
// found: every call it makes to a runtime function that can allocate should
// be on a line Check flagged, on a line it does not check, or growing memory
// Check holds to be reused.
type CrossCheck struct {
	// Calls are the allocating runtime calls nothing above explains.
	Calls []RuntimeCall
	// Buffered are calls on lines Check passed to a runtime function that
	// allocates only when the compiler gives it no stack buffer, which it
	// does only for a result escape analysis reports as escaping.
	Buffered []RuntimeCall
	// Unsorted counts the calls reached functions make to runtime functions
	// known neither to allocate nor not to, by function.
	Unsorted map[string]int
	// Mixed counts allocating calls on a line that also holds code Check does
	// not check, which a line number cannot tell apart from the rest.
	Mixed int
	// Elsewhere counts allocating calls in reached functions at a line outside
	// the module: code inlined from another package, which the compiler places
	// at the inlined line rather than at the call.
	Elsewhere int
	// Unmatched are compiled functions of the module that name no function
	// Check knows, so their calls go unchecked.
	Unmatched []string
	// Flagged and Reused count the lines whose allocating calls only a
	// finding, or only reused memory, explains: a control that the check saw
	// the allocations it should.
	Flagged, Reused int

	// flagged and reused are the lines whose allocating calls only a finding
	// or reused memory explains, so a test can see the check would catch them.
	flagged, reused map[string]bool
}

// RuntimeCall is a call to an allocating runtime function in compiled code.
type RuntimeCall struct {
	File    string
	Line    int
	Source  string
	Func    string // as the compiler names it
	Runtime string
}

func (r RuntimeCall) String() string {
	return fmt.Sprintf("%s:%d: %s: calls runtime.%s", r.File, r.Line, r.Func, r.Runtime)
}

// runtimeCall is one call to a runtime function in the compiler's -S output.
type runtimeCall struct {
	symbol, file string
	line         int
	fn           string
}

// How a line of a reached function is checked: some of its code checked,
// some not, and an append or map write on it allowed as reused memory.
const (
	lineChecked = 1 << iota
	lineUnchecked
	lineReused
)

var allocatingRuntime = regexp.MustCompile(`^(growslice|newobject|makeslice|makeslice64|makeslicecopy|makemap|makemap64|makemap_small|` +
	`makechan|makechan64|mallocgc.*|convT.*|mapassign.*|concatstring.*|concatbyte.*|slicebytetostring|stringtoslicebyte|` +
	`stringtoslicerune|slicerunetostring|intstring|newproc|deferproc|rawstring.*)$`)

// bufferedRuntime are the allocating runtime functions whose stack buffer
// holds any result: a string of one rune.
var bufferedRuntime = regexp.MustCompile(`^intstring$`)

// nonAllocatingRuntime are runtime functions that allocate nothing, or only
// on a panic's path, which Check allows: gopanic and gorecover.
var nonAllocatingRuntime = regexp.MustCompile(`^(gcWriteBarrier.*|wbZero|wbMove|morestack.*|panic[A-Za-z0-9]*|goPanic.*|gopanic|gorecover|` +
	`memequal.*|memmove|memclr.*|mapaccess.*|mapdelete.*|mapIterStart|mapIterNext|mapclear|cmpstring|strhash|memhash.*|` +
	`interhash|nilinterhash|ifaceeq|efaceeq|decoderune|encoderune|countrunes|rand|chanlen|chancap|typedslicecopy|` +
	`typedmemmove|typedmemclr|moveSlice.*|deferreturn|deferprocStack|selectnbsend|selectnbrecv|slicebytetostringtmp|duffzero|duffcopy)$`)

// recordLines notes how each line of fn is checked.
func (c *checker) recordLines(fn *ssa.Function) {
	ok := c.blocks[fn]
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			pos := in.Pos()
			if call, isCall := in.(ssa.CallInstruction); isCall && !pos.IsValid() {
				pos = call.Common().Pos()
			}
			if !pos.IsValid() {
				continue
			}
			p := c.fset.Position(pos)
			if c.lineState[p.Filename] == nil {
				c.lineState[p.Filename] = map[int]uint8{}
			}
			switch {
			case !ok[b] || c.exemptAt(pos):
				c.lineState[p.Filename][p.Line] |= lineUnchecked
			case c.growsReused(in):
				c.lineState[p.Filename][p.Line] |= lineChecked | lineReused
			default:
				c.lineState[p.Filename][p.Line] |= lineChecked
			}
		}
	}
}

// growsReused reports whether in appends to or writes a map in memory that
// is reused, which can grow it.
func (c *checker) growsReused(in ssa.Instruction) bool {
	if mu, ok := in.(*ssa.MapUpdate); ok {
		return c.reused(mu.Map)
	}
	call, ok := in.(ssa.CallInstruction)
	if !ok {
		return false
	}
	cm := call.Common()
	if b, ok := cm.Value.(*ssa.Builtin); ok {
		return b.Name() == "append" && len(cm.Args) > 1 && c.reused(cm.Args[0])
	}
	callees, _ := c.expand(c.sites[call], map[*ssa.Function]bool{})
	for _, callee := range callees {
		if c.appendRoot(callee) || c.returnsAppended(callee) {
			if dst := appendedTo(cm, callee); dst != nil && c.reused(dst) {
				return true
			}
		}
	}
	return false
}

// crossCheck holds calls, from the compiled module, to what Check found.
func (c *checker) crossCheck(calls []runtimeCall, starts map[string]asmPos, found map[string]map[int]*Finding) *CrossCheck {
	reached, known := map[string]bool{}, map[string]bool{}
	names := map[string]string{}
	for _, fn := range c.order {
		if c.ours(fn) && fn.Blocks != nil {
			reached[symbolOf(fn)] = true
			names[symbolOf(fn)] = c.stableName(fn)
		}
	}
	// A closure or a range-over-func loop body the compiler numbers apart from
	// SSA is found where it starts instead.
	nested := map[asmPos][]*ssa.Function{}
	for fn := range ssautil.AllFunctions(c.prog) {
		if !c.ours(fn) {
			continue
		}
		known[symbolOf(fn)] = true
		if fn.Parent() != nil && fn.Pos().IsValid() {
			p := c.fset.Position(fn.Pos())
			nested[asmPos{p.Filename, p.Line}] = append(nested[asmPos{p.Filename, p.Line}], fn)
		}
	}
	var prefixes []string
	for _, p := range c.modulePkgs {
		if p.Name() == "main" {
			prefixes = append(prefixes, "main.")
		} else {
			prefixes = append(prefixes, p.Path()+".")
		}
	}
	ours := func(sym string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(sym, p) })
	}

	// The compiler and the loader can name one file by two paths, as when a
	// temporary directory is reached through a symbolic link.
	resolved := map[string]string{}
	resolve := func(path string) string {
		if r, ok := resolved[path]; ok {
			return r
		}
		r, err := filepath.EvalSymlinks(path)
		if err != nil {
			r = path
		}
		resolved[path] = r
		return r
	}
	source := map[string]string{}
	for f := range c.moduleFiles {
		source[resolve(f)] = f
	}
	// A body whose escapes are reported where it is inlined has them found
	// at its calls, while its compiled code is at its own lines.
	type lines struct {
		file       string
		start, end int
	}
	var inlinedBodies []lines
	for fn := range c.inlinedBodies {
		start, end := c.fset.Position(fn.Syntax().Pos()), c.fset.Position(fn.Syntax().End())
		inlinedBodies = append(inlinedBodies, lines{start.Filename, start.Line, end.Line})
	}
	byStart := map[asmPos]string{}
	for at, fns := range nested {
		if len(fns) == 1 {
			byStart[asmPos{resolve(at.file), at.line}] = symbolOf(fns[0])
		}
	}

	x := &CrossCheck{Unsorted: map[string]int{}, flagged: map[string]bool{}, reused: map[string]bool{}}
	unmatched := map[string]bool{}
	seen := map[RuntimeCall]bool{}
	for _, rc := range calls {
		if !ours(rc.symbol) {
			continue
		}
		sym := compiledSymbol(rc.symbol)
		if sym == "" {
			continue
		}
		// The pointer method the compiler makes for a value method holds its body.
		if v := pointerWrapper.ReplaceAllString(sym, "$1$2."); !known[sym] && known[v] {
			sym = v
		}
		if at, ok := starts[rc.symbol]; !known[sym] && ok {
			if s, ok := byStart[asmPos{resolve(at.file), at.line}]; ok {
				sym = s
			}
		}
		if !known[sym] {
			// A method promoted from an embedded type of another package holds
			// that package's code.
			if _, inModule := source[resolve(rc.file)]; allocatingRuntime.MatchString(rc.fn) && inModule {
				unmatched[rc.symbol] = true
			}
			continue
		}
		if !reached[sym] {
			continue
		}
		switch {
		case nonAllocatingRuntime.MatchString(rc.fn):
			continue
		case !allocatingRuntime.MatchString(rc.fn):
			x.Unsorted[rc.fn]++
			continue
		}
		file, ok := source[resolve(rc.file)]
		if !ok {
			x.Elsewhere++
			continue
		}
		state := c.lineState[file][rc.line]
		switch {
		case state&lineChecked == 0:
			continue
		case state&lineUnchecked != 0:
			x.Mixed++
			continue
		case found[c.rel(file)][rc.line] != nil && explains(found[c.rel(file)][rc.line].Reasons, rc.fn):
			x.flagged[fmt.Sprintf("%s:%d", c.rel(file), rc.line)] = true
			continue
		case state&lineReused != 0 && (rc.fn == "growslice" || strings.HasPrefix(rc.fn, "mapassign")):
			x.reused[fmt.Sprintf("%s:%d", c.rel(file), rc.line)] = true
			continue
		case slices.ContainsFunc(inlinedBodies, func(b lines) bool { return b.file == file && b.start <= rc.line && rc.line <= b.end }):
			x.flagged[fmt.Sprintf("%s:%d", c.rel(file), rc.line)] = true
			continue
		}
		r := RuntimeCall{File: c.rel(file), Line: rc.line, Source: c.lineTokens(file, rc.line), Func: rc.symbol, Runtime: rc.fn}
		if seen[r] {
			continue
		}
		seen[r] = true
		if bufferedRuntime.MatchString(rc.fn) {
			x.Buffered = append(x.Buffered, r)
		} else {
			x.Calls = append(x.Calls, r)
		}
	}
	x.Unmatched = slices.Sorted(func(yield func(string) bool) {
		for s := range unmatched {
			if !yield(s) {
				return
			}
		}
	})
	slices.SortFunc(x.Calls, func(a, b RuntimeCall) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		return a.Line - b.Line
	})
	x.Flagged, x.Reused = len(x.flagged), len(x.reused)
	return x
}

// explainedBy are the words of a finding's reason that account for a call to
// each kind of allocating runtime function. An escape accounts for any.
var explainedBy = []struct {
	runtime *regexp.Regexp
	reasons []string
}{
	{regexp.MustCompile(`^(mapassign.*|makemap.*)$`), []string{"map write", "make with a size"}},
	{regexp.MustCompile(`^(concatstring.*|concatbyte.*|slicebytetostring|stringtoslicebyte|stringtoslicerune|slicerunetostring|intstring|rawstring.*)$`),
		[]string{"string concatenation", "conversion"}},
	{regexp.MustCompile(`^(growslice|makeslice.*)$`), []string{"append", "make with a size"}},
	{regexp.MustCompile(`^(makechan.*)$`), []string{"make chan"}},
	{regexp.MustCompile(`^(newproc)$`), []string{"go statement"}},
	{regexp.MustCompile(`^(deferproc)$`), []string{"defer in a loop"}},
	// A defer in a loop and a go statement's closure are records the runtime
	// or the compiled code allocates directly.
	{regexp.MustCompile(`^(newobject|mallocgc.*|convT.*)$`), []string{"defer in a loop", "go statement"}},
}

// explains reports whether one of reasons accounts for a call to the runtime
// function fn.
func explains(reasons []string, fn string) bool {
	for _, r := range reasons {
		if strings.HasPrefix(r, "escapes: ") {
			return true
		}
	}
	for _, e := range explainedBy {
		if e.runtime.MatchString(fn) {
			return slices.ContainsFunc(reasons, func(r string) bool {
				return slices.ContainsFunc(e.reasons, func(w string) bool { return strings.Contains(r, w) })
			})
		}
	}
	return false
}

// symbolOf names fn as compiledSymbol names the compiler's symbol for it.
func symbolOf(fn *ssa.Function) string {
	if o := fn.Origin(); o != nil {
		fn = o
	}
	top := fn
	for top.Parent() != nil {
		top = top.Parent()
	}
	if top.Pkg == nil {
		return ""
	}
	pkg := top.Pkg.Pkg
	prefix := pkg.Path()
	if pkg.Name() == "main" {
		prefix = "main"
	}
	name := fn.Name()
	if recv := top.Signature.Recv(); recv != nil {
		t, ptr := types.Unalias(recv.Type()), false
		if p, ok := t.(*types.Pointer); ok {
			t, ptr = types.Unalias(p.Elem()), true
		}
		if named, ok := t.(*types.Named); ok {
			if ptr {
				name = "(*" + named.Obj().Name() + ")." + name
			} else {
				name = named.Obj().Name() + "." + name
			}
		}
	}
	return prefix + "." + stripTypeArgs(name)
}

var (
	closureSuffix  = regexp.MustCompile(`\.func(\d+)((?:\.\d+)*)`)
	wrapperSuffix  = regexp.MustCompile(`\.(deferwrap|gowrap)\d+$`)
	inlinedCopy    = regexp.MustCompile(`#[^#]*#`)
	packageInit    = regexp.MustCompile(`\.init(\.\d+)?(\.|$)`)
	pointerWrapper = regexp.MustCompile(`^(.*\.)\(\*([^)]+)\)\.`)
)

// compiledSymbol names a compiler symbol as symbolOf names its function: no
// type arguments, closures numbered as SSA numbers them, a closure copied
// where its function was inlined as the closure, and the bodies of defer and
// go statements as the function holding them. A method value wrapper, which
// no source holds, and a package's initialization, which no root reaches,
// are "".
func compiledSymbol(sym string) string {
	sym = inlinedCopy.ReplaceAllString(sym, "")
	if strings.HasSuffix(sym, "-fm") || packageInit.MatchString(sym) {
		return ""
	}
	sym = stripTypeArgs(sym)
	for wrapperSuffix.MatchString(sym) {
		sym = wrapperSuffix.ReplaceAllString(sym, "")
	}
	return closureSuffix.ReplaceAllStringFunc(sym, func(m string) string {
		parts := strings.Split(strings.TrimPrefix(m, ".func"), ".")
		return "$" + strings.Join(parts, "$")
	})
}

// stripTypeArgs removes every bracketed type argument list from name.
func stripTypeArgs(name string) string {
	var b strings.Builder
	depth := 0
	for _, r := range name {
		switch {
		case r == '[':
			depth++
		case r == ']' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

var asmCall = regexp.MustCompile(`\((/.+\.go):(\d+)\)\tCALL\truntime\.([A-Za-z0-9_]+)\(SB\)`)

// asmReader collects the runtime calls in the compiler's -S output, each
// under the function the last STEXT header named.
type asmReader struct {
	symbol string
	skip   bool
	seen   map[string]bool
	calls  []runtimeCall
	// starts holds the file and line each function's first instruction is at.
	starts map[string]asmPos
}

type asmPos struct {
	file string
	line int
}

var asmPosition = regexp.MustCompile(`^\t0x[0-9a-f]+ \d+ \((/.+\.go):(\d+)\)\t`)

func (a *asmReader) line(l string) {
	if l == "" {
		return
	}
	if l[0] != '\t' && l[0] != ' ' {
		if i := strings.Index(l, " STEXT"); i > 0 {
			a.symbol = l[:i]
			// The same function compiled for another pattern prints again.
			a.skip = a.seen[a.symbol]
			a.seen[a.symbol] = true
		}
		return
	}
	if a.skip {
		return
	}
	if _, ok := a.starts[a.symbol]; !ok {
		if m := asmPosition.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			a.starts[a.symbol] = asmPos{m[1], n}
		}
	}
	if !strings.Contains(l, "\tCALL\truntime.") {
		return
	}
	m := asmCall.FindStringSubmatch(l)
	if m == nil {
		return
	}
	n, _ := strconv.Atoi(m[2])
	a.calls = append(a.calls, runtimeCall{symbol: a.symbol, file: m[1], line: n, fn: m[3]})
}
