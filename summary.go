package noalloc

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// A summary is what a call to a function outside the module allocates,
// worked out from its code: from source for a function written in Go, and
// from its compiled code for one written in assembly or in the runtime.
// Each is indexed by the failures the call gives the function, as
// calleeFailures has them: Alloc when it can allocate on a path those
// failures leave checked, and Appends when it allocates nothing once its
// first argument, a slice it returns, is reused memory.
type summary struct {
	Alloc   [8]bool
	Appends [8]bool
	Shaped  bool `json:",omitempty"`
	// Returns is whether, shaped like append, every return of the function
	// is its first argument appended to.
	Returns bool `json:",omitempty"`
	// Why is, where Alloc is set, the chain of calls from the function to
	// what allocates, for whoever decides where an override belongs.
	Why [8]string `json:",omitempty"`
	// Bare is Alloc but for the function's own escapes, which the compiler
	// reports again, as it decides them there, at a call it inlines.
	Bare [8]bool
	// Assumed are the allowlist lines that kept the summary from
	// allocating: each covers a function the summary reached that would
	// allocate without it.
	Assumed []string `json:",omitempty"`
	// NoImpl is, for a method call, that no type outside the module
	// implements the method.
	NoImpl bool `json:",omitempty"`
}

// summaryFormat names how summaries are worked out, so that a cache written
// another way is not read.
const summaryFormat = "5"

// testHookSummarize sees the functions each Check summarizes from source,
// past what its cache held. Only tests set it.
var testHookSummarize func(names []string)

// loadSummaries gives c a summary of every function outside the module that
// a reached call calls, from the cache when it has one and otherwise worked
// out from the module's dependencies and saved there.
func (c *checker) loadSummaries(env []string) error {
	need := map[string]bool{}
	// The module's functions with no Go body are judged by their machine
	// code, which changes with the module and is not cached.
	var local []*ssa.Function
	for site, callees := range c.sites {
		if !c.ours(site.Parent()) {
			continue
		}
		for _, f := range callees {
			switch {
			case c.outside(f):
				need[summaryName(f)] = true
			case f.Blocks == nil && !slices.Contains(local, f):
				local = append(local, f)
			}
		}
	}
	// A method call VTA resolves to nothing is also judged by every
	// implementation outside the module.
	for _, fn := range append(slices.Clone(c.order), c.coverOrder...) {
		if !c.ours(fn) {
			continue
		}
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				if call, ok := in.(ssa.CallInstruction); ok && c.open[call] {
					need[methodSummaryName(call.Common())] = true
				}
			}
		}
	}
	pkgs, err := buildPackages(c.cfg, env)
	if err != nil {
		return err
	}
	cache, err := openSummaryCache(c.cfg, buildKey(c.cfg, pkgs, c.allow.assumed))
	if err != nil {
		return err
	}
	c.sums = map[string]summary{}
	var missing []string
	for n := range need {
		if s, ok := cache.get(n); ok {
			c.sums[n] = s
		} else {
			missing = append(missing, n)
		}
	}
	slices.Sort(missing)
	if testHookSummarize != nil {
		testHookSummarize(missing)
	}
	archives := map[string]string{}
	for _, p := range pkgs {
		archives[p.path] = p.archive
	}
	if len(missing) > 0 {
		got, err := summarize(c.cfg, env, missing, archives, c.allow.assumed)
		if err != nil {
			return err
		}
		for n, s := range got {
			c.sums[n] = s
			cache.put(n, s)
		}
		if err := cache.save(); err != nil {
			return err
		}
	}
	c.reliedOn = map[string]bool{}
	for n := range need {
		for _, l := range c.sums[n].Assumed {
			c.reliedOn[l] = true
		}
	}
	c.machineSummaries(env, local, archives)
	return nil
}

// machineSummaries gives c a summary of each of fns, the module's functions
// with no Go body, from its machine code: its own, or that of the symbol a
// //go:linkname binds it to.
func (c *checker) machineSummaries(env []string, fns []*ssa.Function, archives map[string]string) {
	if len(fns) == 0 {
		return
	}
	mc := newMachineCode(env, archives)
	mc.assumed = func(sym string) bool {
		return slices.ContainsFunc(symbolFuncs(sym), c.overridden)
	}
	for _, f := range fns {
		sym := symbolOf(f)
		if to, ok := c.linknames[f.Pkg.Pkg.Path()+"."+f.Name()]; ok {
			sym = to
		}
		var sum summary
		if mc.allocates(sym) {
			why := strings.Join(append([]string{"machine code of " + sym}, mc.chain(sym)...), " > ")
			for _, fails := range failureContexts {
				sum.Alloc[fails], sum.Why[fails] = true, why
				sum.Bare[fails] = true
			}
		}
		c.sums[summaryName(f)] = sum
	}
}

// summarizer works out summaries over a program built with every package
// from source. Its checker's module is everything but the caller's module
// and the runtime, whose code is read compiled.
type summarizer struct {
	c       *checker
	mc      *machineCode
	escapes map[string][]escape
	memo    memo[sumKey]
	appends memo[*ssa.Function]
	edges   map[*ssa.Function]map[ssa.CallInstruction][]*ssa.Function
	// assumed are the allowlist's labeled functions. relied holds, for
	// each summary, symbol, and function summarized, the lines that kept
	// it from allocating, and open those being worked out, innermost last.
	assumed map[string]string
	relied  map[any]map[string]bool
	open    []any
	// inlined holds the calls the compiler inlined, and inlinedEsc those it
	// reports an inlined body's escape at, other than a constant's, by
	// position.
	inlined, inlinedEsc map[token.Pos]bool
	// linknames maps a function declared with no body, as its package and
	// name, to the symbol a //go:linkname directive binds it to.
	linknames map[string]string
	// whys holds, for each summary that allocates, the first step found
	// toward what allocates.
	whys map[sumKey]why
	// noted holds where the compiler reported anything, and calls the
	// callee of each call, by its position, for escapes at inlined calls.
	noted map[string][]token.Pos
	calls map[token.Pos]*ssa.Function
}

// A why is one step toward an allocation, and what to follow from it: a
// summary, or the machine code of a symbol.
type why struct {
	step    string
	next    *sumKey
	nextSym string
}

// because notes why the summary k allocates, and reports that it does.
func (s *summarizer) because(k sumKey, w why) bool {
	if _, ok := s.whys[k]; !ok {
		s.whys[k] = w
	}
	return true
}

// chain is why the summary k allocates, from k to the allocation.
func (s *summarizer) chain(k sumKey) string {
	var steps []string
	for range 24 {
		w, ok := s.whys[k]
		if !ok {
			break
		}
		steps = append(steps, w.step)
		if w.nextSym != "" {
			steps = append(steps, s.mc.chain(w.nextSym)...)
			break
		}
		if w.next == nil {
			break
		}
		k = *w.next
	}
	return strings.Join(steps, " > ")
}

// enter notes that k, a summary, a symbol, or a function summarized, is
// being worked out.
func (s *summarizer) enter(k any) {
	s.open = append(s.open, k)
}

// leave notes that k is worked out, and that what is being worked out
// around it relies on what it relied on.
func (s *summarizer) leave(k any) {
	s.open = s.open[:len(s.open)-1]
	for l := range s.relied[k] {
		s.rely(l)
	}
}

// rely notes that what is being worked out relies on the allowlist line l.
// A line noted for one open entry is noted for every entry opened before it,
// so the first that has it ends the walk.
func (s *summarizer) rely(l string) {
	for _, k := range slices.Backward(s.open) {
		if s.relied[k][l] {
			return
		}
		if s.relied[k] == nil {
			s.relied[k] = map[string]bool{}
		}
		s.relied[k][l] = true
	}
}

// assume reports whether an allowlist line covers name, a function that
// would allocate, and notes that what is being worked out relies on each
// line that does.
func (s *summarizer) assume(name string) bool {
	labels := allowLabels(s.assumed, name)
	for _, l := range labels {
		s.rely(l)
	}
	return len(labels) > 0
}

// symbolFuncs names a function as funcName can, from its symbol:
// "runtime.(*m).f" is "(*runtime.m).f", and "runtime.t.f" is either a
// method of t, "(runtime.t).f", or a function t's closure f.
func symbolFuncs(sym string) []string {
	pkg := symbolPackage(sym)
	rest := strings.TrimPrefix(sym, pkg+".")
	if r, ok := strings.CutPrefix(rest, "(*"); ok {
		return []string{"(*" + pkg + "." + r}
	}
	out := []string{sym}
	if t, m, ok := strings.Cut(rest, "."); ok {
		out = append(out, "("+pkg+"."+t+")."+m)
	}
	return out
}

type sumKey struct {
	fn    *ssa.Function
	fails failures
	app   bool
	// consts are the bool parameters a call passes a constant to, as
	// constArgs writes them, which decide the branches they are tested in.
	consts string
	// bare is whether the call is inlined, so the function's own escapes
	// are left to its caller.
	bare bool
}

// failureContexts are every value of failures, as calleeFailures gives
// them and a caller can narrow them.
var failureContexts = []failures{0, 1, 2, 3, 4, 5, 6, 7}

// summarize works out the summaries of names, each as funcName has it.
func summarize(cfg Config, env []string, names []string, archives map[string]string, assumed map[string]string) (map[string]summary, error) {
	graph, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps,
		Dir: cfg.Dir, Env: env, Overlay: cfg.Overlay}, cfg.Patterns...)
	if err != nil {
		return nil, err
	}
	var all []string
	packages.Visit(graph, nil, func(p *packages.Package) {
		all = append(all, p.PkgPath)
	})
	type compiled struct {
		escapes, notes, inlines []escape
		err                     error
	}
	done := make(chan compiled, 1)
	go func() {
		r, err := compilerEscapes(cfg, env, []string{"-gcflags=all=-m"})
		done <- compiled{r.escapes, r.notes, r.inlines, err}
	}()
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
		packages.NeedImports | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		Dir: cfg.Dir, Env: env, Overlay: cfg.Overlay}, all...)
	if err != nil {
		return nil, err
	}
	prog := ssa.NewProgram(pkgs[0].Fset, ssa.InstantiateGenerics)
	for _, p := range pkgs {
		if p.Types != nil {
			prog.CreatePackage(p.Types, p.Syntax, p.TypesInfo, true)
		}
	}
	prog.Build()
	mc := newMachineCode(env, archives)
	comp := <-done
	if comp.err != nil {
		return nil, comp.err
	}

	c := &checker{cfg: cfg, prog: prog, fset: prog.Fset, exempt: map[string]map[int]bool{}, lines: map[string][]string{},
		appends: map[*ssa.Function]bool{}, fixed: map[types.Type]bool{}, marks: map[*ast.Comment]bool{},
		lineState: map[string]map[int]uint8{}, moduleFiles: map[string]bool{}, inlinedBodies: map[*ssa.Function]bool{},
		impls: map[string][]*ssa.Function{}, open: map[ssa.CallInstruction]bool{}, files: map[string]*ast.File{}, fileInfo: map[string]*types.Info{}}
	c.world = func(path string) bool { return !cfg.inModule(path) && !runtimePackage(path) }
	for _, p := range pkgs {
		if p.Types != nil {
			c.modulePkgs = append(c.modulePkgs, p.Types)
		}
		for _, f := range p.Syntax {
			name := filepath.Clean(prog.Fset.Position(f.Pos()).Filename)
			c.files[name], c.fileInfo[name] = f, p.TypesInfo
		}
	}
	c.graph()
	c.reach(nil)

	s := &summarizer{c: c, mc: mc, escapes: map[string][]escape{}, edges: map[*ssa.Function]map[ssa.CallInstruction][]*ssa.Function{},
		assumed: assumed, relied: map[any]map[string]bool{}, linknames: linknames(pkgs), whys: map[sumKey]why{}}
	// A call returns the memory it is handed, appended to, when every
	// return of the function it calls, shaped like append, is that, or when
	// that function, handed reused memory, allocates nothing but to grow it.
	c.appended = func(g *ssa.Function) bool {
		return s.returnsAppended(g) || appendSignature(g.Signature) && !s.allocates(g, failOnce, true)
	}
	mc.assumed = func(sym string) bool {
		return slices.ContainsFunc(symbolFuncs(sym), s.assume)
	}
	mc.enter = func(sym string) { s.enter("symbol: " + sym) }
	mc.leave = func(sym string) { s.leave("symbol: " + sym) }
	for _, e := range comp.escapes {
		s.escapes[e.file] = append(s.escapes[e.file], e)
	}
	// Where the compiler reported anything of its escape analysis, to tell
	// an inlined closure compiled on its own from one only ever inlined.
	s.noted = map[string][]token.Pos{}
	for _, e := range comp.notes {
		if pos, ok := c.posOf(e); ok {
			s.noted[e.file] = append(s.noted[e.file], pos)
		}
	}
	s.inlined = c.positions(comp.inlines)
	s.inlinedEsc = c.positions(inlinedEscapes(comp.escapes))
	byName := map[string]*ssa.Function{}
	for fn := range ssautil.AllFunctions(prog) {
		byName[summaryName(fn)] = fn
	}
	out := map[string]summary{}
	for _, n := range names {
		s.enter(n)
		sum := s.summary(pkgs, byName, n)
		s.leave(n)
		sum.Assumed = slices.Sorted(maps.Keys(s.relied[n]))
		out[n] = sum
	}
	return out, nil
}

// summary works out the summary of n, a function as funcName has it or a
// method call as methodSummaryName has it.
func (s *summarizer) summary(pkgs []*packages.Package, byName map[string]*ssa.Function, n string) summary {
	{
		if iface, method, ok := strings.Cut(strings.TrimPrefix(n, "method: "), " "); ok && strings.HasPrefix(n, "method: ") {
			return s.methodSummary(pkgs, iface, method)
		}
		fn := byName[n]
		if fn == nil {
			// An instance no package member is, such as one with the
			// caller's own type parameters as its arguments, is judged by
			// its generic function's body, which holds for every argument.
			if base, ok := genericBase(n); ok {
				fn = byName[base]
			}
		}
		if fn == nil {
			// Code that cannot be found allocates, and is not looked for
			// again.
			var unknown summary
			for _, f := range failureContexts {
				unknown.Alloc[f], unknown.Bare[f] = true, true
				unknown.Why[f] = n + " has no code to summarize"
			}
			return unknown
		}
		var sum summary
		sum.Shaped = appendSignature(fn.Signature)
		sum.Returns = sum.Shaped && s.returnsAppended(fn)
		for _, f := range failureContexts {
			sum.Alloc[f] = s.allocates(fn, f, false)
			sum.Appends[f] = sum.Shaped && !s.allocates(fn, f, true)
			if sum.Alloc[f] {
				sum.Why[f] = s.chain(sumKey{fn, f, false, "", false})
			}
			sum.Bare[f] = sum.Alloc[f] && s.allocatesWith(fn, f, false, "", true)
		}
		return sum
	}
}

// genericBase is the name of the generic function an instance's name, as
// summaryName has it, instantiates: the name without its type arguments.
func genericBase(name string) (string, bool) {
	if !strings.HasSuffix(name, "]") {
		return "", false
	}
	depth := 0
	for i := len(name) - 1; i >= 0; i-- {
		switch name[i] {
		case ']':
			depth++
		case '[':
			if depth--; depth == 0 {
				return name[:i], true
			}
		}
	}
	return "", false
}

// summaryName names fn for its summary: a generic function's instance by
// its type arguments, since each instance is code of its own.
func summaryName(fn *ssa.Function) string {
	if fn.Origin() != nil {
		return fn.RelString(nil)
	}
	return funcName(fn)
}

// linknames maps each function pkgs declare with no body, as its package and
// name, to the symbol a //go:linkname directive in its package binds it to:
// the code it runs is under that name.
func linknames(pkgs []*packages.Package) map[string]string {
	out := map[string]string{}
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			for _, g := range f.Comments {
				for _, cm := range g.List {
					fields := strings.Fields(cm.Text)
					if len(fields) == 3 && fields[0] == "//go:linkname" {
						out[p.PkgPath+"."+fields[1]] = fields[2]
					}
				}
			}
		}
	}
	return out
}

// methodSummaryName names the summary of a method call, as of every
// implementation outside the module: "method: " and the interface, as its
// type string, and the method, as in "method: context.Context Err". A call on
// a type parameter is named by the parameter's constraint.
func methodSummaryName(cm *ssa.CallCommon) string {
	t := cm.Value.Type()
	if tp, ok := t.(*types.TypeParam); ok {
		t = tp.Constraint()
	}
	return "method: " + t.String() + " " + cm.Method.Name()
}

// methodSummary summarizes a call of method of the named interface iface,
// written as types writes it, as of every type outside the module of every
// package loaded that implements it: it allocates where one of their methods
// does.
func (s *summarizer) methodSummary(pkgs []*packages.Package, iface, method string) summary {
	var out summary
	unknown := func(why string) summary {
		for _, f := range failureContexts {
			out.Alloc[f], out.Why[f] = true, why
			out.Bare[f] = true
		}
		return out
	}
	path, name := iface, ""
	if i := strings.LastIndex(iface, "."); i > 0 {
		path, name = iface[:i], iface[i+1:]
	}
	var it *types.Interface
	for _, p := range pkgs {
		if p.Types != nil && p.Types.Path() == path {
			if tn, ok := p.Types.Scope().Lookup(name).(*types.TypeName); ok {
				it, _ = tn.Type().Underlying().(*types.Interface)
			}
		}
	}
	if it == nil {
		return unknown(iface + " is no named interface to find implementations of")
	}
	var impls []*ssa.Function
	for _, p := range pkgs {
		if p.Types == nil || s.c.cfg.inModule(p.PkgPath) {
			continue
		}
		scope := p.Types.Scope()
		for _, n := range scope.Names() {
			tn, ok := scope.Lookup(n).(*types.TypeName)
			if !ok || tn.IsAlias() || types.IsInterface(tn.Type()) {
				continue
			}
			if named, ok := tn.Type().(*types.Named); ok && named.TypeParams().Len() > 0 {
				continue
			}
			for _, t := range []types.Type{tn.Type(), types.NewPointer(tn.Type())} {
				if !types.Implements(t, it) {
					continue
				}
				m, _, _ := types.LookupFieldOrMethod(t, true, nil, method)
				if fn, ok := m.(*types.Func); ok {
					if f := s.c.prog.LookupMethod(t, fn.Pkg(), method); f != nil {
						impls = append(impls, f)
					}
				}
				break
			}
		}
	}
	out.NoImpl = len(impls) == 0
	for _, f := range failureContexts {
		for _, fn := range impls {
			if s.allocates(fn, f, false) {
				out.Alloc[f] = true
				out.Why[f] = s.chain(sumKey{fn, f, false, "", false})
				break
			}
		}
	}
	return out
}

// runtimePackage reports whether path is the runtime's, whose code a
// summary reads compiled: its allocator is what allocating means.
func runtimePackage(path string) bool {
	return path == "runtime" || strings.HasPrefix(path, "runtime/") || strings.HasPrefix(path, "internal/runtime/")
}

// appendSignature reports whether sig takes a slice first and returns the
// same type first.
func appendSignature(sig *types.Signature) bool {
	if sig.Params().Len() == 0 || sig.Results().Len() == 0 {
		return false
	}
	first := sig.Params().At(0).Type()
	_, slice := first.Underlying().(*types.Slice)
	return slice && types.Identical(first, sig.Results().At(0).Type())
}

// returnsAppended reports whether fn, shaped like append, returns its first
// argument appended to on every return, through calls to functions that do
// as well. A function calling itself again is taken, there, to do so.
func (s *summarizer) returnsAppended(fn *ssa.Function) bool {
	if !appendSignature(fn.Signature) {
		return false
	}
	if fn.Blocks == nil && fn.Origin() != nil && fn.Origin().Blocks != nil {
		fn = fn.Origin()
	}
	if fn.Blocks == nil {
		return false
	}
	return s.appends.get(fn, true, func() bool {
		for _, b := range fn.Blocks {
			if r, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && !s.c.fromFirstParam(fn, r.Results[0], map[ssa.Value]bool{}) {
				return false
			}
		}
		return true
	})
}

// allocates reports whether fn can allocate when its failures are fails: on
// a block those failures leave checked, or in a call from one. With app, its
// first argument is reused memory, so appending to it allocates nothing.
func (s *summarizer) allocates(fn *ssa.Function, fails failures, app bool) bool {
	return s.allocatesWith(fn, fails, app, "", false)
}

// allocatesWith is allocates with the bool parameters consts names holding
// the values it gives them, and, with bare, of a call the compiler inlines.
func (s *summarizer) allocatesWith(fn *ssa.Function, fails failures, app bool, consts string, bare bool) bool {
	c := s.c
	top := topLevel(fn)
	if top.Pkg != nil && c.cfg.inModule(top.Pkg.Pkg.Path()) {
		// The caller's module is checked where it runs.
		return false
	}
	if len(allowLabels(s.assumed, funcName(fn))) > 0 {
		// A labeled function is worked out as any other, and the line
		// relied on only when it would allocate.
		return s.allocatesFrom(fn, fails, app, consts, bare) && !s.assume(funcName(fn))
	}
	return s.allocatesFrom(fn, fails, app, consts, bare)
}

// allocatesFrom is allocatesWith of fn, a function outside the module, by
// its code.
func (s *summarizer) allocatesFrom(fn *ssa.Function, fails failures, app bool, consts string, bare bool) bool {
	top := topLevel(fn)
	if fn.Blocks == nil && fn.Origin() != nil && fn.Origin().Blocks != nil {
		// An instance not built for its own type arguments, as one in a
		// generic body is, runs its generic function's body.
		fn = fn.Origin()
	}
	readable := fn.Blocks != nil && (wrapper(fn) || top.Pkg != nil && !runtimePackage(top.Pkg.Pkg.Path()))
	if !readable {
		sym := symbolOf(fn)
		if fn.Blocks == nil && top.Pkg != nil {
			if to, ok := s.linknames[top.Pkg.Pkg.Path()+"."+fn.Name()]; ok {
				sym = to
			}
		}
		k := sumKey{fn, fails, app, "", false}
		if sym == "" {
			return s.because(k, why{step: funcName(fn) + " has no code to read"})
		}
		if s.mc.allocates(sym) {
			return s.because(k, why{step: "machine code of " + sym, nextSym: sym})
		}
		return false
	}
	app = app && appendSignature(fn.Signature)
	k := sumKey{fn, fails, app, consts, bare && fn.Blocks != nil}
	s.enter(k)
	defer s.leave(k)
	return s.memo.get(k, false, func() bool { return s.compute(k) })
}

func topLevel(fn *ssa.Function) *ssa.Function {
	if o := fn.Origin(); o != nil {
		fn = o
	}
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	return fn
}

func (s *summarizer) compute(k sumKey) bool {
	fn, fails, app := k.fn, k.fails, k.app
	c := s.c
	at := func(in ssa.Instruction) string {
		p := c.fset.Position(in.Pos())
		return fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line)
	}
	was, had := c.failing[fn]
	stops := c.stops[fn]
	c.failing[fn], c.stops[fn] = fails, nil
	origin := fn
	if o := fn.Origin(); o != nil {
		origin = o
	}
	marked := app && !c.appends[origin]
	if marked {
		c.appends[origin] = true
		c.fromScr = memo[ssa.Value]{}
	}
	defer func() {
		if had {
			c.failing[fn] = was
		} else {
			delete(c.failing, fn)
		}
		c.stops[fn] = stops
		if marked {
			delete(c.appends, origin)
			c.fromScr = memo[ssa.Value]{}
		}
	}()
	ok := c.allowedBlocks(fn)
	known := paramConsts(fn, k.consts)
	if len(known) > 0 {
		ok = reachableWith(fn, ok, known)
	}
	edges := s.callees(fn)
	for _, b := range fn.Blocks {
		if !ok[b] {
			continue
		}
		for _, in := range b.Instrs {
			call, isCall := in.(ssa.CallInstruction)
			if !isCall {
				if r := c.instrAllocs(in); len(r) > 0 {
					return s.because(k, why{step: funcName(fn) + ": " + r[0] + " at " + at(in)})
				}
				continue
			}
			if _, isBuiltin := call.Common().Value.(*ssa.Builtin); isBuiltin {
				if r := c.callAllocs(call); len(r) > 0 {
					return s.because(k, why{step: funcName(fn) + ": " + r[0] + " at " + at(in)})
				}
				continue
			}
			if _, isGo := call.(*ssa.Go); isGo {
				return s.because(k, why{step: funcName(fn) + ": go statement at " + at(in)})
			}
			if d, isDefer := call.(*ssa.Defer); isDefer && inLoop(d.Block()) {
				return s.because(k, why{step: funcName(fn) + ": defer in a loop at " + at(in)})
			}
			gs := edges[call]
			if len(gs) == 0 {
				return s.because(k, why{step: funcName(fn) + ": call with no known callee at " + at(in)})
			}
			for _, g := range gs {
				if onceSetup(g) {
					continue
				}
				y := c.calleeFailures(fn, call, g, ok)
				if cl, isCall := call.(*ssa.Call); isCall && c.newError(cl) && !c.failureArm(cl.Block()) {
					// An error made on a normal path is no failure's.
					y &^= failErrors
				}
				consts := constArgs(call, g, known)
				// Inlined, g's own escapes are what the compiler reports at
				// the call: g allocates only as it does without them, or
				// when the compiler reports one.
				pos := call.Common().Pos()
				bare := s.inlined[pos] && !s.inlinedEsc[pos]
				if dst := appendedTo(call.Common(), g); dst != nil && appendSignature(g.Signature) && c.reused(dst) {
					// g appends to reused memory: it allocates only if it does
					// more than grow that.
					if !s.allocatesWith(g, y, true, consts, false) || bare && !s.allocatesWith(g, y, true, consts, true) {
						continue
					}
					next := sumKey{g, y, true, consts, false}
					return s.because(k, why{step: funcName(fn) + " calls " + funcName(g) + " at " + at(in), next: &next})
				}
				if s.allocatesWith(g, y, false, consts, false) && (!bare || s.allocatesWith(g, y, false, consts, true)) {
					next := sumKey{g, y, false, consts, false}
					return s.because(k, why{step: funcName(fn) + " calls " + funcName(g) + " at " + at(in), next: &next})
				}
			}
		}
	}
	if k.bare {
		return false
	}
	if e, found := s.escapesIn(fn, ok); found {
		return s.because(k, why{step: fmt.Sprintf("%s: escapes: %s at %s:%d", funcName(fn), e.msg, filepath.Base(e.file), e.line)})
	}
	return false
}

// constArgs writes which of g's bool parameters call passes a constant to,
// directly or as one of the caller's parameters whose value known holds, as
// "index=value" pairs.
func constArgs(call ssa.CallInstruction, g *ssa.Function, known map[*ssa.Parameter]bool) string {
	cm := call.Common()
	if cm.IsInvoke() || len(cm.Args) != len(g.Params) {
		return ""
	}
	var out []string
	for i, a := range cm.Args {
		if b, isBasic := g.Params[i].Type().Underlying().(*types.Basic); !isBasic || b.Kind() != types.Bool {
			continue
		}
		switch a := a.(type) {
		case *ssa.Const:
			if a.Value != nil && a.Value.Kind() == constant.Bool {
				out = append(out, fmt.Sprintf("%d=%v", i, constant.BoolVal(a.Value)))
			}
		case *ssa.Parameter:
			if v, ok := known[a]; ok {
				out = append(out, fmt.Sprintf("%d=%v", i, v))
			}
		}
	}
	return strings.Join(out, ",")
}

// paramConsts reads consts, as constArgs writes it, as values of fn's
// parameters.
func paramConsts(fn *ssa.Function, consts string) map[*ssa.Parameter]bool {
	if consts == "" {
		return nil
	}
	out := map[*ssa.Parameter]bool{}
	for _, kv := range strings.Split(consts, ",") {
		var i int
		var v bool
		if _, err := fmt.Sscanf(kv, "%d=%t", &i, &v); err == nil && i < len(fn.Params) {
			out[fn.Params[i]] = v
		}
	}
	return out
}

// reachableWith is ok less the blocks fn cannot reach with its parameters
// holding the values known gives them: past each branch those values decide,
// only the arm they take.
func reachableWith(fn *ssa.Function, ok map[*ssa.BasicBlock]bool, known map[*ssa.Parameter]bool) map[*ssa.BasicBlock]bool {
	f := &facts{is: map[ssa.Value]bool{}, flags: map[ssa.Value]bool{}, fields: map[valueField]bool{}}
	for p, v := range known {
		f.flags[p] = v
	}
	out := map[*ssa.BasicBlock]bool{}
	stack := []*ssa.BasicBlock{fn.Blocks[0]}
	for len(stack) > 0 {
		b := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[b] {
			continue
		}
		out[b] = true
		succs := b.Succs
		if br, isIf := b.Instrs[len(b.Instrs)-1].(*ssa.If); isIf {
			if v, decided := f.value(br.Cond); decided {
				if v {
					succs = succs[:1]
				} else {
					succs = succs[1:]
				}
			}
		}
		stack = append(stack, succs...)
	}
	for b := range out {
		if !ok[b] {
			delete(out, b)
		}
	}
	return out
}

// callees maps each call of fn to the functions it can call.
func (s *summarizer) callees(fn *ssa.Function) map[ssa.CallInstruction][]*ssa.Function {
	if m, ok := s.edges[fn]; ok {
		return m
	}
	m := map[ssa.CallInstruction][]*ssa.Function{}
	for _, e := range s.c.edges(fn) {
		if !slices.Contains(m[e.Site], e.Callee.Func) {
			m[e.Site] = append(m[e.Site], e.Callee.Func)
		}
	}
	s.edges[fn] = m
	return m
}

// onceSetup reports whether fn is how package sync runs a function once:
// what it runs is setup, as it is for a sync.Once the module calls.
func onceSetup(fn *ssa.Function) bool {
	top := topLevel(fn)
	if top.Pkg == nil || top.Pkg.Pkg.Path() != "sync" {
		return false
	}
	if strings.HasPrefix(top.Name(), "Once") {
		return true
	}
	if recv := top.Signature.Recv(); recv != nil {
		if p, isPtr := recv.Type().(*types.Pointer); isPtr {
			if n, isNamed := p.Elem().(*types.Named); isNamed && n.Obj().Name() == "Once" {
				return true
			}
		}
	}
	return false
}

// escapesIn reports whether the compiler moves to the heap something fn
// makes on a line one of its blocks in ok runs, or on a line none of its
// blocks runs.
func (s *summarizer) escapesIn(fn *ssa.Function, ok map[*ssa.BasicBlock]bool) (escape, bool) {
	c := s.c
	syn := fn.Syntax()
	if syn == nil {
		return escape{}, false
	}
	tf := c.fset.File(syn.Pos())
	if tf == nil {
		return escape{}, false
	}
	file := filepath.Clean(tf.Name())
	var inner [][2]token.Pos
	for _, a := range fn.AnonFuncs {
		if a.Syntax() != nil {
			inner = append(inner, [2]token.Pos{a.Syntax().Pos(), a.Syntax().End()})
		}
	}
	var lines map[int][]*ssa.BasicBlock
	for _, e := range s.escapes[file] {
		if e.line > tf.LineCount() {
			continue
		}
		pos := tf.LineStart(e.line) + token.Pos(e.col-1)
		// A closure's own allocation, reported where it starts, is the
		// function's that makes it.
		_, lit := syn.(*ast.FuncLit)
		if pos < syn.Pos() || pos >= syn.End() || lit && pos == syn.Pos() ||
			slices.ContainsFunc(inner, func(r [2]token.Pos) bool { return pos > r[0] && pos < r[1] }) {
			continue
		}
		if e.inlined {
			// An escape at an inlined call is this function's when it is
			// the call's boxed result; the callee's own summary has the rest.
			if s.calls == nil {
				s.calls = c.staticCallees()
			}
			msg, ok := c.inlinedEscape(s.calls[pos], e, s.noted)
			if !ok {
				continue
			}
			e.msg = msg
		} else if constantText(e.msg) && c.constantAt(file, pos) || c.boxedFreeAt(file, pos, e.msg) || c.handGrownAt(file, e.line, e.msg) {
			continue
		}
		if lines == nil {
			lines = map[int][]*ssa.BasicBlock{}
			for _, b := range fn.Blocks {
				for _, in := range b.Instrs {
					if p := in.Pos(); p.IsValid() {
						l := c.fset.Position(p).Line
						lines[l] = append(lines[l], b)
					}
				}
			}
		}
		bs := lines[e.line]
		if len(bs) == 0 || slices.ContainsFunc(bs, func(b *ssa.BasicBlock) bool { return ok[b] }) {
			return e, true
		}
	}
	return escape{}, false
}

// summaryCache holds summaries on disk, keyed by buildKey, so that a check
// of the same build after the first reads them rather than working them out
// again.
type summaryCache struct {
	path    string
	file    summaryFile
	changed bool
}

type summaryFile struct {
	Summaries map[string]summary
}

// testSummaryCache, when set, is where a check whose Config names no
// directory keeps its summaries. Only tests set it.
var testSummaryCache string

func openSummaryCache(cfg Config, key string) (*summaryCache, error) {
	dir := cfg.SummaryCache
	if dir == "" {
		dir = testSummaryCache
	}
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "noalloc")
	}
	c := &summaryCache{path: filepath.Join(dir, "summaries-"+key[:24]+".json")}
	if b, err := os.ReadFile(c.path); err == nil {
		json.Unmarshal(b, &c.file)
	}
	if c.file.Summaries == nil {
		c.file = summaryFile{Summaries: map[string]summary{}}
	}
	return c, nil
}

func (c *summaryCache) get(name string) (summary, bool) {
	s, ok := c.file.Summaries[name]
	return s, ok
}

func (c *summaryCache) put(name string, s summary) {
	c.file.Summaries[name] = s
	c.changed = true
}

func (c *summaryCache) save() error {
	if !c.changed {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(c.file)
	if err != nil {
		return err
	}
	// Each save writes a file of its own and renames it into place, so
	// checks saving at once leave one whole file.
	tmp, err := os.CreateTemp(filepath.Dir(c.path), filepath.Base(c.path)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
