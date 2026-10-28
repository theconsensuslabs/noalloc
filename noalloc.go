// Package noalloc checks that the code reachable from functions marked
// //noalloc:root makes no heap allocation on its normal path.
//
// It builds the program's call graph (CHA, refined by VTA) from the marked
// roots and checks every function of the module it reaches for heap escapes
// the compiler reports, for constructs that allocate at run time (growing
// appends, map writes, string concatenation and conversion, unsized make, go
// statements, defer in a loop), and for calls out of the module to functions
// not on the allowlist. Blocks that only lead to returning a non-nil error or
// to a panic, a branch that makes an error and goes on, and lazy first-use
// setup (`if x.f == nil { x.f = ... }`) are allowed. A line carrying `//noalloc:exempt <reason>`, or the line after one
// standing alone, is exempt, and so is everything it calls unless the
// reason's kind is one Config.LineOnly names.
//
// A function marked //noalloc:append is a root shaped like the builtin
// append: it appends to its first argument and returns it as its first
// result, allocating only to grow it. Its appends to that argument are
// allowed, and each call of it is judged as a call of append is.
//
// A type marked //noalloc:fixed holds memory its methods never grow, as its
// own tests show: the slices in its fields are reused memory wherever the
// pointer to it came from.
package noalloc

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// Config says what to check.
type Config struct {
	// Dir is the module directory Patterns load from, and Patterns the
	// packages whose call graph is built; roots may be in any package of
	// Module they reach.
	Dir      string
	Patterns []string
	// Module is the import path prefix of the packages checked. Calls into
	// any other package are calls out of the module.
	Module string
	// SourceModules are import path prefixes of other packages checked from
	// source as Module is, rather than vouched for by the allowlist: a call
	// into one is followed, and its exemptions are listed with Module's.
	SourceModules []string
	// Scratch names the per-request memory types, fully qualified
	// ("example.com/pkg.Scratch"): a request's own memory and the regions
	// lent to it. Slices and maps held in their fields, by value at any depth, are
	// reused memory: appending to them and writing to them are allowed.
	Scratch []string
	// Allow lists functions outside the module to take to allocate nothing,
	// each for a reason it names, one a line as Allowlist describes. What
	// every other function allocates is worked out from its code.
	Allow []string
	// Root is the directory findings are reported relative to.
	Root string
	// Env is the environment `go` runs in; nil is the process's.
	Env []string
	// Overlay replaces the contents of files, by absolute path, for both the
	// load and the compiler.
	Overlay map[string][]byte
	// LineOnly names the exemption kinds, the words before a reason's colon,
	// that exempt only their own line: what the line calls is still checked.
	// Every other exemption covers what its line calls too.
	LineOnly []string
	// SummaryCache is the directory the summaries of what calls out of the
	// module run are kept in between checks, a file for each build; empty is
	// noalloc's directory in the user's cache.
	SummaryCache string
	// Groups sorts the roots, named as SSA names them, into groups in order.
	// A finding belongs to the first group whose roots reach its function; a
	// root in no group is in a last group with no name.
	Groups []Group
}

// Group is a named set of roots.
type Group struct {
	Name  string
	Roots []string
}

// Allowlist is the allowlist checked in beside this file: the functions
// outside the module noalloc takes to allocate nothing although their code
// does not show it. Each line is "kind: name": a kind of debug, profiling,
// amortized, or measured, and a function as SSA names it
// ("(*sync.Mutex).Lock"), a pattern ending in "*" ("(*sync.Mutex).*"), or
// the call with no known callee a finding names. Text after # is a comment.
//
//go:embed allow.txt
var Allowlist string

// Finding is one line of reached code that allocates.
type Finding struct {
	File string
	Line int
	// Source is the line's Go tokens without comments, a space only between
	// two words: what names the line where its number would change with edits
	// above it.
	Source  string
	Func    string
	Group   string
	Reasons []string
	// Path is a shortest call path from a root to Func.
	Path []string
	// Why holds, for each reason that is a call out of the module whose
	// code allocates, the chain of calls from it to what allocates.
	Why map[string]string `json:",omitempty"`

	sites []site
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.Func, strings.Join(f.Reasons, "; "))
}

// Exemption is one //noalloc:exempt comment in the module.
type Exemption struct {
	File   string
	Line   int // the line it exempts
	Func   string
	Reason string
	// Covers is what the exemption lets through, each as function: reason,
	// once for each allocation: what its line allocates and, unless its kind
	// is LineOnly, what its line calls that no root reaches another way.
	Covers []string
	// Reached is whether a root reaches the exemption's line. One reached
	// that covers nothing exempts nothing.
	Reached bool
	// InSource is an exemption in one of SourceModules, which covers what
	// it covers for its own module's callers too, so what it covers here
	// alone is not grounds to delete it.
	InSource bool
}

// Key is the exemption as the checked-in list records it, without its line
// so that edits elsewhere in the file leave the list alone.
func (e Exemption) Key() string { return fmt.Sprintf("%s: %s: %s", e.File, e.Func, e.Reason) }

// Entry is the exemption as the checked-in list records it with what it
// covers: Key, then each covered allocation on a line of its own, indented.
func (e Exemption) Entry() string {
	var b strings.Builder
	b.WriteString(e.Key())
	for _, c := range e.Covers {
		b.WriteString("\n\t")
		b.WriteString(c)
	}
	return b.String()
}

// Result is what Check finds.
type Result struct {
	Roots      []string
	Reached    int // functions of the module checked
	Findings   []Finding
	Exemptions []Exemption
	// CrossCheck holds each call the module's compiled code makes to an
	// allocating runtime function to what Check found.
	CrossCheck *CrossCheck
	// Assumptions are the allowlist's lines, each with whether a check
	// relied on it.
	Assumptions []Assumption
}

// An Assumption is an allowlist line: a function or call the check takes to
// allocate nothing, for the reason its kind names (debug, profiling,
// amortized, or measured).
type Assumption struct {
	Kind, Func string
	// Used is whether the check relied on it: it covers a function the
	// check reached that would allocate without it.
	Used bool
}

// assumptionKinds are the reasons an allowlist line can give a function to
// be taken to allocate nothing, as "kind: function": it allocates only on a
// path that runs to debug or to profile, only to grow memory the runtime
// keeps and reuses, so a cost that falls away as a program runs, or, for
// what its code cannot show, as a test that calls it and counts its
// allocations does.
var assumptionKinds = []string{"debug", "profiling", "amortized", "measured"}

// Check runs the checker.
func Check(cfg Config) (*Result, error) {
	// The compiler names files relative to Dir, and the load names them
	// absolute.
	var err error
	if cfg.Dir, err = filepath.Abs(cfg.Dir); err != nil {
		return nil, err
	}
	if cfg.Root != "" {
		if cfg.Root, err = filepath.Abs(cfg.Root); err != nil {
			return nil, err
		}
	}
	env := cfg.Env
	if env == nil {
		env = os.Environ()
	}
	ours, err := listModule(cfg, env)
	if err != nil {
		return nil, err
	}
	// The compiler's escape report and the load run at once.
	type compiled struct {
		escapes []escape
		notes   []escape
		inlines []escape
		calls   []runtimeCall
		starts  map[string]asmPos
		err     error
	}
	done := make(chan compiled, 1)
	go func() {
		r, err := compilerEscapes(cfg, env, nil)
		if err != nil {
			done <- compiled{err: err}
			return
		}
		done <- compiled{r.escapes, r.notes, r.inlines, r.asm.calls, r.asm.starts, nil}
	}()
	pkgs, err := loadModule(cfg, env, ours)
	if err != nil {
		return nil, err
	}
	prog := ssa.NewProgram(pkgs[0].Fset, ssa.InstantiateGenerics)
	created := map[*types.Package]bool{}
	for _, p := range pkgs {
		prog.CreatePackage(p.Types, p.Syntax, p.TypesInfo, true)
		created[p.Types] = true
	}
	var createDeps func(tp *types.Package)
	createDeps = func(tp *types.Package) {
		for _, imp := range tp.Imports() {
			if !created[imp] {
				created[imp] = true
				prog.CreatePackage(imp, nil, nil, true)
				createDeps(imp)
			}
		}
	}
	for _, p := range pkgs {
		createDeps(p.Types)
	}
	prog.Build()

	allow, err := parseAllow(cfg.Allow)
	if err != nil {
		return nil, err
	}
	c := &checker{cfg: cfg, prog: prog, fset: prog.Fset, allow: allow,
		exempt: map[string]map[int]bool{}, lines: map[string][]string{}, appends: map[*ssa.Function]bool{},
		fixed: map[types.Type]bool{}, marks: map[*ast.Comment]bool{}, lineState: map[string]map[int]uint8{},
		moduleFiles: map[string]bool{}, inlinedBodies: map[*ssa.Function]bool{}}
	c.impls = map[string][]*ssa.Function{}
	c.files, c.fileInfo = map[string]*ast.File{}, map[string]*types.Info{}
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			name := filepath.Clean(prog.Fset.Position(f.Pos()).Filename)
			c.files[name], c.fileInfo[name] = f, p.TypesInfo
		}
	}
	for _, p := range pkgs {
		c.modulePkgs = append(c.modulePkgs, p.Types)
		for _, f := range p.CompiledGoFiles {
			c.moduleFiles[filepath.Clean(f)] = true
		}
	}
	slices.SortFunc(c.modulePkgs, func(a, b *types.Package) int { return strings.Compare(a.Path(), b.Path()) })
	var roots []*ssa.Function
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			roots = append(roots, c.scanFile(p, f)...)
		}
	}
	if len(c.stray) > 0 {
		return nil, fmt.Errorf("a //noalloc: comment that is not root or append on a function, fixed on a type, or exempt: %s",
			strings.Join(c.stray, ", "))
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("no //noalloc:root function in %v", cfg.Patterns)
	}
	if len(c.misshapen) > 0 {
		return nil, fmt.Errorf("marked //noalloc:append but not taking a slice first and returning it first: %s", strings.Join(c.misshapen, ", "))
	}
	c.scratch = c.scratchTypes()

	comp := <-done
	if comp.err != nil {
		return nil, comp.err
	}
	escapes := comp.escapes
	c.inlinedCalls = c.positions(comp.inlines)
	c.inlinedEscapes = c.positions(inlinedEscapes(escapes))
	c.graph()
	c.reach(groupRoots(cfg.Groups, roots))
	c.cover()
	c.linknames = linknames(pkgs)
	if err := c.loadSummaries(env); err != nil {
		return nil, err
	}

	res := &Result{}
	for _, r := range roots {
		res.Roots = append(res.Roots, r.String())
	}
	found := map[string]map[int]*Finding{}
	sites := map[site]bool{}
	add := func(fn *ssa.Function, at site) {
		if is, ok := c.coveredBy[fn]; ok {
			for _, i := range is {
				c.addCover(i, fn, at)
			}
			return
		}
		pos := at.pos
		file := c.rel(pos.Filename)
		if found[file] == nil {
			found[file] = map[int]*Finding{}
		}
		f := found[file][pos.Line]
		if f == nil {
			f = &Finding{File: file, Line: pos.Line, Source: c.lineTokens(pos.Filename, pos.Line), Func: c.stableName(fn),
				Group: c.group[fn], Path: c.path(fn)}
			found[file][pos.Line] = f
		}
		if !sites[at] {
			sites[at] = true
			f.Reasons = append(f.Reasons, at.reason)
		}
		f.sites = append(f.sites, at)
	}
	for _, fn := range c.order {
		if !c.ours(fn) || fn.Blocks == nil {
			continue
		}
		res.Reached++
		c.check(fn, add)
		c.recordLines(fn)
	}
	for _, fn := range c.coverOrder {
		if c.ours(fn) && fn.Blocks != nil {
			c.check(fn, add)
		}
	}
	c.matchEscapes(escapes, comp.notes, add)
	res.CrossCheck = c.crossCheck(comp.calls, comp.starts, found)
	for f, kind := range c.allow.assumed {
		res.Assumptions = append(res.Assumptions, Assumption{Kind: kind, Func: f, Used: c.reliedOn[f] || c.allow.used[f]})
	}
	slices.SortFunc(res.Assumptions, func(a, b Assumption) int { return strings.Compare(a.Func, b.Func) })
	for i := range c.exemptions {
		if i < len(c.covered) {
			for at := range c.covered[i] {
				c.exemptions[i].Covers = append(c.exemptions[i].Covers, at.label)
			}
			slices.Sort(c.exemptions[i].Covers)
		}
	}
	res.Exemptions = c.exemptions

	for _, byLine := range found {
		for _, f := range byLine {
			for _, at := range f.sites {
				if w := c.whyAt[at]; w != "" {
					if f.Why == nil {
						f.Why = map[string]string{}
					}
					f.Why[at.reason] = w
				}
			}
			f.sites = nil
			res.Findings = append(res.Findings, *f)
		}
	}
	slices.SortFunc(res.Findings, func(a, b Finding) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		return a.Line - b.Line
	})
	slices.SortFunc(res.Exemptions, func(a, b Exemption) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		return a.Line - b.Line
	})
	return res, nil
}

// Report is res as text: a summary, the count of findings in each file, each
// finding with the call path that reaches its function, the exemptions, and
// the allowlist's lines with whether a check relied on each.
func Report(res *Result) string {
	var b strings.Builder
	files := map[string]int{}
	for _, f := range res.Findings {
		files[f.File]++
	}
	fmt.Fprintf(&b, "%d lines allocate in %d files, in %d functions reached from %d roots:\n", len(res.Findings), len(files), res.Reached, len(res.Roots))
	for _, r := range res.Roots {
		fmt.Fprintf(&b, "  %s\n", r)
	}
	names := slices.Sorted(maps.Keys(files))
	slices.SortStableFunc(names, func(x, y string) int { return files[y] - files[x] })
	b.WriteString("\nLines per file:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "%6d  %s\n", files[n], n)
	}
	b.WriteString("\nFindings:\n")
	shown := map[string]bool{}
	for _, f := range res.Findings {
		fmt.Fprintf(&b, "%s\n", f)
		for _, r := range f.Reasons {
			if w := f.Why[r]; w != "" {
				fmt.Fprintf(&b, "    because %s\n", w)
			}
		}
		if !shown[f.Func] {
			shown[f.Func] = true
			fmt.Fprintf(&b, "    reached by %s\n", strings.Join(f.Path, " > "))
		}
	}
	covered, empty, unreached := 0, 0, 0
	for _, e := range res.Exemptions {
		covered += len(e.Covers)
		switch {
		case !e.Reached:
			unreached++
		case len(e.Covers) == 0 && !e.InSource:
			empty++
		}
	}
	fmt.Fprintf(&b, "\n%d exemptions, covering %d allocations; %d exempt nothing, and no root reaches %d:\n",
		len(res.Exemptions), covered, empty, unreached)
	for _, e := range res.Exemptions {
		note := ""
		switch {
		case !e.Reached:
			note = " (no root reaches it)"
		case len(e.Covers) == 0 && !e.InSource:
			note = " (exempts nothing)"
		}
		fmt.Fprintf(&b, "%s:%d: %s: %s%s\n", e.File, e.Line, e.Func, e.Reason, note)
		for _, c := range e.Covers {
			fmt.Fprintf(&b, "    covers %s\n", c)
		}
	}
	unused := 0
	for _, a := range res.Assumptions {
		if !a.Used {
			unused++
		}
	}
	fmt.Fprintf(&b, "\n%d allowlist lines; no check relied on %d:\n", len(res.Assumptions), unused)
	for _, a := range res.Assumptions {
		note := ""
		if !a.Used {
			note = " (nothing relied on it)"
		}
		fmt.Fprintf(&b, "%s: %s%s\n", a.Kind, a.Func, note)
	}
	return b.String()
}

// listModule returns the module's packages the patterns reach.
func listModule(cfg Config, env []string) (ours []string, err error) {
	graph, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports |
		packages.NeedDeps, Dir: cfg.Dir, Env: env, Overlay: cfg.Overlay}, cfg.Patterns...)
	if err != nil {
		return nil, err
	}
	packages.Visit(graph, nil, func(p *packages.Package) {
		if cfg.inModule(p.PkgPath) {
			ours = append(ours, p.PkgPath)
		}
	})
	return ours, nil
}

// loadModule loads the packages ours names from syntax; every other package's
// types come from export data.
func loadModule(cfg Config, env []string, ours []string) ([]*packages.Package, error) {
	mode := packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports |
		packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo
	pkgs, err := packages.Load(&packages.Config{Mode: mode, Dir: cfg.Dir, Env: env, Overlay: cfg.Overlay}, ours...)
	if err != nil {
		return nil, err
	}
	var errs []string
	for _, p := range pkgs {
		for _, e := range p.Errors {
			errs = append(errs, e.Error())
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("loading %v: %s", cfg.Patterns, strings.Join(errs[:min(len(errs), 5)], "\n"))
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no package of %s in %v", cfg.Module, cfg.Patterns)
	}
	return pkgs, nil
}

type checker struct {
	cfg   Config
	prog  *ssa.Program
	fset  *token.FileSet
	allow allowlist
	cg    *callgraph.Graph
	cha   *callgraph.Graph
	// modulePkgs are the module's packages, by path, and impls the methods
	// implementers found for each interface method.
	modulePkgs []*types.Package
	impls      map[string][]*ssa.Function
	// files holds the module's syntax by absolute path, and fileInfo the type
	// information each file was checked with.
	files    map[string]*ast.File
	fileInfo map[string]*types.Info

	// exempt holds the exempt lines of each file, by absolute path, and
	// exemptions the comments that exempt them.
	exempt map[string]map[int]bool
	// written holds the fields writtenFields found.
	written map[field]bool
	// lineOnly holds the exempt lines whose calls are still walked.
	lineOnly   map[string]map[int]bool
	exemptions []Exemption
	// exemptionAt indexes exemptions by file and line; coveredBy holds the
	// exemptions each function is reached through when no root reaches it
	// another way, coverOrder those functions in the order first reached,
	// and covered each exemption's Covers as a set.
	exemptionAt map[string]map[int]int
	coveredBy   map[*ssa.Function][]int
	coverOrder  []*ssa.Function
	covered     []map[coverSite]bool
	lines       map[string][]string

	// appends holds the module's functions marked //noalloc:append, and
	// misshapen the marked ones not shaped like append.
	appends   map[*ssa.Function]bool
	misshapen []string
	// fixed holds the module's types marked //noalloc:fixed.
	fixed map[types.Type]bool
	// lineState holds how each line of a reached function is checked, by
	// absolute path, and moduleFiles the module's source files, for CrossCheck.
	lineState   map[string]map[int]uint8
	moduleFiles map[string]bool
	// inlinedBodies are the functions whose escapes the compiler reports only
	// where they are inlined.
	inlinedBodies map[*ssa.Function]bool
	// marks holds the directives read off a declaration, and stray every other
	// //noalloc: comment: a misspelling, or a mark where no declaration takes it.
	marks map[*ast.Comment]bool
	stray []string

	// scratch holds the types whose slice and map fields are reused memory,
	// and scratchRoots the Scratch types themselves.
	scratch, scratchRoots map[types.Type]bool

	// parent is how reach first came to each function it reached, nil for a
	// root, order the functions in the order reach found them, and group the
	// name of the group that reached each first.
	parent map[*ssa.Function]*ssa.Function
	order  []*ssa.Function
	group  map[*ssa.Function]string
	// sites holds the callees of each call site reach followed, and callers
	// the sites that reach followed into each function.
	sites   map[ssa.CallInstruction][]*ssa.Function
	callers map[*ssa.Function][]ssa.CallInstruction
	// blocks holds the allowed blocks of each module function reach entered.
	blocks map[*ssa.Function]map[*ssa.BasicBlock]bool
	// failing holds, for each function reach entered, which of its failures
	// end the root's call, and so may allocate: the strictest of every way
	// reach came to it.
	failing map[*ssa.Function]failures
	// roots holds every group's roots, whose failures end their own call
	// however else reach comes to them.
	roots map[*ssa.Function]bool
	// carry memoizes carries.
	carry map[carryKey]bool
	// flags memoizes failureFlags.
	flags map[carryKey]map[int]bool
	// world, when set, is which packages are the module's, in place of
	// Config's: for summaries, everything but the caller's module.
	world func(path string) bool
	// byLine indexes functions by the lines of their code, for funcsAt.
	byLine map[string]map[int][]*ssa.Function
	// appended, when set, answers returnsAppended as summaries are worked
	// out, for a summarizing checker.
	appended func(*ssa.Function) bool
	// sums holds the summary of each function outside the module a reached
	// call calls.
	sums map[string]summary
	// callWhy holds, for each call judged by a summary that allocates, why,
	// and whyAt the same for each site a finding records.
	callWhy map[ssa.CallInstruction]string
	whyAt   map[site]string
	// reliedOn holds the allowlist lines a summary this check read relied
	// on, and linknames the symbols the module's functions with no Go body
	// are bound to, as the summarizer's linknames has them.
	reliedOn  map[string]bool
	linknames map[string]string
	// inlinedCalls holds the module's calls the compiler inlined, and
	// inlinedEscapes those it reports an inlined body's escape at, other
	// than a constant's, by position.
	inlinedCalls, inlinedEscapes map[token.Pos]bool
	// stops holds, for each function reach entered, the signals it returns
	// that every caller reach came to it from stops on.
	stops map[*ssa.Function]map[signal]bool

	nonNil  memo[*ssa.Function]
	fromScr memo[ssa.Value]
	// denied holds the variables grounded is asking about, and groundedAt
	// its answers.
	fromPtr    memo[ssa.Value]
	denied     map[*ssa.Alloc]bool
	groundedAt map[*ssa.Alloc]bool
}

func (c *checker) inModule(path string) bool { return c.cfg.inModule(path) }

// inModule reports whether path is a package of Module or of a source module.
func (cfg Config) inModule(path string) bool {
	for _, m := range append([]string{cfg.Module}, cfg.SourceModules...) {
		if path == m || strings.HasPrefix(path, m+"/") {
			return true
		}
	}
	return false
}

// ours reports whether fn is a source function of the module.
func (c *checker) ours(fn *ssa.Function) bool {
	if wrapper(fn) {
		return false
	}
	if o := fn.Origin(); o != nil {
		fn = o
	}
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	if fn.Pkg == nil {
		return false
	}
	if c.world != nil {
		return c.world(fn.Pkg.Pkg.Path())
	}
	// sync is checked as outside code even from source: a sync.Once's
	// function is setup that runs once, and a lock is no call into what it
	// guards.
	if fn.Pkg.Pkg.Path() == "sync" {
		return false
	}
	return c.inModule(fn.Pkg.Pkg.Path())
}

// wrapper reports whether fn is a function SSA made up, such as a bound
// method or a promoted method's wrapper, rather than one from source; an
// instance of a generic function counts as its source, and so does the body
// of a loop over a function iterator.
func wrapper(fn *ssa.Function) bool {
	return fn.Synthetic != "" && fn.Origin() == nil && !strings.HasPrefix(fn.Synthetic, "range-over-func")
}

// outside reports whether fn belongs to a package outside the module.
func (c *checker) outside(fn *ssa.Function) bool {
	return !c.ours(fn) && topLevel(fn).Pkg != nil
}

func (c *checker) rel(file string) string {
	if c.cfg.Root == "" {
		return file
	}
	if r, err := filepath.Rel(c.cfg.Root, file); err == nil {
		return filepath.ToSlash(r)
	}
	return file
}

// stableName is fn's name with each closure's index, which SSA counts in the
// order closures appear in their parent, replaced by the tokens of the line
// the closure starts on, so a closure added above another leaves its name
// alone.
func (c *checker) stableName(fn *ssa.Function) string {
	if o := fn.Origin(); o != nil {
		fn = o
	}
	if fn.Parent() == nil || !fn.Pos().IsValid() {
		return funcName(fn)
	}
	pos := c.prog.Fset.Position(fn.Pos())
	return c.stableName(fn.Parent()) + "$(" + strings.ReplaceAll(c.lineTokens(pos.Filename, pos.Line), "|", "¦") + ")"
}

func funcName(fn *ssa.Function) string {
	if o := fn.Origin(); o != nil {
		fn = o
	}
	return fn.RelString(nil)
}

// scanFile records f's exemptions and returns its //noalloc:root functions.
func (c *checker) scanFile(p *packages.Package, f *ast.File) []*ssa.Function {
	var roots []*ssa.Function
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
			c.scanTypes(p, gd)
			continue
		}
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Doc == nil {
			continue
		}
		for _, cm := range fd.Doc.List {
			mark := strings.TrimSpace(cm.Text)
			if mark != "//noalloc:root" && mark != "//noalloc:append" {
				continue
			}
			obj, ok := p.TypesInfo.Defs[fd.Name].(*types.Func)
			if !ok {
				continue
			}
			fn := c.prog.FuncValue(obj)
			if fn == nil {
				continue
			}
			c.marks[cm] = true
			roots = append(roots, fn)
			if mark == "//noalloc:append" {
				if !appendShaped(obj) {
					c.misshapen = append(c.misshapen, c.fset.Position(fd.Pos()).String()+": "+fd.Name.Name)
				}
				c.appends[fn] = true
			}
		}
	}
	for _, g := range f.Comments {
		for _, cm := range g.List {
			if !strings.HasPrefix(cm.Text, "//noalloc:") || c.marks[cm] {
				continue
			}
			reason, ok := strings.CutPrefix(cm.Text, "//noalloc:exempt")
			if !ok || reason != "" && reason[0] != ' ' && reason[0] != '\t' {
				c.stray = append(c.stray, c.fset.Position(cm.Pos()).String()+": "+cm.Text)
				continue
			}
			pos := c.fset.Position(cm.Pos())
			line := pos.Line
			if src := c.source(pos.Filename); line-1 < len(src) && pos.Column-1 <= len(src[line-1]) &&
				strings.TrimSpace(src[line-1][:pos.Column-1]) == "" {
				line++
			}
			if c.exempt[pos.Filename] == nil {
				c.exempt[pos.Filename] = map[int]bool{}
			}
			c.exempt[pos.Filename][line] = true
			if kind, _, ok := strings.Cut(strings.TrimSpace(reason), ":"); ok && slices.Contains(c.cfg.LineOnly, kind) {
				if c.lineOnly == nil {
					c.lineOnly = map[string]map[int]bool{}
				}
				if c.lineOnly[pos.Filename] == nil {
					c.lineOnly[pos.Filename] = map[int]bool{}
				}
				c.lineOnly[pos.Filename][line] = true
			}
			if c.exemptionAt == nil {
				c.exemptionAt = map[string]map[int]int{}
			}
			if c.exemptionAt[pos.Filename] == nil {
				c.exemptionAt[pos.Filename] = map[int]int{}
			}
			c.exemptionAt[pos.Filename][line] = len(c.exemptions)
			inSource := p.PkgPath != c.cfg.Module && !strings.HasPrefix(p.PkgPath, c.cfg.Module+"/")
			c.exemptions = append(c.exemptions, Exemption{File: c.rel(pos.Filename), Line: line,
				Func: enclosingFunc(p, f, cm.Pos()), Reason: strings.TrimSpace(reason), InSource: inSource})
		}
	}
	return roots
}

// scanTypes records the types gd declares that are marked //noalloc:fixed.
func (c *checker) scanTypes(p *packages.Package, gd *ast.GenDecl) {
	for _, spec := range gd.Specs {
		ts := spec.(*ast.TypeSpec)
		doc := ts.Doc
		if doc == nil && len(gd.Specs) == 1 {
			doc = gd.Doc
		}
		if doc == nil {
			continue
		}
		for _, cm := range doc.List {
			if strings.TrimSpace(cm.Text) == "//noalloc:fixed" {
				if obj := p.TypesInfo.Defs[ts.Name]; obj != nil {
					c.fixed[obj.Type()] = true
					c.marks[cm] = true
				}
			}
		}
	}
}

// foreignStore reports whether st puts a slice or map into a field of a
// Scratch or fixed type that is not memory the Scratch already holds: the
// caller's memory, or another value's, which the Scratch would then reuse as
// its own. A fresh make is reported where it is made.
func (c *checker) foreignStore(st *ssa.Store) bool {
	fa, ok := st.Addr.(*ssa.FieldAddr)
	if !ok {
		return false
	}
	ptr, ok := fa.X.Type().Underlying().(*types.Pointer)
	if !ok || !c.isFixed(ptr.Elem()) && !(c.scratch[ptr.Elem()] && c.scratchPtr(fa.X)) {
		return false
	}
	switch st.Val.Type().Underlying().(type) {
	case *types.Slice, *types.Map:
	default:
		return false
	}
	// What the stored slice is a view of: a parameter, a captured variable,
	// or memory loaded from somewhere. A call's result is judged where it is
	// made, by the rules for appends and escapes.
	root := st.Val
	for {
		switch v := root.(type) {
		case *ssa.Slice:
			root = v.X
			continue
		case *ssa.ChangeType:
			root = v.X
			continue
		}
		break
	}
	switch root.(type) {
	case *ssa.Parameter, *ssa.FreeVar, *ssa.UnOp:
	default:
		return false
	}
	// Holding another value's memory only to read it is harmless; reusing
	// it is not, so only a field something writes into counts.
	if !c.writtenFields()[fieldOf(fa)] {
		return false
	}
	return !c.reused(st.Val)
}

// field is one field of a struct type.
type field struct {
	t types.Type
	i int
}

func fieldOf(fa *ssa.FieldAddr) field {
	return field{fa.X.Type().Underlying().(*types.Pointer).Elem(), fa.Field}
}

// writtenFields is every slice field that reached code appends to or writes
// an element of, found once.
func (c *checker) writtenFields() map[field]bool {
	if c.written != nil {
		return c.written
	}
	c.written = map[field]bool{}
	// loaded is the field a value was loaded from, through reslicing.
	loaded := func(v ssa.Value) (field, bool) {
		for {
			switch x := v.(type) {
			case *ssa.Slice:
				v = x.X
				continue
			case *ssa.UnOp:
				if fa, ok := x.X.(*ssa.FieldAddr); ok && x.Op == token.MUL {
					return fieldOf(fa), true
				}
			}
			return field{}, false
		}
	}
	for _, fn := range c.order {
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				switch in := in.(type) {
				case *ssa.Store:
					if ia, ok := in.Addr.(*ssa.IndexAddr); ok {
						if f, ok := loaded(ia.X); ok {
							c.written[f] = true
						}
					}
				case *ssa.Call:
					var dst ssa.Value
					if bl, ok := in.Call.Value.(*ssa.Builtin); ok && bl.Name() == "append" && len(in.Call.Args) > 0 {
						dst = in.Call.Args[0]
					} else if f := in.Call.StaticCallee(); f != nil && (c.returnsAppended(f) || c.appendRoot(f)) {
						dst = appendedTo(&in.Call, f)
					}
					if dst == nil {
						continue
					}
					if f, ok := loaded(dst); ok {
						c.written[f] = true
					}
				}
			}
		}
	}
	return c.written
}

// isFixed reports whether t, or the generic type it instantiates, is marked
// //noalloc:fixed.
func (c *checker) isFixed(t types.Type) bool {
	if n, ok := t.(*types.Named); ok {
		t = n.Origin()
	}
	return c.fixed[t]
}

// appendRoot reports whether fn, or the generic function it instantiates, is
// marked //noalloc:append.
func (c *checker) appendRoot(fn *ssa.Function) bool {
	if o := fn.Origin(); o != nil {
		fn = o
	}
	return c.appends[fn]
}

// appendedTo is the slice a call of a function shaped like append appends
// to: its first argument, after the receiver of a method.
func appendedTo(cm *ssa.CallCommon, callee *ssa.Function) ssa.Value {
	i := 0
	if callee.Signature.Recv() != nil && !cm.IsInvoke() {
		i = 1
	}
	if i >= len(cm.Args) {
		return nil
	}
	return cm.Args[i]
}

// appendShaped reports whether fn takes a slice first and returns the same
// type first, as a function marked //noalloc:append must.
func appendShaped(fn *types.Func) bool {
	sig := fn.Type().(*types.Signature)
	if sig.Params().Len() == 0 || sig.Results().Len() == 0 {
		return false
	}
	first := sig.Params().At(0).Type()
	_, slice := first.Underlying().(*types.Slice)
	return slice && types.Identical(first, sig.Results().At(0).Type())
}

// lineTokens is line n of file as its Go tokens without comments, a space
// only between two words, so spacing and comments leave it alone.
func (c *checker) lineTokens(file string, n int) string {
	lines := c.source(file)
	if n < 1 || n > len(lines) {
		return ""
	}
	src := []byte(lines[n-1])
	var s scanner.Scanner
	s.Init(token.NewFileSet().AddFile("", -1, len(src)), src, func(token.Position, string) {}, 0)
	var b strings.Builder
	word := func(t token.Token) bool { return t.IsLiteral() || t.IsKeyword() }
	prev := token.ILLEGAL
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		if word(prev) && word(tok) {
			b.WriteByte(' ')
		}
		prev = tok
		if lit != "" {
			b.WriteString(lit)
		} else {
			b.WriteString(tok.String())
		}
	}
	return b.String()
}

// source is file's lines as the check reads them: the overlay's, if it
// replaces the file.
func (c *checker) source(file string) []string {
	if l, ok := c.lines[file]; ok {
		return l
	}
	b, ok := c.cfg.Overlay[file]
	if !ok {
		b, _ = os.ReadFile(file)
	}
	l := strings.Split(string(b), "\n")
	c.lines[file] = l
	return l
}

func enclosingFunc(p *packages.Package, f *ast.File, pos token.Pos) string {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || pos < fd.Pos() || pos >= fd.End() {
			continue
		}
		if obj, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
			return strings.TrimPrefix(obj.FullName(), p.PkgPath+".")
		}
		return fd.Name.Name
	}
	return "(package level)"
}

// graph builds the call graph: VTA over every function of the module, which
// sees where values get their concrete types, starting from CHA.
func (c *checker) graph() {
	c.cha = cha.CallGraph(c.prog)
	c.cg = vta.CallGraph(ssautil.AllFunctions(c.prog), c.cha)
}

// rootGroup is the roots of one group.
type rootGroup struct {
	name  string
	roots []*ssa.Function
}

// groupRoots sorts roots into groups, in the groups' order, with every root
// no group names in a last group with no name.
func groupRoots(groups []Group, roots []*ssa.Function) []rootGroup {
	var out []rootGroup
	placed := map[*ssa.Function]bool{}
	for _, g := range groups {
		rg := rootGroup{name: g.Name}
		for _, r := range roots {
			if slices.Contains(g.Roots, r.String()) && !placed[r] {
				rg.roots = append(rg.roots, r)
				placed[r] = true
			}
		}
		out = append(out, rg)
	}
	rest := rootGroup{}
	for _, r := range roots {
		if !placed[r] {
			rest.roots = append(rest.roots, r)
		}
	}
	return append(out, rest)
}

// reach walks the call graph from each group's roots in turn, so a function
// belongs to the first group that reaches it. It follows calls only from
// blocks a module function runs on its normal path and from lines that are
// not exempt. Functions outside the module have no bodies, so a call out of
// it also reaches what it hands over: a module function or closure, or the
// module methods of a value it boxes. Package sync is the exception: its
// Once and OnceFunc run their functions once.
func (c *checker) reach(groups []rootGroup) {
	c.parent = map[*ssa.Function]*ssa.Function{}
	c.group = map[*ssa.Function]string{}
	c.sites = map[ssa.CallInstruction][]*ssa.Function{}
	c.callers = map[*ssa.Function][]ssa.CallInstruction{}
	c.blocks = map[*ssa.Function]map[*ssa.BasicBlock]bool{}
	c.failing = map[*ssa.Function]failures{}
	c.roots = map[*ssa.Function]bool{}
	c.carry = map[carryKey]bool{}
	c.flags = map[carryKey]map[int]bool{}
	c.stops = map[*ssa.Function]map[signal]bool{}
	for _, g := range groups {
		for _, r := range g.roots {
			c.roots[r] = true
		}
	}
	for _, g := range groups {
		queue := []*ssa.Function{}
		for _, r := range g.roots {
			if _, ok := c.parent[r]; !ok {
				c.parent[r] = nil
				c.failing[r] = failOnce
				queue = append(queue, r)
			}
		}
		c.walk(queue, g.name)
	}
}

// failures is which of a function's failures end the call of the root that
// reached it, so that each costs the root's call a fixed number of
// allocations however much input it reads.
type failures uint8

const (
	// failErrors: a non-nil error it returns reaches the root's return.
	failErrors failures = 1 << iota
	// failPanics: a panic in it is not recovered below the root.
	failPanics
	// failOnce: it runs at most once in a root's call, so every failure of
	// its own, a recorded one that it goes on from included, happens at
	// most once.
	failOnce = 1<<iota | failErrors | failPanics
)

// failuresOf is how fn's failures may allocate: as reach found them, or as
// a root's for a function reach did not enter.
func (c *checker) failuresOf(fn *ssa.Function) failures {
	if f, ok := c.failing[fn]; ok {
		return f
	}
	return failOnce
}

// callees returns what a call at site in fn, whose allowed blocks are ok,
// reaches through callee: callee, and for a callee outside the module or
// with no Go body the module functions the call hands it, with the failures
// and stop signals each gets from the call.
func (c *checker) callees(fn *ssa.Function, site ssa.CallInstruction, callee *ssa.Function, ok map[*ssa.BasicBlock]bool) ([]*ssa.Function, []failures, []map[signal]bool) {
	next := []*ssa.Function{callee}
	fails := []failures{c.calleeFailures(fn, site, callee, ok)}
	stops := []map[signal]bool{c.stopsAt(fn, site, callee, ok)}
	if c.outside(callee) || c.intoStd(fn, callee) || callee.Blocks == nil {
		if top := topLevel(callee); top.Pkg != nil && top.Pkg.Pkg.Path() == "sync" {
			return nil, nil, nil
		}
		for _, f := range c.handedOver(site) {
			next = append(next, f)
			fails = append(fails, 0)
			stops = append(stops, nil)
		}
	}
	for i, f := range next {
		if c.roots[f] {
			fails[i] = failOnce
		}
	}
	return next, fails, stops
}

// intoStd reports whether a call from fn to callee leaves code outside the
// standard library for code in it, which can call what it is handed by
// reflection, a call the call graph cannot follow.
func (c *checker) intoStd(fn, callee *ssa.Function) bool {
	to, from := topLevel(callee), topLevel(fn)
	return to.Pkg != nil && c.standard(to.Pkg.Pkg.Path()) && from.Pkg != nil && !c.standard(from.Pkg.Pkg.Path())
}

// standard reports whether path is a standard library package: one outside
// the module whose first element, as Go has it, has no dot.
func (c *checker) standard(path string) bool {
	if path == c.cfg.Module || strings.HasPrefix(path, c.cfg.Module+"/") {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// calleeFailures is how the failures of callee, called at site in fn, whose
// allowed blocks are ok, may allocate. A call fn can make again in one of
// its calls runs more than once, and a call out of the module hands its
// functions to code that may call them any number of times, recover their
// panics, and drop their errors.
func (c *checker) calleeFailures(fn *ssa.Function, site ssa.CallInstruction, callee *ssa.Function, ok map[*ssa.BasicBlock]bool) failures {
	from := c.failuresOf(fn)
	if !c.ours(fn) {
		// A wrapper the compiler writes, such as a method value or a value
		// method called through a pointer, only passes its call on.
		if fn.Synthetic != "" {
			return from
		}
		return 0
	}
	if from&failOnce == failOnce && callee != fn && !inLoop(site.Block()) {
		return failOnce
	}
	var f failures
	if from&failErrors != 0 && c.propagatesError(site, callee, ok) {
		f |= failErrors
	}
	if from&failPanics != 0 && !defersRecover(callee) {
		f |= failPanics
	}
	return f
}

// propagatesError reports whether fn, whose allowed blocks are ok, fails
// whenever callee, called at site, returns a non-nil error: every path from
// the call on which the error is not nil ends in a failure or returns it. A
// callee with no error result has none to propagate.
func (c *checker) propagatesError(site ssa.CallInstruction, callee *ssa.Function, ok map[*ssa.BasicBlock]bool) bool {
	idx := errorResult(callee)
	if idx < 0 {
		return true
	}
	call, isCall := site.(*ssa.Call)
	if !isCall {
		return false
	}
	err := ssa.Value(call)
	if callee.Signature.Results().Len() > 1 {
		err = nil
		for _, r := range *call.Referrers() {
			if x, isExtract := r.(*ssa.Extract); isExtract && x.Index == idx {
				err = x
			}
		}
		if err == nil {
			return false
		}
	}
	fn, idx := site.Parent(), errorResult(site.Parent())
	start := &facts{is: map[ssa.Value]bool{err: true}, flags: map[ssa.Value]bool{}, fields: map[valueField]bool{}}
	for j, v := range c.errorFlags(callee) {
		for _, r := range *call.Referrers() {
			if x, isExtract := r.(*ssa.Extract); isExtract && x.Index == j {
				start.flags[x] = v
			}
		}
	}
	return c.pathsFrom(call.Block(), start, ok, c.failuresOf(fn)&failOnce == failOnce, func(ret *ssa.Return, f *facts) bool {
		return idx >= 0 && f.is[ret.Results[idx]] || c.endsAt(fn, ret, f)
	})
}

// errorFlags maps each bool result of fn, a module function, to the value it
// has on every return whose error may not be nil, for the results that have
// one.
func (c *checker) errorFlags(fn *ssa.Function) map[int]bool {
	idx := errorResult(fn)
	if idx < 0 || fn.Blocks == nil || !c.ours(fn) {
		return nil
	}
	return boolResults(fn, func(j int) (want, set bool) {
		for _, b := range fn.Blocks {
			ret, isRet := b.Instrs[len(b.Instrs)-1].(*ssa.Return)
			if !isRet {
				continue
			}
			if n, isConst := ret.Results[idx].(*ssa.Const); isConst && n.IsNil() {
				continue
			}
			k, isConst := ret.Results[j].(*ssa.Const)
			if !isConst || k.Value == nil || set && constant.BoolVal(k.Value) != want {
				return false, false
			}
			want, set = constant.BoolVal(k.Value), true
		}
		return want, set
	})
}

// boolResults maps each bool result j of fn to the value one gives it, for
// the results one reports a value for.
func boolResults(fn *ssa.Function, one func(j int) (v, ok bool)) map[int]bool {
	results := fn.Signature.Results()
	out := map[int]bool{}
	for j := range results.Len() {
		if b, isBasic := results.At(j).Type().Underlying().(*types.Basic); !isBasic || b.Kind() != types.Bool {
			continue
		}
		if v, ok := one(j); ok {
			out[j] = v
		}
	}
	return out
}

// A signal is a bool a function returns, its result'th result or, when field
// is not -1, that field of it, with the value v.
type signal struct {
	result, field int
	v             bool
}

// endsAt reports whether ret, a return of fn on a path from a failure that
// does not come back to it, ends what the failure is part of: fn runs once in
// a root's call, so the failure is made once, or ret returns a signal every
// caller stops on.
func (c *checker) endsAt(fn *ssa.Function, ret *ssa.Return, f *facts) bool {
	if c.failuresOf(fn)&failOnce == failOnce {
		return true
	}
	stops := c.stops[fn]
	for j, r := range ret.Results {
		if v, known := f.value(r); known && stops[signal{j, -1, v}] {
			return true
		}
		for k, v := range f.fields {
			if k.v == r && stops[signal{j, k.field, v}] {
				return true
			}
		}
	}
	return false
}

// stopsAt returns the signals of callee that fn, whose allowed blocks are ok,
// stops on at site, where its failures end the root's call: every path from
// the call on which callee returns the signal ends in a failure of fn, in a
// return of a non-nil error, or as endsAt has it.
func (c *checker) stopsAt(fn *ssa.Function, site ssa.CallInstruction, callee *ssa.Function, ok map[*ssa.BasicBlock]bool) map[signal]bool {
	call, isCall := site.(*ssa.Call)
	if !isCall || !c.ours(fn) || !c.ours(callee) || c.failuresOf(fn)&failErrors == 0 {
		return nil
	}
	results := callee.Signature.Results()
	resultValue := func(j int) ssa.Value {
		if results.Len() == 1 {
			return call
		}
		for _, r := range *call.Referrers() {
			if x, isExtract := r.(*ssa.Extract); isExtract && x.Index == j {
				return x
			}
		}
		return nil
	}
	idx := errorResult(fn)
	var out map[signal]bool
	for j := range results.Len() {
		r := resultValue(j)
		if r == nil {
			continue
		}
		var cands []signal
		switch t := results.At(j).Type().Underlying().(type) {
		case *types.Basic:
			if t.Kind() == types.Bool {
				cands = []signal{{j, -1, true}, {j, -1, false}}
			}
		case *types.Struct:
			for k := range t.NumFields() {
				if b, isBasic := t.Field(k).Type().Underlying().(*types.Basic); isBasic && b.Kind() == types.Bool {
					cands = append(cands, signal{j, k, true}, signal{j, k, false})
				}
			}
		}
		for _, s := range cands {
			start := &facts{is: map[ssa.Value]bool{}, flags: map[ssa.Value]bool{}, fields: map[valueField]bool{}}
			if s.field < 0 {
				start.flags[r] = s.v
			} else {
				start.fields[valueField{r, s.field}] = s.v
			}
			if c.pathsFrom(call.Block(), start, ok, c.failuresOf(fn)&failOnce == failOnce, func(ret *ssa.Return, f *facts) bool {
				return idx >= 0 && c.definitelyNonNil(ret.Results[idx], ret.Block(), map[ssa.Value]bool{}) || c.endsAt(fn, ret, f)
			}) {
				if out == nil {
					out = map[signal]bool{}
				}
				out[s] = true
			}
		}
	}
	return out
}

func subset(a, b map[signal]bool) bool {
	for s := range a {
		if !b[s] {
			return false
		}
	}
	return true
}

func intersect(a, b map[signal]bool) map[signal]bool {
	var out map[signal]bool
	for s := range a {
		if b[s] {
			if out == nil {
				out = map[signal]bool{}
			}
			out[s] = true
		}
	}
	return out
}

// errorEnds reports whether every path from b on which err is a failure
// ends in a panic or in a return of an error that cannot be nil as b's
// function's error, as failurePaths follows them.
func (c *checker) errorEnds(b *ssa.BasicBlock, err ssa.Value) bool {
	idx := errorResult(b.Parent())
	return c.failurePaths(b, err, nil, func(ret *ssa.Return, f *facts) bool {
		if idx < 0 {
			return false
		}
		v := ret.Results[idx]
		return f.is[v] || c.definitelyNonNil(v, ret.Block(), map[ssa.Value]bool{})
	})
}

// failurePaths reports whether every path from b on which err is a failure,
// not nil and not a package-level sentinel, ends in a block not in ok, in a
// panic, or in a return that ret accepts, given what is known on the path. A
// nil ok counts no block as a failure. It follows err through the phis it
// reaches and into a module helper that returns it, along with each bool the
// helper returns that has one value whenever err is a failure, and gives up
// on a path that comes back to b or runs past a fixed number of steps.
func (c *checker) failurePaths(b *ssa.BasicBlock, err ssa.Value, ok map[*ssa.BasicBlock]bool, ret func(*ssa.Return, *facts) bool) bool {
	return c.pathsFrom(b, &facts{is: map[ssa.Value]bool{err: true}, flags: map[ssa.Value]bool{}, fields: map[valueField]bool{}}, ok, false, ret)
}

// facts is what a path from a failure knows: the values that hold the
// failure, bools with a known value, and bool fields with a known value, of
// a struct value or of the struct a local variable holds.
type facts struct {
	is, flags map[ssa.Value]bool
	fields    map[valueField]bool
}

type valueField struct {
	v     ssa.Value
	field int
}

func (f *facts) clone() *facts {
	return &facts{maps.Clone(f.is), maps.Clone(f.flags), maps.Clone(f.fields)}
}

// forgetShared forgets the fields a call can write: those of a variable
// whose address is taken other than to read or write a field, or for a call
// deferred to the function's return.
func (f *facts) forgetShared() {
	for k := range f.fields {
		if a, isAlloc := k.v.(*ssa.Alloc); isAlloc && addressTaken(a) {
			delete(f.fields, k)
		} else if _, isPtr := k.v.Type().Underlying().(*types.Pointer); isPtr && !isAlloc {
			delete(f.fields, k)
		}
	}
}

func addressTaken(a *ssa.Alloc) bool {
	for _, r := range *a.Referrers() {
		switch r := r.(type) {
		case *ssa.FieldAddr:
			for _, rr := range *r.Referrers() {
				switch rr := rr.(type) {
				case *ssa.UnOp:
					if rr.Op != token.MUL {
						return true
					}
				case *ssa.Store:
					if rr.Val == ssa.Value(r) {
						return true
					}
				case *ssa.DebugRef:
				default:
					return true
				}
			}
		case *ssa.Store:
			if r.Val == ssa.Value(a) {
				return true
			}
		case *ssa.UnOp:
			if r.Op != token.MUL {
				return true
			}
		case *ssa.DebugRef, *ssa.Defer:
		default:
			return true
		}
	}
	return false
}

// copyFields gives to what dst holds the known fields of src.
func (f *facts) copyFields(dst, src ssa.Value) {
	for k := range f.fields {
		if k.v == dst {
			delete(f.fields, k)
		}
	}
	for k, v := range f.fields {
		if k.v == src {
			f.fields[valueField{dst, k.field}] = v
		}
	}
}

// pathsFrom is failurePaths from b with start known. With once, b's function
// runs once in a root's call, so a path that can no longer come back to b
// ends.
func (c *checker) pathsFrom(b *ssa.BasicBlock, start *facts, ok map[*ssa.BasicBlock]bool, once bool, ret func(*ssa.Return, *facts) bool) bool {
	steps := 0
	onPath := map[*ssa.BasicBlock]bool{}
	var back map[*ssa.BasicBlock]bool
	if once {
		back = reachesBack(b)
	}
	var walk func(x, from *ssa.BasicBlock, f *facts) bool
	walk = func(x, from *ssa.BasicBlock, f *facts) bool {
		if ok != nil && !ok[x] {
			return true
		}
		if back != nil && !back[x] {
			return true
		}
		// Coming back to b runs it again with the failure dropped. Another
		// loop's way out is a path of its own.
		if x == b && from != nil {
			return false
		}
		if onPath[x] {
			return true
		}
		if steps++; steps > failurePathSteps {
			return false
		}
		f = f.clone()
		for _, in := range x.Instrs {
			switch in := in.(type) {
			case *ssa.Phi:
				if from != nil && f.is[in.Edges[slices.Index(x.Preds, from)]] {
					f.is[in] = true
				}
			case *ssa.Call:
				if out, k := c.carried(in, f.is); out != nil {
					f.is[out] = true
					for j, v := range c.failureFlags(in.Call.StaticCallee(), k) {
						for _, r := range *in.Referrers() {
							if x, isExtract := r.(*ssa.Extract); isExtract && x.Index == j {
								f.flags[x] = v
							}
						}
					}
				}
				f.forgetShared()
			case *ssa.Go:
				f.forgetShared()
			case *ssa.Store:
				switch addr := in.Addr.(type) {
				case *ssa.FieldAddr:
					if v, isConst := in.Val.(*ssa.Const); isConst && v.Value != nil && v.Value.Kind() == constant.Bool {
						f.fields[valueField{addr.X, addr.Field}] = constant.BoolVal(v.Value)
					} else {
						delete(f.fields, valueField{addr.X, addr.Field})
					}
				case *ssa.Alloc:
					f.copyFields(addr, in.Val)
				}
			case *ssa.UnOp:
				if _, isAlloc := in.X.(*ssa.Alloc); isAlloc && in.Op == token.MUL {
					f.copyFields(in, in.X)
				}
			}
		}
		succs := x.Succs
		switch last := x.Instrs[len(x.Instrs)-1].(type) {
		case *ssa.Panic:
			return true
		case *ssa.Return:
			return ret(last, f)
		case *ssa.If:
			if arm, decided := errArm(last, f.is); decided {
				succs = succs[arm : arm+1]
			} else if v, known := f.value(last.Cond); known {
				if v {
					succs = succs[:1]
				} else {
					succs = succs[1:]
				}
			}
		}
		if len(succs) == 0 {
			return false
		}
		onPath[x] = true
		defer delete(onPath, x)
		for _, s := range succs {
			if !walk(s, x, f) {
				return false
			}
		}
		return true
	}
	return walk(b, nil, start)
}

// reachesBack returns the blocks from which a path reaches b.
func reachesBack(b *ssa.BasicBlock) map[*ssa.BasicBlock]bool {
	out := map[*ssa.BasicBlock]bool{b: true}
	stack := []*ssa.BasicBlock{b}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range x.Preds {
			if !out[p] {
				out[p] = true
				stack = append(stack, p)
			}
		}
	}
	return out
}

// value is the value v, a bool, has on the path, when the path decides it: a
// known bool or field, its negation, or a comparison of a failure with nil.
func (f *facts) value(v ssa.Value) (bool, bool) {
	if b, known := f.flags[v]; known {
		return b, true
	}
	switch v := v.(type) {
	case *ssa.UnOp:
		switch v.Op {
		case token.NOT:
			b, known := f.value(v.X)
			return !b, known
		case token.MUL:
			if a, isField := v.X.(*ssa.FieldAddr); isField {
				b, known := f.fields[valueField{a.X, a.Field}]
				return b, known
			}
		}
	case *ssa.Field:
		b, known := f.fields[valueField{v.X, v.Field}]
		return b, known
	case *ssa.BinOp:
		if v.Op != token.EQL && v.Op != token.NEQ {
			break
		}
		x, y := v.X, v.Y
		if !f.is[x] {
			x, y = y, x
		}
		if n, isConst := y.(*ssa.Const); f.is[x] && isConst && n.IsNil() {
			return v.Op == token.NEQ, true
		}
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.Bool {
			return constant.BoolVal(v.Value), true
		}
	}
	return false, false
}

// failureFlags maps each bool result of fn to the value it has on every
// return where its k-th parameter, an error, is a failure, for the results
// that have one.
func (c *checker) failureFlags(fn *ssa.Function, k int) map[int]bool {
	key := carryKey{fn, k}
	if v, seen := c.flags[key]; seen {
		return v
	}
	c.flags[key] = nil
	out := boolResults(fn, func(j int) (want, set bool) {
		ok := c.failurePaths(fn.Blocks[0], fn.Params[k], nil, func(ret *ssa.Return, f *facts) bool {
			v, known := f.value(ret.Results[j])
			if !known || set && v != want {
				return false
			}
			want, set = v, true
			return true
		})
		return want, ok && set
	})
	c.flags[key] = out
	return out
}

// failurePathSteps bounds the blocks pathsFrom visits for one failure.
const failurePathSteps = 4096

// errArm is the successor an If takes when the error it tests, one of is,
// is neither nil nor a package-level sentinel, when the If decides it.
func errArm(br *ssa.If, is map[ssa.Value]bool) (int, bool) {
	cmp, isCmp := br.Cond.(*ssa.BinOp)
	if !isCmp || cmp.Op != token.EQL && cmp.Op != token.NEQ {
		return 0, false
	}
	other := cmp.Y
	if !is[cmp.X] {
		if !is[cmp.Y] {
			return 0, false
		}
		other = cmp.X
	}
	switch o := other.(type) {
	case *ssa.Const:
		if !o.IsNil() {
			return 0, false
		}
	case *ssa.UnOp:
		if _, isGlobal := o.X.(*ssa.Global); !isGlobal || o.Op != token.MUL {
			return 0, false
		}
	case *ssa.MakeInterface:
		// A constant error, which nothing allocates.
		if _, isConst := o.X.(*ssa.Const); !isConst {
			return 0, false
		}
	default:
		return 0, false
	}
	if cmp.Op == token.NEQ {
		return 0, true
	}
	return 1, true
}

// carried is the error result of call when call passes one of is, as its
// k-th argument, to a module function that returns it whenever it is a
// failure, or nil.
func (c *checker) carried(call *ssa.Call, is map[ssa.Value]bool) (ssa.Value, int) {
	callee := call.Call.StaticCallee()
	if callee == nil || !c.ours(callee) || callee.Blocks == nil {
		return nil, 0
	}
	idx := errorResult(callee)
	if idx < 0 {
		return nil, 0
	}
	for k, a := range call.Call.Args {
		if !is[a] || k >= len(callee.Params) || !c.carries(callee, k) {
			continue
		}
		if callee.Signature.Results().Len() == 1 {
			return call, k
		}
		for _, r := range *call.Referrers() {
			if x, isExtract := r.(*ssa.Extract); isExtract && x.Index == idx {
				return x, k
			}
		}
		return nil, 0
	}
	return nil, 0
}

// carries reports whether fn returns a non-nil error whenever its k-th
// parameter is a non-nil error other than a package-level sentinel.
func (c *checker) carries(fn *ssa.Function, k int) bool {
	key := carryKey{fn, k}
	if v, seen := c.carry[key]; seen {
		return v
	}
	c.carry[key] = false
	v := types.Identical(fn.Params[k].Type(), errorType) && c.errorEnds(fn.Blocks[0], fn.Params[k])
	c.carry[key] = v
	return v
}

type carryKey struct {
	fn *ssa.Function
	k  int
}

// defersRecover reports whether fn defers a function that calls recover,
// which stops a panic below it from ending a root's call.
func defersRecover(fn *ssa.Function) bool {
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			d, isDefer := in.(*ssa.Defer)
			if !isDefer {
				continue
			}
			var deferred *ssa.Function
			switch v := d.Call.Value.(type) {
			case *ssa.Function:
				deferred = v
			case *ssa.MakeClosure:
				deferred, _ = v.Fn.(*ssa.Function)
			}
			if deferred != nil && callsRecover(deferred) {
				return true
			}
		}
	}
	return false
}

func callsRecover(fn *ssa.Function) bool {
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			if call, isCall := in.(*ssa.Call); isCall {
				if builtin, isBuiltin := call.Call.Value.(*ssa.Builtin); isBuiltin && builtin.Name() == "recover" {
					return true
				}
			}
		}
	}
	return false
}

// walk reaches everything queue's functions call that nothing reached before,
// for group, and enters again a function reached once more with stricter
// failures, to check the blocks they no longer allow.
func (c *checker) walk(queue []*ssa.Function, group string) {
	entered := map[*ssa.Function]bool{}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if !entered[fn] {
			entered[fn] = true
			if _, seen := c.group[fn]; !seen {
				c.order = append(c.order, fn)
				c.group[fn] = group
			}
		}
		ours := c.ours(fn)
		var ok map[*ssa.BasicBlock]bool
		if ours {
			ok = c.allowedBlocks(fn)
			c.blocks[fn] = ok
			if testHookAllowed != nil {
				testHookAllowed(c, fn, ok)
			}
		}
		for _, e := range c.edges(fn) {
			if ours && (!ok[e.Site.Block()] || c.exemptAt(e.Site.Pos()) && !c.lineOnlyAt(e.Site.Pos())) {
				continue
			}
			callee := e.Callee.Func
			if !slices.Contains(c.sites[e.Site], callee) {
				c.sites[e.Site] = append(c.sites[e.Site], callee)
				c.callers[callee] = append(c.callers[callee], e.Site)
			}
			next, fails, stops := c.callees(fn, e.Site, callee, ok)
			for i, f := range next {
				if _, seen := c.parent[f]; !seen {
					c.parent[f] = fn
					c.failing[f], c.stops[f] = fails[i], stops[i]
					queue = append(queue, f)
				} else if was, ws := c.failuresOf(f), c.stops[f]; was&fails[i] != was || !subset(ws, stops[i]) {
					c.failing[f], c.stops[f] = was&fails[i], intersect(ws, stops[i])
					queue = append(queue, f)
				}
			}
		}
	}
}

// edges returns fn's calls in VTA's graph, with every module method that can
// answer each interface method call VTA resolves to nothing, and the callee of
// each static call VTA's graph lacks, as a generic method's instance can be.
func (c *checker) edges(fn *ssa.Function) []*callgraph.Edge {
	var out []*callgraph.Edge
	resolved := map[ssa.CallInstruction]bool{}
	if n := c.cg.Nodes[fn]; n != nil {
		out = append(out, n.Out...)
		for _, e := range n.Out {
			resolved[e.Site] = true
		}
	}
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			site, ok := in.(ssa.CallInstruction)
			if !ok || resolved[site] {
				continue
			}
			var callees []*ssa.Function
			if site.Common().IsInvoke() {
				callees = c.implementers(site.Common())
			} else if callee := site.Common().StaticCallee(); callee != nil {
				callees = []*ssa.Function{callee}
			}
			for _, callee := range callees {
				out = append(out, &callgraph.Edge{Site: site, Callee: &callgraph.Node{Func: callee}})
			}
		}
	}
	return out
}

// implementers returns the module's methods that an interface method call
// can reach: the method of every named type of the module, or of a pointer
// to one, that implements the interface, in a fixed order.
func (c *checker) implementers(cm *ssa.CallCommon) []*ssa.Function {
	iface, ok := cm.Value.Type().Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	key := cm.Value.Type().String() + "." + cm.Method.Name()
	if fns, ok := c.impls[key]; ok {
		return fns
	}
	var fns []*ssa.Function
	for _, p := range c.modulePkgs {
		scope := p.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok || named.TypeParams().Len() > 0 || types.IsInterface(named) {
				continue
			}
			for _, t := range []types.Type{named, types.NewPointer(named)} {
				if !types.Implements(t, iface) {
					continue
				}
				if fn := c.prog.LookupMethod(t, cm.Method.Pkg(), cm.Method.Name()); fn != nil {
					fns = append(fns, fn)
					break
				}
			}
		}
	}
	c.impls[key] = fns
	return fns
}

// handedOver returns the module functions a call passes out: functions and
// closures among its arguments, and the methods of module types it boxes.
func (c *checker) handedOver(site ssa.CallInstruction) []*ssa.Function {
	var out []*ssa.Function
	cm := site.Common()
	args := cm.Args
	if cm.IsInvoke() {
		args = append([]ssa.Value{cm.Value}, args...)
	}
	for _, a := range args {
		switch a := a.(type) {
		case *ssa.Function:
			out = append(out, a)
		case *ssa.MakeClosure:
			out = append(out, a.Fn.(*ssa.Function))
		case *ssa.MakeInterface:
			ms := c.prog.MethodSets.MethodSet(a.X.Type())
			for i := range ms.Len() {
				if f := c.prog.MethodValue(ms.At(i)); f != nil {
					out = append(out, f)
				}
			}
		}
	}
	return slices.DeleteFunc(out, func(f *ssa.Function) bool { return !c.ours(f) && !wrapper(f) })
}

// lineOnlyAt reports whether the exemption at pos leaves what its line calls
// to be checked.
func (c *checker) lineOnlyAt(pos token.Pos) bool {
	if !pos.IsValid() {
		return false
	}
	p := c.fset.Position(pos)
	return c.lineOnly[p.Filename][p.Line]
}

// exemptionIndex is the exemption on pos's line, if one is.
func (c *checker) exemptionIndex(pos token.Pos) (int, bool) {
	p := c.fset.Position(pos)
	i, ok := c.exemptionAt[p.Filename][p.Line]
	return i, ok
}

// A site is one allocation: a reason at a column of a line. The compiler can
// report one escape at a column more than once, which nth tells apart.
type site struct {
	pos    token.Position
	nth    int
	reason string
}

func newSite(pos token.Position, reason string) site {
	return site{pos: token.Position{Filename: pos.Filename, Line: pos.Line, Column: pos.Column}, reason: reason}
}

// addCover records that exemption i lets at through in fn.
func (c *checker) addCover(i int, fn *ssa.Function, at site) {
	for len(c.covered) <= i {
		c.covered = append(c.covered, nil)
	}
	if c.covered[i] == nil {
		c.covered[i] = map[coverSite]bool{}
	}
	c.covered[i][coverSite{at, c.stableName(fn) + ": " + at.reason}] = true
}

// A coverSite is an allocation an exemption covers, with how its list names
// it.
type coverSite struct {
	site
	label string
}

// cover walks, from each exempt line a root reaches whose kind is not
// LineOnly, what the line calls that reach left unreached, and records each
// function it finds as reached through that exemption. Check counts what
// those functions allocate as the exemption's, not as findings.
func (c *checker) cover() {
	c.coveredBy = map[*ssa.Function][]int{}
	// A function reached through several exemptions is each one's: any of
	// them taken away leaves it reached through the others, and all of them
	// taken away makes its allocations findings.
	var queue []*ssa.Function
	queued := map[*ssa.Function]bool{}
	// follow covers what e, a call in fn whose allowed blocks are ok, reaches,
	// with the failures walk would give it there.
	follow := func(fn *ssa.Function, ok map[*ssa.BasicBlock]bool, e *callgraph.Edge, is []int) {
		callee := e.Callee.Func
		if !slices.Contains(c.sites[e.Site], callee) {
			c.sites[e.Site] = append(c.sites[e.Site], callee)
		}
		next, fails, stops := c.callees(fn, e.Site, callee, ok)
		for i, f := range next {
			if _, reached := c.parent[f]; reached {
				continue
			}
			have := c.coveredBy[f]
			_, seen := c.coveredBy[f]
			if !seen {
				c.coverOrder = append(c.coverOrder, f)
			}
			stricter := false
			if was, ws := c.failuresOf(f), c.stops[f]; !seen {
				c.failing[f], c.stops[f] = fails[i], stops[i]
			} else if was&fails[i] != was || !subset(ws, stops[i]) {
				c.failing[f], c.stops[f] = was&fails[i], intersect(ws, stops[i])
				delete(c.blocks, f)
				stricter = true
			}
			grown := false
			for _, i := range is {
				if !slices.Contains(have, i) {
					have = append(have, i)
					grown = true
				}
			}
			c.coveredBy[f] = have
			if (grown || stricter) && !queued[f] {
				queued[f] = true
				queue = append(queue, f)
			}
		}
	}
	for _, fn := range c.order {
		if !c.ours(fn) {
			continue
		}
		ok := c.blocks[fn]
		for _, e := range c.edges(fn) {
			if !ok[e.Site.Block()] || c.lineOnlyAt(e.Site.Pos()) || !e.Site.Pos().IsValid() {
				continue
			}
			if i, exempt := c.exemptionIndex(e.Site.Pos()); exempt {
				c.exemptions[i].Reached = true
				follow(fn, ok, e, []int{i})
			}
		}
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		queued[fn] = false
		if !c.ours(fn) {
			continue
		}
		ok := c.blocks[fn]
		if ok == nil {
			ok = c.allowedBlocks(fn)
			c.blocks[fn] = ok
		}
		for _, e := range c.edges(fn) {
			if !ok[e.Site.Block()] || c.exemptAt(e.Site.Pos()) && !c.lineOnlyAt(e.Site.Pos()) {
				continue
			}
			follow(fn, ok, e, c.coveredBy[fn])
		}
	}
}

func (c *checker) exemptAt(pos token.Pos) bool {
	if !pos.IsValid() {
		return false
	}
	p := c.fset.Position(pos)
	return c.exempt[p.Filename][p.Line]
}

// path is the call path reach took from a root to fn, through the module's
// functions.
func (c *checker) path(fn *ssa.Function) []string {
	var p []string
	for f := fn; f != nil; f = c.parent[f] {
		if c.ours(f) {
			p = append(p, funcName(f))
		}
	}
	slices.Reverse(p)
	return p
}

// allowedBlocks returns fn's blocks that run on its normal path: not lazy
// first-use setup, and not its failures that end the root's call, as
// failuresOf has them: blocks that only lead to returning a non-nil error or
// to a panic, and blocks off any loop that make a new error to report one.
func (c *checker) allowedBlocks(fn *ssa.Function) map[*ssa.BasicBlock]bool {
	errIdx := errorResult(fn)
	fails := c.failuresOf(fn)
	bad := map[*ssa.BasicBlock]bool{}
	for _, b := range fn.Blocks {
		switch last := b.Instrs[len(b.Instrs)-1].(type) {
		case *ssa.Panic:
			bad[b] = fails&failPanics != 0
		case *ssa.Return:
			if fails&failErrors != 0 && errIdx >= 0 && c.definitelyNonNil(last.Results[errIdx], b, map[ssa.Value]bool{}) {
				bad[b] = true
			}
		}
		if fails&failOnce != failOnce || inLoop(b) {
			continue
		}
		for _, in := range b.Instrs {
			if call, ok := in.(*ssa.Call); ok && types.Identical(call.Type(), errorType) && c.newError(call) && c.failureArm(b) {
				bad[b] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, b := range fn.Blocks {
			if bad[b] || len(b.Succs) == 0 {
				continue
			}
			all := true
			for _, s := range b.Succs {
				all = all && bad[s]
			}
			if all {
				bad[b], changed = true, true
			}
		}
	}
	for _, b := range lazyInit(fn) {
		bad[b] = true
	}
	ok := map[*ssa.BasicBlock]bool{}
	for _, b := range fn.Blocks {
		if !bad[b] {
			ok[b] = true
		}
	}
	return ok
}

var errorType = types.Universe.Lookup("error").Type()

// testHookAllowed sees the blocks allowedBlocks allows in each module
// function reached. Only tests set it.
var testHookAllowed func(c *checker, fn *ssa.Function, ok map[*ssa.BasicBlock]bool)

// failureArm reports whether b runs on one arm of a branch: the nearest
// branch above it, past any it runs on both arms of, can reach a normal
// return without running b. A block that makes an error on every normal
// path, or on every turn of a loop, is not a failure's: one that runs again
// without passing the branch, or whose branch is the loop's exit test.
func (c *checker) failureArm(b *ssa.BasicBlock) bool {
	for p := b.Idom(); p != nil; p = p.Idom() {
		if _, ok := p.Instrs[len(p.Instrs)-1].(*ssa.If); !ok {
			continue
		}
		if reaches(b.Succs, b, p) {
			return false
		}
		if reaches(b.Succs, p, nil) && slices.ContainsFunc(p.Succs, func(s *ssa.BasicBlock) bool {
			return !s.Dominates(b) && !reaches([]*ssa.BasicBlock{s}, p, nil)
		}) {
			return false
		}
		if c.returnsAround(p, b) {
			return true
		}
	}
	return false
}

// reaches reports whether a path from one of from reaches to without
// running avoid.
func reaches(from []*ssa.BasicBlock, to, avoid *ssa.BasicBlock) bool {
	seen := map[*ssa.BasicBlock]bool{avoid: true}
	stack := slices.Clone(from)
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if x == to {
			return true
		}
		if !seen[x] {
			seen[x] = true
			stack = append(stack, x.Succs...)
		}
	}
	return false
}

// returnsAround reports whether a path from p reaches a return of no error,
// or of an error that may be nil, without running b.
func (c *checker) returnsAround(p, b *ssa.BasicBlock) bool {
	errIdx := errorResult(p.Parent())
	seen := map[*ssa.BasicBlock]bool{p: true, b: true}
	stack := slices.Clone(p.Succs)
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[x] {
			continue
		}
		seen[x] = true
		if r, ok := x.Instrs[len(x.Instrs)-1].(*ssa.Return); ok &&
			(errIdx < 0 || !c.definitelyNonNil(r.Results[errIdx], x, map[ssa.Value]bool{})) {
			return true
		}
		stack = append(stack, x.Succs...)
	}
	return false
}

// errorResult is the index of fn's last result if it is an error, else -1.
func errorResult(fn *ssa.Function) int {
	r := fn.Signature.Results()
	if r.Len() > 0 && types.Identical(r.At(r.Len()-1).Type(), errorType) {
		return r.Len() - 1
	}
	return -1
}

// definitelyNonNil reports whether v, returned at the end of b, cannot be
// nil: a boxed value, a new error, a package-level sentinel, the result of a
// function that only returns non-nil errors, or a value b runs only when it
// was found non-nil.
func (c *checker) definitelyNonNil(v ssa.Value, b *ssa.BasicBlock, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return true
	}
	seen[v] = true
	switch v := v.(type) {
	case *ssa.MakeInterface:
		return true
	case *ssa.ChangeInterface:
		return c.definitelyNonNil(v.X, b, seen)
	case *ssa.UnOp:
		if v.Op != token.MUL {
			break
		}
		switch x := v.X.(type) {
		case *ssa.Global:
			if types.Identical(v.Type(), errorType) {
				return true
			}
		case *ssa.Alloc:
			// A result slot a deferring function returns through: the value
			// last stored to it in this block.
			instrs := v.Block().Instrs
			for i := slices.Index(instrs, ssa.Instruction(v)) - 1; i >= 0; i-- {
				if st, ok := instrs[i].(*ssa.Store); ok && st.Addr == x {
					return c.definitelyNonNil(st.Val, b, seen)
				}
			}
		}
	case *ssa.Call:
		if c.newError(v) {
			return true
		}
	case *ssa.Phi:
		all := true
		for i, e := range v.Edges {
			all = all && c.definitelyNonNil(e, v.Block().Preds[i], seen)
		}
		if all {
			return true
		}
	}
	return checkedNonNil(v, b)
}

// newError reports whether call makes a new error: fmt.Errorf, errors.New,
// or a module function that only returns non-nil errors.
func (c *checker) newError(call *ssa.Call) bool {
	callee := call.Call.StaticCallee()
	if callee == nil {
		return false
	}
	switch callee.String() {
	case "fmt.Errorf", "errors.New":
		return true
	}
	return c.ours(callee) && c.returnsNonNil(callee)
}

// returnsNonNil reports whether every return of fn, whose only or last
// result is an error, returns a non-nil one.
func (c *checker) returnsNonNil(fn *ssa.Function) bool {
	// A function that reaches itself again is taken, there, to return nil.
	return c.nonNil.get(fn, false, func() bool {
		idx := errorResult(fn)
		if idx < 0 || fn.Blocks == nil {
			return false
		}
		for _, b := range fn.Blocks {
			if r, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && !c.definitelyNonNil(r.Results[idx], b, map[ssa.Value]bool{}) {
				return false
			}
		}
		return true
	})
}

// memo caches a recursive question whose recursion can come back to one still
// being answered. A question met again while open gets the assumed answer, and
// nothing answered under that assumption is cached, so every answer is the
// same whatever order the questions are asked in.
type memo[K comparable] struct {
	done    map[K]bool
	open    map[K]bool
	assumed bool
}

func (m *memo[K]) get(k K, assume bool, answer func() bool) bool {
	if m.done == nil {
		m.done, m.open = map[K]bool{}, map[K]bool{}
	}
	if v, ok := m.done[k]; ok {
		return v
	}
	if m.open[k] {
		m.assumed = true
		return assume
	}
	m.open[k] = true
	outer := m.assumed
	m.assumed = false
	v := answer()
	delete(m.open, k)
	if !m.assumed {
		m.done[k] = v
	}
	m.assumed = outer || m.assumed
	return v
}

// checkedNonNil reports whether b runs only on the edge of a branch taken
// when v != nil.
func checkedNonNil(v ssa.Value, b *ssa.BasicBlock) bool {
	for d := b; d != nil; d = d.Idom() {
		if len(d.Preds) != 1 {
			continue
		}
		p := d.Preds[0]
		ifi, ok := p.Instrs[len(p.Instrs)-1].(*ssa.If)
		if !ok {
			continue
		}
		if x, op := nilCompare(ifi.Cond); x == v && (op == token.NEQ) == (p.Succs[0] == d) {
			return true
		}
	}
	return false
}

// nilCompare returns x and the operator when cond is x == nil or x != nil.
func nilCompare(cond ssa.Value) (ssa.Value, token.Token) {
	bin, ok := cond.(*ssa.BinOp)
	if !ok || bin.Op != token.EQL && bin.Op != token.NEQ {
		return nil, 0
	}
	if k, ok := bin.Y.(*ssa.Const); ok && k.IsNil() {
		return bin.X, bin.Op
	}
	if k, ok := bin.X.(*ssa.Const); ok && k.IsNil() {
		return bin.Y, bin.Op
	}
	return nil, 0
}

// lazyInit returns fn's blocks that set up a field or package variable the
// first time it is used: those dominated by the branch taken when it is nil,
// where that branch stores to it, or, for one loaded through sync/atomic,
// stores to it through sync/atomic.
func lazyInit(fn *ssa.Function) []*ssa.BasicBlock {
	var out []*ssa.BasicBlock
	for _, p := range fn.Blocks {
		ifi, ok := p.Instrs[len(p.Instrs)-1].(*ssa.If)
		if !ok {
			continue
		}
		x, op := nilCompare(ifi.Cond)
		var addr ssa.Value
		atomically := false
		switch x := x.(type) {
		case *ssa.UnOp:
			if x.Op == token.MUL {
				addr = x.X
			}
		case *ssa.Call:
			if atomicMethod(x.Common(), "Load") {
				addr, atomically = x.Call.Args[0], true
			}
		}
		switch addr.(type) {
		case *ssa.FieldAddr, *ssa.Global:
		default:
			continue
		}
		s := p.Succs[0]
		if op == token.NEQ {
			s = p.Succs[1]
		}
		if len(s.Preds) != 1 {
			continue
		}
		var region []*ssa.BasicBlock
		stores := false
		for _, b := range fn.Blocks {
			if !s.Dominates(b) {
				continue
			}
			region = append(region, b)
			for _, in := range b.Instrs {
				if st, ok := in.(*ssa.Store); ok && !atomically && sameAddr(st.Addr, addr) {
					stores = true
				}
				if call, ok := in.(ssa.CallInstruction); ok && atomically &&
					(atomicMethod(call.Common(), "Store") || atomicMethod(call.Common(), "CompareAndSwap") || atomicMethod(call.Common(), "Swap")) &&
					sameAddr(call.Common().Args[0], addr) {
					stores = true
				}
			}
		}
		if stores {
			out = append(out, region...)
		}
	}
	return out
}

// atomicMethod reports whether cm calls method of a sync/atomic type on a
// value it is handed the address of, such as (*atomic.Value).Load.
func atomicMethod(cm *ssa.CallCommon, method string) bool {
	callee := cm.StaticCallee()
	if callee == nil || len(cm.Args) == 0 || callee.Name() != method {
		return false
	}
	top := topLevel(callee)
	return top.Pkg != nil && top.Pkg.Pkg.Path() == "sync/atomic" && callee.Signature.Recv() != nil
}

// sameAddr reports whether a and b are the same field or package variable
// reached the same way.
func sameAddr(a, b ssa.Value) bool {
	if a == b {
		return true
	}
	switch a := a.(type) {
	case *ssa.FieldAddr:
		b, ok := b.(*ssa.FieldAddr)
		return ok && a.Field == b.Field && sameAddr(a.X, b.X)
	case *ssa.UnOp:
		b, ok := b.(*ssa.UnOp)
		return ok && a.Op == token.MUL && b.Op == token.MUL && sameAddr(a.X, b.X)
	}
	return false
}

// check adds what fn's allowed blocks allocate.
func (c *checker) check(fn *ssa.Function, add func(*ssa.Function, site)) {
	ok := c.blocks[fn]
	for _, b := range fn.Blocks {
		if !ok[b] {
			continue
		}
		for _, in := range b.Instrs {
			pos := in.Pos()
			if call, isCall := in.(ssa.CallInstruction); isCall && !pos.IsValid() {
				pos = call.Common().Pos()
			}
			if !pos.IsValid() {
				continue
			}
			if i, ok := c.exemptionIndex(pos); ok {
				c.exemptions[i].Reached = true
				for _, r := range c.instrAllocs(in) {
					c.addCover(i, fn, newSite(c.sitePos(in, pos), r))
				}
				continue
			}
			for _, r := range c.instrAllocs(in) {
				at := newSite(c.sitePos(in, pos), r)
				add(fn, at)
				if call, isCall := in.(ssa.CallInstruction); isCall && c.callWhy[call] != "" && strings.HasPrefix(r, "calls ") {
					c.whyAt[at] = c.callWhy[call]
				}
			}
		}
	}
}

// sitePos is where in's allocation is counted, given pos, where it is. The
// entries of a map literal on a line fill one map, so they count at the
// literal's opening brace.
func (c *checker) sitePos(in ssa.Instruction, pos token.Pos) token.Position {
	p := c.fset.Position(pos)
	u, ok := in.(*ssa.MapUpdate)
	if !ok {
		return p
	}
	m, ok := u.Map.(*ssa.MakeMap)
	if !ok || !m.Pos().IsValid() {
		return p
	}
	f, _ := c.syntax(p.Filename)
	if f == nil {
		return p
	}
	path, _ := astutil.PathEnclosingInterval(f, m.Pos(), m.Pos())
	for _, n := range path {
		if lit, ok := n.(*ast.CompositeLit); ok && lit.Lbrace == m.Pos() && pos > lit.Lbrace && pos < lit.Rbrace {
			p.Column = c.fset.Position(lit.Lbrace).Column
			break
		}
	}
	return p
}

// concatOperand reports whether v is only an operand of another string
// concatenation on its line, which the compiler joins with it into one.
func (c *checker) concatOperand(v *ssa.BinOp) bool {
	refs := *v.Referrers()
	if len(refs) != 1 {
		return false
	}
	b, ok := refs[0].(*ssa.BinOp)
	return ok && b.Op == token.ADD && isString(b.Type()) &&
		c.fset.Position(b.Pos()).Line == c.fset.Position(v.Pos()).Line
}

// handGrownAt reports whether msg, an escape the compiler reports on line of
// file, is a make there that grows reused memory by hand, as growsOwnByHand
// has it, which that rule judges as it judges an append.
func (c *checker) handGrownAt(file string, line int, msg string) bool {
	if !strings.HasPrefix(msg, "make(") {
		return false
	}
	for _, fn := range c.funcsAt(file, line) {
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				if m, isMake := in.(*ssa.MakeSlice); isMake && m.Pos().IsValid() && c.fset.Position(m.Pos()).Line == line && c.growsOwnByHand(m) {
					return true
				}
			}
		}
	}
	return false
}

// funcsAt returns the functions whose code is on line of file.
func (c *checker) funcsAt(file string, line int) []*ssa.Function {
	if c.byLine == nil {
		c.byLine = map[string]map[int][]*ssa.Function{}
		for fn := range ssautil.AllFunctions(c.prog) {
			if fn.Syntax() == nil {
				continue
			}
			start, end := c.fset.Position(fn.Syntax().Pos()), c.fset.Position(fn.Syntax().End())
			f := filepath.Clean(start.Filename)
			if c.byLine[f] == nil {
				c.byLine[f] = map[int][]*ssa.Function{}
			}
			for l := start.Line; l <= end.Line; l++ {
				c.byLine[f][l] = append(c.byLine[f][l], fn)
			}
		}
	}
	return c.byLine[file][line]
}

// growsOwnByHand reports whether m, in a function shaped like append, is
// that function growing what it appends to as append does: made as long as
// a slice it holds as reused memory, which it then copies into it.
func (c *checker) growsOwnByHand(m *ssa.MakeSlice) bool {
	fn := m.Parent()
	if !c.appendRoot(fn) {
		return false
	}
	n, isCall := m.Len.(*ssa.Call)
	if !isCall {
		return false
	}
	if b, isBuiltin := n.Call.Value.(*ssa.Builtin); !isBuiltin || b.Name() != "len" || !c.reused(n.Call.Args[0]) {
		return false
	}
	from := n.Call.Args[0]
	for _, r := range *m.Referrers() {
		if cp, isCall := r.(*ssa.Call); isCall {
			if b, isBuiltin := cp.Call.Value.(*ssa.Builtin); isBuiltin && b.Name() == "copy" &&
				cp.Call.Args[0] == ssa.Value(m) && cp.Call.Args[1] == from {
				return true
			}
		}
	}
	return false
}

// appendedMake reports whether m, with no capacity of its own, is written
// as what a builtin append spreads onto a slice, append(s, make([]T, n)...),
// which the compiler turns into growing s by n without making the slice.
func (c *checker) appendedMake(m *ssa.MakeSlice) bool {
	pos := m.Pos()
	if m.Cap != m.Len || !pos.IsValid() {
		return false
	}
	mk, outer := c.callAt(pos)
	app, isCall := outer.(*ast.CallExpr)
	if mk == nil || !isCall || !app.Ellipsis.IsValid() || len(app.Args) != 2 || app.Args[1] != mk {
		return false
	}
	id, isIdent := app.Fun.(*ast.Ident)
	return isIdent && id.Name == "append"
}

// callAt is the call whose open parenthesis is at pos, and the node that
// holds it.
func (c *checker) callAt(pos token.Pos) (*ast.CallExpr, ast.Node) {
	f, _ := c.syntax(c.fset.Position(pos).Filename)
	if f == nil {
		return nil, nil
	}
	path, _ := astutil.PathEnclosingInterval(f, pos, pos)
	for i, n := range path {
		if call, isCall := n.(*ast.CallExpr); isCall && call.Lparen == pos && i+1 < len(path) {
			return call, path[i+1]
		}
	}
	return nil, nil
}

// instrAllocs returns why in allocates, if it does.
func (c *checker) instrAllocs(in ssa.Instruction) []string {
	var out []string
	switch in := in.(type) {
	case *ssa.Go:
		out = append(out, "go statement")
	case *ssa.Defer:
		if inLoop(in.Block()) {
			out = append(out, "defer in a loop")
		}
	case *ssa.MapUpdate:
		if !c.reused(in.Map) {
			out = append(out, "map write")
		} else {
			out = append(out, "map write that may grow reused memory")
		}
	case *ssa.BinOp:
		if in.Op == token.ADD && isString(in.Type()) && !c.concatOperand(in) {
			out = append(out, "string concatenation")
		}
	case *ssa.Convert:
		if conv := stringConversion(in); conv != "" && !compilerAvoidsCopy(in) && !c.rangedOver(in) {
			out = append(out, conv)
		}
	case *ssa.MakeSlice:
		if (!isConst(in.Len) || !isConst(in.Cap)) && !c.appendedMake(in) && !c.growsOwnByHand(in) {
			out = append(out, "make with a size known only at run time")
		}
	case *ssa.MakeMap:
		if in.Reserve != nil && !isConst(in.Reserve) {
			out = append(out, "make with a size known only at run time")
		}
	case *ssa.MakeChan:
		out = append(out, "make chan")
	case *ssa.Store:
		if c.foreignStore(in) {
			out = append(out, "stores memory the Scratch does not own into it")
		}
	}
	if call, ok := in.(ssa.CallInstruction); ok {
		out = append(out, c.callAllocs(call)...)
	}
	return out
}

func (c *checker) callAllocs(call ssa.CallInstruction) []string {
	cm := call.Common()
	if b, ok := cm.Value.(*ssa.Builtin); ok {
		if b.Name() != "append" || len(cm.Args) < 2 {
			return nil
		}
		if in, ok := call.(*ssa.Call); ok && fits(in) {
			return nil
		}
		if !c.reused(cm.Args[0]) {
			return []string{"append to a slice outside the Scratch"}
		}
		if in, ok := call.(*ssa.Call); ok && !c.ownAppend(in) {
			return []string{"append that may grow reused memory, with no check that it has room"}
		}
		return nil
	}
	callees := c.expand(c.sites[call], map[*ssa.Function]bool{})
	if len(callees) == 0 {
		desc := callDesc(cm)
		if s, ok := c.methodSummary(cm); ok {
			// Every implementation is outside the module, and judged by
			// its code, with no failure of theirs taken to end the call.
			if !s.Alloc[0] || c.overridden(desc) {
				return nil
			}
			c.noteWhy(call, s.Why[0])
			return []string{"calls " + desc}
		}
		if c.overridden(desc) {
			return nil
		}
		return []string{"call with no known callee: " + desc}
	}
	var out []string
	for _, callee := range callees {
		if c.appendRoot(callee) {
			if dst := appendedTo(cm, callee); dst == nil || !c.reused(dst) {
				out = append(out, "calls "+funcName(callee))
			} else if !c.ownAppendCall(call, dst) {
				out = append(out, "calls "+funcName(callee)+", which may grow reused memory")
			}
			continue
		}
		// The module's Go functions are checked where they are defined; one
		// with no Go body is judged by its machine code, as code outside is.
		if c.ours(callee) && callee.Blocks != nil {
			continue
		}
		out = append(out, c.summaryAllocs(call, callee, funcName(callee))...)
	}
	return out
}

// methodSummary is the summary of cm, a call of an interface method, as of
// every implementation outside the module.
func (c *checker) methodSummary(cm *ssa.CallCommon) (summary, bool) {
	if !cm.IsInvoke() {
		return summary{}, false
	}
	s, ok := c.sums[methodSummaryName(cm)]
	return s, ok
}

// overridden reports whether an allowlist line covers the function or call
// named name, which would allocate, and notes that the check relied on each
// line that does.
func (c *checker) overridden(name string) bool {
	labels := allowLabels(c.allow.assumed, name)
	for _, l := range labels {
		c.allow.used[l] = true
	}
	return len(labels) > 0
}

// allowLabels are the lines of assumed that cover name, in order: name
// itself, and each line ending in "*" whose text before it starts name.
func allowLabels(assumed map[string]string, name string) []string {
	var out []string
	for label := range assumed {
		p, pattern := strings.CutSuffix(label, "*")
		if label == name || pattern && strings.HasPrefix(name, p) {
			out = append(out, label)
		}
	}
	slices.Sort(out)
	return out
}

// noteWhy records why call, judged by a summary, allocates.
func (c *checker) noteWhy(call ssa.CallInstruction, why string) {
	if c.callWhy == nil {
		c.callWhy, c.whyAt = map[ssa.CallInstruction]string{}, map[site]string{}
	}
	c.callWhy[call] = why
}

// summaryAllocs is why call, to callee outside the module, allocates, as
// callee's summary has it for the failures the call gives it.
func (c *checker) summaryAllocs(call ssa.CallInstruction, callee *ssa.Function, name string) []string {
	s, ok := c.sums[summaryName(callee)]
	if !ok {
		return []string{"calls " + name}
	}
	fn := call.Parent()
	f := c.calleeFailures(fn, call, callee, c.blocks[fn])
	if cl, isCall := call.(*ssa.Call); isCall && c.newError(cl) && !c.failureArm(cl.Block()) {
		// An error made on a normal path is no failure's.
		f &^= failErrors
	}
	alloc, why := s.Alloc[f], s.Why[f]
	if pos := call.Common().Pos(); alloc && c.inlinedCalls[pos] && !s.Bare[f] && !c.inlinedEscapes[pos] {
		// Inlined, the callee's own escapes are what the compiler reports
		// at the call, and it reports none.
		alloc = false
	}
	if !alloc {
		return nil
	}
	if c.ours(callee) && c.overridden(name) {
		// The summarizer applies the allowlist to code outside the
		// module; the module's code with no Go body is judged here.
		return nil
	}
	c.noteWhy(call, why)
	if s.Shaped && s.Appends[f] {
		if dst := appendedTo(call.Common(), callee); dst == nil || !c.reused(dst) {
			return []string{"calls " + name}
		} else if !c.ownAppendCall(call, dst) {
			return []string{"calls " + name + ", which may grow reused memory"}
		}
		return nil
	}
	return []string{"calls " + name}
}

func callDesc(cm *ssa.CallCommon) string {
	if cm.IsInvoke() {
		return cm.Value.Type().String() + "." + cm.Method.Name()
	}
	return funcValue(cm.Value)
}

// funcValue names a func value by where it comes from, never by its SSA
// register, which code added above it renumbers.
func funcValue(v ssa.Value) string {
	switch v := v.(type) {
	case *ssa.UnOp:
		if v.Op == token.MUL {
			if f, ok := v.X.(*ssa.FieldAddr); ok {
				return funcValue(f)
			}
			return "*" + funcValue(v.X)
		}
	case *ssa.FieldAddr:
		if p, ok := v.X.Type().Underlying().(*types.Pointer); ok {
			if st, ok := p.Elem().Underlying().(*types.Struct); ok {
				return "field " + st.Field(v.Field).Name()
			}
		}
	case *ssa.Field:
		if st, ok := v.X.Type().Underlying().(*types.Struct); ok {
			return "field " + st.Field(v.Field).Name()
		}
	case *ssa.Alloc:
		if v.Comment != "" {
			return v.Comment
		}
	case *ssa.Call:
		if f := v.Call.StaticCallee(); f != nil {
			return "result of " + funcName(f)
		}
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok {
			if f := call.Call.StaticCallee(); f != nil {
				return fmt.Sprintf("result %d of %s", v.Index, funcName(f))
			}
		}
	case *ssa.Global, *ssa.Parameter, *ssa.FreeVar:
		return v.Name()
	case *ssa.Function, *ssa.Builtin:
		return v.String()
	}
	return "a func value"
}

// expand replaces synthetic wrappers among callees with what they call.
func (c *checker) expand(callees []*ssa.Function, seen map[*ssa.Function]bool) []*ssa.Function {
	var out []*ssa.Function
	for _, f := range callees {
		if seen[f] {
			continue
		}
		seen[f] = true
		if !wrapper(f) || f.Blocks == nil {
			out = append(out, f)
			continue
		}
		var next []*ssa.Function
		if n := c.cg.Nodes[f]; n != nil {
			for _, e := range n.Out {
				next = append(next, e.Callee.Func)
			}
		}
		out = append(out, c.expand(next, seen)...)
	}
	return out
}

func inLoop(b *ssa.BasicBlock) bool {
	seen := map[*ssa.BasicBlock]bool{}
	stack := slices.Clone(b.Succs)
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if x == b {
			return true
		}
		if !seen[x] {
			seen[x] = true
			stack = append(stack, x.Succs...)
		}
	}
	return false
}

func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

func isConst(v ssa.Value) bool {
	_, ok := v.(*ssa.Const)
	return ok
}

// stringConversion names in when it copies between a string and a byte or
// rune slice.
func stringConversion(in *ssa.Convert) string {
	from, to := in.X.Type(), in.Type()
	_, fromSlice := from.Underlying().(*types.Slice)
	_, toSlice := to.Underlying().(*types.Slice)
	switch {
	case isString(to) && fromSlice:
		return "string([]byte) conversion"
	case toSlice && isString(from):
		return "[]byte(string) conversion"
	}
	return ""
}

// compilerAvoidsCopy reports whether every use of a string([]byte)
// conversion is one the compiler makes without copying: a map key looked up
// or a comparison.
func compilerAvoidsCopy(in *ssa.Convert) bool {
	if !isString(in.Type()) {
		return false
	}
	refs := in.Referrers()
	if refs == nil || len(*refs) == 0 {
		return false
	}
	for _, r := range *refs {
		switch r := r.(type) {
		case *ssa.Lookup:
			if r.Index != in {
				return false
			}
		case *ssa.BinOp:
			switch r.Op {
			case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}

// rangedOver reports whether in converts a string to a byte slice as the
// expression a for statement ranges over, which the compiler reads in place,
// the string's bytes being immutable. Bytes converted to a string to range
// over are copied, since the loop can change them.
func (c *checker) rangedOver(in *ssa.Convert) bool {
	pos := in.Pos()
	if !pos.IsValid() || !isString(in.X.Type()) {
		return false
	}
	call, outer := c.callAt(pos)
	r, isRange := outer.(*ast.RangeStmt)
	return call != nil && isRange && r.X == call
}

// scratchTypes returns the Scratch types and every struct type they hold by
// value, or as the elements of a slice they hold, directly or by pointer, at
// any depth.
func (c *checker) scratchTypes() map[types.Type]bool {
	out := map[types.Type]bool{}
	c.scratchRoots = map[types.Type]bool{}
	var add func(t types.Type)
	add = func(t types.Type) {
		switch u := t.Underlying().(type) {
		case *types.Struct:
			if out[t] {
				return
			}
			out[t] = true
			for f := range u.Fields() {
				add(f.Type())
			}
		case *types.Array:
			add(u.Elem())
		case *types.Slice:
			elem := u.Elem()
			if p, ok := elem.Underlying().(*types.Pointer); ok {
				elem = p.Elem()
			}
			if _, ok := elem.Underlying().(*types.Struct); ok {
				add(elem)
			}
		}
	}
	for _, name := range c.cfg.Scratch {
		i := strings.LastIndex(name, ".")
		if pkg := c.prog.ImportedPackage(name[:i]); pkg != nil {
			if obj := pkg.Pkg.Scope().Lookup(name[i+1:]); obj != nil {
				add(obj.Type())
				c.scratchRoots[obj.Type()] = true
			}
		}
	}
	return out
}

// reused reports whether v, a slice or map, is held in the Scratch: loaded
// from a field of a Scratch type, resliced or appended from one, or passed
// in from one by every caller reach followed.
func (c *checker) reused(v ssa.Value) bool {
	// A cycle through phis or calls adds no other source.
	return c.fromScr.get(v, true, func() bool { return c.reusedUncached(v) })
}

func (c *checker) reusedUncached(v ssa.Value) bool {
	switch v := v.(type) {
	case *ssa.ChangeType:
		// The same memory under another name, as a []string is a stringSet.
		return c.reused(v.X)
	case *ssa.UnOp:
		return v.Op == token.MUL && c.scratchAddr(v.X)
	case *ssa.Slice:
		if _, ok := v.X.Type().Underlying().(*types.Pointer); ok {
			return c.scratchAddr(v.X)
		}
		return c.reused(v.X)
	case *ssa.Phi:
		for _, e := range v.Edges {
			if !c.reused(e) {
				return false
			}
		}
		return true
	case *ssa.Call:
		if b, ok := v.Call.Value.(*ssa.Builtin); ok {
			return b.Name() == "append" && c.reused(v.Call.Args[0])
		}
		callee := v.Call.StaticCallee()
		if callee != nil && (c.appendRoot(callee) || c.returnsAppended(callee)) {
			dst := appendedTo(&v.Call, callee)
			return dst != nil && c.reused(dst)
		}
		if callee == nil || !c.ours(callee) || callee.Blocks == nil {
			return false
		}
		return c.returnsReused(callee, 0)
	case *ssa.Extract:
		call, ok := v.Tuple.(*ssa.Call)
		if !ok {
			return false
		}
		callee := call.Call.StaticCallee()
		if callee != nil && c.appendRoot(callee) && v.Index == 0 {
			dst := appendedTo(call.Common(), callee)
			return dst != nil && c.reused(dst)
		}
		return callee != nil && c.ours(callee) && callee.Blocks != nil && c.returnsReused(callee, v.Index)
	case *ssa.Parameter:
		return c.paramReused(v)
	case *ssa.MakeSlice:
		// Reused memory grown by hand is reused memory still.
		return c.growsOwnByHand(v)
	}
	return false
}

// returnsAppended reports whether callee, outside the module and shaped
// like append, returns its first argument appended to, as its summary has
// it: every return is, whatever else it allocates, or, handed reused memory,
// it allocates nothing but to grow it.
func (c *checker) returnsAppended(callee *ssa.Function) bool {
	if c.appended != nil {
		return c.appended(callee)
	}
	s, ok := c.sums[summaryName(callee)]
	return ok && s.Shaped && (s.Returns || s.Appends[failOnce])
}

func (c *checker) returnsReused(fn *ssa.Function, idx int) bool {
	for _, b := range fn.Blocks {
		if r, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && idx < len(r.Results) && !c.reused(r.Results[idx]) {
			return false
		}
	}
	return true
}

func (c *checker) paramReused(p *ssa.Parameter) bool {
	fn := p.Parent()
	idx := slices.Index(fn.Params, p)
	if c.appendRoot(fn) && idx == len(fn.Params)-fn.Signature.Params().Len() {
		return true
	}
	sites := c.callers[fn]
	if idx < 0 || len(sites) == 0 {
		return false
	}
	for _, s := range sites {
		cm := s.Common()
		i := idx
		if cm.IsInvoke() {
			if i == 0 {
				return false
			}
			i--
		}
		if i >= len(cm.Args) || !c.reused(cm.Args[i]) {
			return false
		}
	}
	return true
}

// scratchPtr reports whether p, a pointer to a struct of a type Scratch
// holds, points into Scratch memory: not at a fresh variable of that type,
// nor at one every caller does not hand in from Scratch.
func (c *checker) scratchPtr(p ssa.Value) bool {
	return c.fromPtr.get(p, true, func() bool {
		switch p := p.(type) {
		case *ssa.Alloc:
			// A Scratch type's own memory, made once and reused, is the
			// Scratch; a fresh value of a type it holds is not.
			if ptr, ok := p.Type().Underlying().(*types.Pointer); ok && c.scratchRoots[ptr.Elem()] {
				return true
			}
			return c.scratchAddr(p)
		case *ssa.Parameter:
			// A function nothing reached calls, as a root is, is handed its
			// memory from outside, which its type vouches for.
			fn := p.Parent()
			idx := slices.Index(fn.Params, p)
			sites := c.callers[fn]
			if idx < 0 || len(sites) == 0 {
				return true
			}
			for _, s := range sites {
				cm := s.Common()
				i := idx
				if cm.IsInvoke() {
					if i == 0 {
						continue
					}
					i--
				}
				if i < len(cm.Args) && !c.scratchPtr(cm.Args[i]) {
					return false
				}
			}
			return true
		case *ssa.Phi:
			for _, e := range p.Edges {
				if !c.scratchPtr(e) {
					return false
				}
			}
			return true
		}
		return true
	})
}

// grounded reports whether one of the values stored into the variable a is
// Scratch memory without a itself: a variable starts zero, so one that only
// ever holds what it was built from, as a buffer handed to an Into function
// and stored back, holds fresh memory on every call.
func (c *checker) grounded(a *ssa.Alloc, stores []ssa.Value) bool {
	if v, ok := c.groundedAt[a]; ok {
		return v
	}
	if c.denied == nil {
		c.denied, c.groundedAt = map[*ssa.Alloc]bool{}, map[*ssa.Alloc]bool{}
	}
	outer, outerPtr := c.fromScr, c.fromPtr
	c.fromScr, c.fromPtr = memo[ssa.Value]{}, memo[ssa.Value]{}
	c.denied[a] = true
	v := false
	for _, s := range stores {
		if c.reused(s) {
			v = true
			break
		}
	}
	delete(c.denied, a)
	c.fromScr, c.fromPtr = outer, outerPtr
	c.groundedAt[a] = v
	return v
}

// scratchAddr reports whether addr is a field of a Scratch type, an element
// of an array held in one, or a variable that only ever holds Scratch memory,
// including one a closure shares with the function that made it.
func (c *checker) scratchAddr(addr ssa.Value) bool {
	switch a := addr.(type) {
	case *ssa.Alloc:
		if c.denied[a] {
			return false
		}
		refs := a.Referrers()
		if refs == nil {
			return false
		}
		var stores []ssa.Value
		for _, r := range *refs {
			if st, ok := r.(*ssa.Store); ok && st.Addr == a {
				if !c.reused(st.Val) {
					return false
				}
				stores = append(stores, st.Val)
			}
		}
		// A fixed-size array is memory of its own size, which escape analysis
		// accounts for; a zero slice or struct has none until it grows.
		if _, ok := a.Type().Underlying().(*types.Pointer).Elem().Underlying().(*types.Array); ok {
			return true
		}
		return c.grounded(a, stores)
	case *ssa.FreeVar:
		fn := a.Parent()
		i := slices.Index(fn.FreeVars, a)
		if fn.Parent() == nil || i < 0 {
			return false
		}
		for _, b := range fn.Parent().Blocks {
			for _, in := range b.Instrs {
				if mc, ok := in.(*ssa.MakeClosure); ok && mc.Fn == fn {
					return c.scratchAddr(mc.Bindings[i])
				}
			}
		}
	case *ssa.FieldAddr:
		ptr, ok := a.X.Type().Underlying().(*types.Pointer)
		return ok && (c.isFixed(ptr.Elem()) || c.scratch[ptr.Elem()] && c.scratchPtr(a.X))
	case *ssa.IndexAddr:
		if _, ok := a.X.Type().Underlying().(*types.Pointer); ok {
			return c.scratchAddr(a.X)
		}
		// An element of a slice the Scratch holds, as a chunk of an arena is.
		if _, ok := a.X.Type().Underlying().(*types.Slice); ok {
			return c.reused(a.X)
		}
	}
	return false
}

type span struct {
	pos, end token.Pos
	// fns is the reached functions with this syntax, in the order reached:
	// a function, its instances, and the wrappers that call them.
	fns []*ssa.Function
	lit bool
}

// matchEscapes adds each heap escape the compiler reported inside a reached
// function of the module, on a line its normal path runs.
func (c *checker) matchEscapes(escapes, notes []escape, add func(*ssa.Function, site)) {
	byFile := map[string][]span{}
	reached := map[ast.Node][]*ssa.Function{}
	for _, fn := range append(slices.Clone(c.order), c.coverOrder...) {
		if c.ours(fn) && fn.Syntax() != nil {
			reached[fn.Syntax()] = append(reached[fn.Syntax()], fn)
		}
	}
	for fn := range ssautil.AllFunctions(c.prog) {
		if !c.ours(fn) || fn.Syntax() == nil {
			continue
		}
		s := fn.Syntax()
		file := c.fset.Position(s.Pos()).Filename
		_, lit := s.(*ast.FuncLit)
		byFile[file] = append(byFile[file], span{s.Pos(), s.End(), reached[s], lit})
	}
	lineBlocks := map[*ssa.Function]map[int][]*ssa.BasicBlock{}
	var callees map[token.Pos]*ssa.Function
	// noted holds where the compiler reported anything of its escape
	// analysis, by file, to tell a closure compiled on its own from one only
	// ever inlined.
	noted := map[string][]token.Pos{}
	for _, e := range notes {
		if spans := byFile[e.file]; len(spans) > 0 {
			if tf := c.fset.File(spans[0].pos); e.line <= tf.LineCount() {
				noted[e.file] = append(noted[e.file], tf.LineStart(e.line)+token.Pos(e.col-1))
			}
		}
	}
	for _, e := range escapes {
		spans := byFile[e.file]
		if len(spans) == 0 {
			continue
		}
		tf := c.fset.File(spans[0].pos)
		if e.line > tf.LineCount() {
			continue
		}
		pos := tf.LineStart(e.line) + token.Pos(e.col-1)
		if !e.inlined && (constantText(e.msg) && c.constantAt(e.file, pos) || c.boxedFreeAt(e.file, pos, e.msg) || c.handGrownAt(e.file, e.line, e.msg)) {
			continue
		}
		if e.inlined {
			if callees == nil {
				callees = c.staticCallees()
			}
			msg, ok := c.inlinedEscape(callees[pos], e, noted)
			if !ok {
				continue
			}
			e.msg = msg
		}
		var in *span
		for i := range spans {
			s := &spans[i]
			if pos < s.pos || pos >= s.end {
				continue
			}
			if s.lit && pos == s.pos {
				continue // a closure's own allocation is its parent's
			}
			if in == nil || s.pos >= in.pos && s.end <= in.end {
				in = s
			}
		}
		if in == nil || len(in.fns) == 0 {
			continue
		}
		if i, ok := c.exemptionAt[e.file][e.line]; ok && c.exempt[e.file][e.line] {
			c.exemptions[i].Reached = true
			c.addCover(i, in.fns[0], e.site())
			continue
		}
		// The escape is on the normal path when a function that runs its line
		// runs it there. A wrapper sharing the syntax runs none of the body's
		// lines, and judges nothing.
		var fn *ssa.Function
		runs := false
		for _, f := range in.fns {
			lb := lineBlocks[f]
			if lb == nil {
				lb = map[int][]*ssa.BasicBlock{}
				for _, b := range f.Blocks {
					for _, ins := range b.Instrs {
						if p := ins.Pos(); p.IsValid() {
							l := c.fset.Position(p).Line
							lb[l] = append(lb[l], b)
						}
					}
				}
				lineBlocks[f] = lb
			}
			bs := lb[e.line]
			if len(bs) == 0 {
				continue
			}
			runs = true
			if slices.ContainsFunc(bs, func(b *ssa.BasicBlock) bool { return c.blocks[f][b] }) {
				fn = f
				break
			}
		}
		if runs && fn == nil {
			continue
		}
		if fn == nil {
			fn = in.fns[0]
		}
		add(fn, e.site())
	}
}

// constantText reports whether msg, an escape the compiler reports, names
// an expression in the form it prints a constant in: a literal, perhaps
// signed and cut short, true or false, or one converted to a named type.
func constantText(msg string) bool {
	text, ok := strings.CutSuffix(msg, " escapes to heap")
	if !ok {
		return false
	}
	if strings.HasPrefix(text, `"`) && strings.HasSuffix(text, "...") {
		return true
	}
	e, err := parser.ParseExpr(text)
	return err == nil && constantForm(e)
}

func constantForm(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return e.Name == "true" || e.Name == "false"
	case *ast.UnaryExpr:
		return (e.Op == token.SUB || e.Op == token.ADD) && constantForm(e.X)
	case *ast.CallExpr:
		switch fun := e.Fun.(type) {
		case *ast.Ident:
		case *ast.SelectorExpr:
			if _, isIdent := fun.X.(*ast.Ident); !isIdent {
				return false
			}
		default:
			return false
		}
		return len(e.Args) == 1 && constantForm(e.Args[0])
	}
	return false
}

// posOf is the position of e, a place the compiler reports, in a file of
// the program.
func (c *checker) posOf(e escape) (token.Pos, bool) {
	f, _ := c.syntax(e.file)
	if f == nil {
		return token.NoPos, false
	}
	tf := c.fset.File(f.Pos())
	if e.line > tf.LineCount() {
		return token.NoPos, false
	}
	return tf.LineStart(e.line) + token.Pos(e.col-1), true
}

// inlinedEscapes are the escapes of es the compiler reports at a call it
// inlined, other than a constant's, which is read-only data.
func inlinedEscapes(es []escape) []escape {
	var out []escape
	for _, e := range es {
		if e.inlined && !constantText(e.msg) {
			out = append(out, e)
		}
	}
	return out
}

// positions is the set of the places of es.
func (c *checker) positions(es []escape) map[token.Pos]bool {
	out := map[token.Pos]bool{}
	for _, e := range es {
		if pos, ok := c.posOf(e); ok {
			out[pos] = true
		}
	}
	return out
}

// syntax is the syntax and type information of the file at path.
func (c *checker) syntax(path string) (*ast.File, *types.Info) {
	path = filepath.Clean(path)
	return c.files[path], c.fileInfo[path]
}

// constantAt reports whether the expression the compiler reports an escape
// of at pos in file is a constant, which the compiler converts to an
// interface as read-only data: the escape allocates nothing.
func (c *checker) constantAt(file string, pos token.Pos) bool {
	f, info := c.syntax(file)
	if f == nil || info == nil {
		return false
	}
	// The compiler reports a conversion at its opening parenthesis and an
	// operation at its operator: the innermost expression around pos, or one
	// that starts there.
	path, _ := astutil.PathEnclosingInterval(f, pos, pos)
	for i, n := range path {
		e, isExpr := n.(ast.Expr)
		if !isExpr || i > 0 && e.Pos() != pos {
			break
		}
		if tv, ok := info.Types[e]; ok && tv.Value != nil {
			return true
		}
	}
	return false
}

// boxedFreeAt reports whether what the compiler reports with msg as escaping
// at pos in file is a value an interface can point at the runtime's own memory
// for, so boxing it allocates nothing: a value of a type with no size, or a
// one-byte integer or a bool, which the runtime keeps a table of.
func (c *checker) boxedFreeAt(file string, pos token.Pos, msg string) bool {
	if !strings.HasSuffix(msg, " escapes to heap") {
		return false
	}
	f, info := c.syntax(file)
	if f == nil || info == nil {
		return false
	}
	path, _ := astutil.PathEnclosingInterval(f, pos, pos)
	for i, n := range path {
		e, isExpr := n.(ast.Expr)
		if !isExpr || i > 0 && e.Pos() != pos {
			break
		}
		t := info.TypeOf(e)
		if t == nil || hasTypeParam(t, map[types.Type]bool{}) {
			continue
		}
		// The compiler can report an escape at an operand, as it reports
		// string(c) at c, so only the expression the message names counts.
		named := types.ExprString(e)+" escapes to heap" == msg
		if b, ok := t.Underlying().(*types.Basic); ok && named && (b.Info()&types.IsBoolean != 0 || b.Info()&types.IsInteger != 0 && zeroSize.Sizeof(b) == 1) {
			return true
		}
		if zeroSize.Sizeof(t) == 0 {
			return true
		}
	}
	return false
}

// hasTypeParam reports whether t holds a type parameter, whose size is
// known only once it is instantiated.
func hasTypeParam(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t := types.Unalias(t).(type) {
	case *types.TypeParam:
		return true
	case *types.Named:
		for i := range t.TypeArgs().Len() {
			if hasTypeParam(t.TypeArgs().At(i), seen) {
				return true
			}
		}
		return t.TypeParams().Len() > 0 && t.TypeArgs().Len() == 0 || hasTypeParam(t.Underlying(), seen)
	case *types.Array:
		return hasTypeParam(t.Elem(), seen)
	case *types.Struct:
		for i := range t.NumFields() {
			if hasTypeParam(t.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// zeroSize measures types for boxedFreeAt: a type has no size on one
// platform when it has none on every one.
var zeroSize = types.SizesFor("gc", "amd64")

// staticCallees maps the position of each call in the module, its opening
// parenthesis as the compiler reports an inlined call at, to its callee.
func (c *checker) staticCallees() map[token.Pos]*ssa.Function {
	out := map[token.Pos]*ssa.Function{}
	for fn := range ssautil.AllFunctions(c.prog) {
		if !c.ours(fn) {
			continue
		}
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				if call, ok := in.(ssa.CallInstruction); ok {
					if callee := call.Common().StaticCallee(); callee != nil {
						out[call.Common().Pos()] = callee
					}
				}
			}
		}
	}
	return out
}

// inlinedEscape reports whether msg, an escape at a call to callee the
// compiler inlined, allocates in the caller, and if so how to name it. The
// caller's boxing of a result does. So does the inlined body's escape when
// the callee is a closure the compiler never compiled on its own, which it
// shows by reporting nothing inside it: no other report of that escape
// exists. Any other is the body's, which its own compile reports where it is
// written, since inlining only ever removes escapes.
func (c *checker) inlinedEscape(callee *ssa.Function, e escape, noted map[string][]token.Pos) (string, bool) {
	msg := e.msg
	if r, ok := inlinedResult(callee, msg); ok {
		return r, true
	}
	if callee == nil || callee.Parent() == nil || !c.ours(callee) {
		return "", false
	}
	syn := callee.Syntax()
	if syn == nil {
		return "", false
	}
	for _, p := range noted[c.fset.Position(syn.Pos()).Filename] {
		// The closure's own position holds what its parent's compile says of it.
		if p > syn.Pos() && p < syn.End() {
			return "", false
		}
	}
	// An escape at a closure that does nothing that allocates is from what it
	// calls, inlined in turn, such as another package's panic message.
	if !c.allocates(callee) {
		return "", false
	}
	// A string constant the closure did not write or get passed is one from
	// code it inlined.
	if lit, ok := strings.CutSuffix(msg, " escapes to heap"); ok && isStringLiteral(lit) {
		start, end := c.fset.Position(syn.Pos()), c.fset.Position(syn.End())
		src := c.source(start.Filename)
		var text strings.Builder
		for l := start.Line; l <= end.Line && l-1 < len(src); l++ {
			text.WriteString(src[l-1])
		}
		if src := c.source(e.file); e.line-1 < len(src) {
			text.WriteString(src[e.line-1])
		}
		if !strings.Contains(text.String(), lit) {
			return "", false
		}
	}
	c.inlinedBodies[callee] = true
	return msg + " in the inlined " + funcName(callee), true
}

func isStringLiteral(s string) bool {
	_, err := strconv.Unquote(s)
	return err == nil
}

// allocates reports whether fn's normal path holds an instruction escape
// analysis can move to the heap, rather than only calls.
func (c *checker) allocates(fn *ssa.Function) bool {
	ok := c.blocks[fn]
	for _, b := range fn.Blocks {
		if ok != nil && !ok[b] {
			continue
		}
		for _, in := range b.Instrs {
			switch in := in.(type) {
			case *ssa.MakeInterface, *ssa.MakeMap, *ssa.MakeSlice, *ssa.MakeChan, *ssa.MakeClosure:
				return true
			case *ssa.Alloc:
				if in.Heap {
					return true
				}
			case *ssa.BinOp:
				if in.Op == token.ADD && isString(in.Type()) {
					return true
				}
			case *ssa.Convert:
				if stringConversion(in) != "" {
					return true
				}
			case *ssa.Call:
				if b, isBuiltin := in.Call.Value.(*ssa.Builtin); isBuiltin && b.Name() == "append" {
					return true
				}
			}
		}
	}
	return false
}

var unnamedResult = regexp.MustCompile(`^~r\d+ escapes to heap$`)

// inlinedResult reports whether msg, an escape at a call to callee the
// compiler inlined, is the caller's boxing of one of callee's results, and if
// so how to name it.
func inlinedResult(callee *ssa.Function, msg string) (string, bool) {
	if callee == nil {
		return "the result of an inlined call escapes to heap", unnamedResult.MatchString(msg)
	}
	results := callee.Signature.Results()
	for i := range results.Len() {
		name := results.At(i).Name()
		if name == "" || name == "_" {
			name = "~r" + strconv.Itoa(i)
		}
		if msg == name+" escapes to heap" {
			return "the result of " + funcName(callee) + " escapes to heap", true
		}
	}
	return "", false
}

type escape struct {
	file      string
	line, col int
	msg       string
	// inlined is an escape at a call the compiler inlined, where it reports
	// the inlined body's escapes as well as the caller's own.
	inlined bool
	// nth tells apart escapes an inlined body reports at one call.
	nth int
}

func (e escape) site() site {
	return site{pos: token.Position{Filename: e.file, Line: e.line, Column: e.col}, nth: e.nth, reason: "escapes: " + e.msg}
}

var diagLine = regexp.MustCompile(`^(.+\.go):(\d+):(\d+): (.*)$`)

// compilerEscapes builds each pattern with the compiler's escape report and
// assembly for the module's packages, and returns the heap allocations it
// reports and the runtime calls their compiled code makes.
func compilerEscapes(cfg Config, env []string, gcflags []string) (compilerReport, error) {
	var escapes, notes, inlines []escape
	asm := &asmReader{seen: map[string]bool{}, starts: map[string]asmPos{}}
	seen := map[escape]bool{}
	var overlay []string
	if len(cfg.Overlay) > 0 {
		dir, err := os.MkdirTemp("", "noalloc-overlay")
		if err != nil {
			return compilerReport{}, err
		}
		defer os.RemoveAll(dir)
		f, err := writeOverlay(dir, cfg.Overlay)
		if err != nil {
			return compilerReport{}, err
		}
		overlay = []string{"-overlay", f}
	}
	goenv := exec.Command("go", "env", "GOROOT", "GOMODCACHE")
	goenv.Dir, goenv.Env = cfg.Dir, env
	roots, err := goenv.Output()
	if err != nil {
		return compilerReport{}, err
	}
	paths := map[string]string{}
	const flags = "-m -S"
	for _, pat := range cfg.Patterns {
		args := append([]string{"build"}, overlay...)
		args = append(args, gcflags...)
		if gcflags == nil {
			for _, m := range append([]string{cfg.Module}, cfg.SourceModules...) {
				args = append(args, "-gcflags="+m+"/...="+flags)
			}
		}
		args = append(args, "-o", os.DevNull, pat)
		cmd := exec.Command("go", args...)
		cmd.Dir, cmd.Env = cfg.Dir, env
		// The assembly of a large module runs to hundreds of megabytes, so it
		// is read as it is written.
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		if err := cmd.Start(); err != nil {
			return compilerReport{}, err
		}
		waited := make(chan error, 1)
		go func() {
			err := cmd.Wait()
			pw.Close()
			waited <- err
		}()
		var last []string
		// The compiler reports an inlined body's escapes at the call it was
		// inlined into; the body's own compile reports them at their lines.
		// It reports each once for each time it inlines the call, once in
		// each shape a generic caller compiles to.
		inlined := map[escape]int{}
		var found []escape
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			text := sc.Text()
			if last = append(last, text); len(last) > 40 {
				last = last[1:]
			}
			asm.line(text)
			m := diagLine.FindStringSubmatch(text)
			if m == nil {
				continue
			}
			file, ok := paths[m[1]]
			if !ok {
				file = reportedFile(m[1], cfg.Dir, append(strings.Fields(string(roots)), cfg.Dir, cfg.Root))
				paths[m[1]] = file
			}
			line, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			at, msg := escape{file: file, line: line, col: col}, m[4]
			if !strings.HasPrefix(msg, "inlining call to ") && !strings.HasPrefix(msg, "can inline ") {
				notes = append(notes, at)
			}
			switch {
			case strings.HasPrefix(msg, "inlining call to "):
				if inlined[at] == 0 {
					inlines = append(inlines, at)
				}
				inlined[at]++
			case msg == "append escapes to heap":
				// A growing append is the append rule's, which knows the Scratch.
			case strings.HasPrefix(msg, "moved to heap: "), strings.HasSuffix(msg, " escapes to heap"):
				at.msg = foldShapes(msg)
				found = append(found, at)
			}
		}
		scanErr := sc.Err()
		io.Copy(io.Discard, pr)
		if err := <-waited; err != nil {
			return compilerReport{}, fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, tail(strings.Join(last, "\n"), 2000))
		}
		if scanErr != nil {
			return compilerReport{}, scanErr
		}
		reports := map[escape]int{}
		for _, e := range found {
			n := inlined[escape{file: e.file, line: e.line, col: e.col}]
			e.inlined = n > 0
			if e.inlined {
				// Of the n reports of each escape in an inlined body, the
				// first stands for the body's first allocation of that text.
				e.nth = reports[e] / n
				reports[e]++
			}
			if !seen[e] {
				seen[e] = true
				escapes = append(escapes, e)
			}
		}
	}
	return compilerReport{escapes, notes, inlines, asm}, nil
}

// compilerReport is what compilerEscapes reads off the compiler: its
// escapes; notes, where it reported anything of its escape analysis; inlines,
// where it inlined a call; and the assembly it printed.
type compilerReport struct {
	escapes, notes, inlines []escape
	asm                     *asmReader
}

// reportedFile is the file at path, as the compiler's report gives it, built
// in dir. The build cache replays a report as the compile that wrote it
// printed it, relative to the directory that compile ran in, so a relative
// path dir does not hold is found under an ancestor of one of roots.
func reportedFile(path, dir string, roots []string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	joined := filepath.Join(dir, path)
	if _, err := os.Stat(joined); err == nil {
		return joined
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	for len(parts) > 1 && (parts[0] == ".." || parts[0] == ".") {
		parts = parts[1:]
	}
	rest := filepath.Join(parts...)
	for _, root := range roots {
		if root == "" {
			continue
		}
		for a := root; ; a = filepath.Dir(a) {
			p := filepath.Join(a, rest)
			if strings.HasPrefix(p, root+string(filepath.Separator)) {
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
			if filepath.Dir(a) == a {
				break
			}
		}
	}
	return joined
}

// writeOverlay writes files into dir with an overlay file, as go build
// -overlay takes, that puts each in place of its path, and returns that file.
func writeOverlay(dir string, files map[string][]byte) (string, error) {
	replace := map[string]string{}
	for path, b := range files {
		f := filepath.Join(dir, strconv.Itoa(len(replace))+".go")
		if err := os.WriteFile(f, b, 0o644); err != nil {
			return "", err
		}
		replace[path] = f
	}
	j, _ := json.Marshal(map[string]any{"Replace": replace})
	f := filepath.Join(dir, "overlay.json")
	return f, os.WriteFile(f, j, 0o644)
}

// foldShapes replaces each generic type argument list the compiler names a
// shape in, as in "&Entry[go.shape.string,go.shape.int64]{...}", with
// "[...]", so a line of a generic function reads the same however many
// types it is instantiated with.
func foldShapes(msg string) string {
	for {
		i := strings.Index(msg, "[go.shape.")
		if i < 0 {
			return msg
		}
		depth, j := 0, i
		for ; j < len(msg); j++ {
			if msg[j] == '[' {
				depth++
			} else if msg[j] == ']' {
				if depth--; depth == 0 {
					break
				}
			}
		}
		if j == len(msg) {
			return msg
		}
		msg = msg[:i] + "[...]" + msg[j+1:]
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// allowlist matches the functions and calls a check takes to allocate
// nothing although their code does not show it.
type allowlist struct {
	// assumed holds each overridden function's kind, by its name as
	// funcName has it or a pattern ending in "*", and used the ones a check
	// relied on.
	assumed map[string]string
	used    map[string]bool
}

// parseAllow reads allowlist lines, each "kind: function" with an optional
// "# comment"; a line of any other form is an error.
func parseAllow(lines []string) (allowlist, error) {
	a := allowlist{assumed: map[string]string{}, used: map[string]bool{}}
	var bad []string
	for _, l := range lines {
		if i := strings.Index(l, "#"); i >= 0 {
			l = l[:i]
		}
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		kind, fn, labeled := strings.Cut(l, ":")
		if !labeled || !slices.Contains(assumptionKinds, kind) || strings.TrimSpace(fn) == "" {
			bad = append(bad, l)
			continue
		}
		a.assumed[strings.TrimSpace(fn)] = kind
	}
	if len(bad) > 0 {
		return a, fmt.Errorf("an allowlist line names a function and the kind of reason it allocates nothing, as \"kind: function\" with a kind of %s; what a function allocates is worked out from its code otherwise: %s",
			strings.Join(assumptionKinds, ", "), strings.Join(bad, "; "))
	}
	return a, nil
}
