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
	// Allow lists functions outside the module known not to allocate, one
	// pattern a line as Allowlist describes.
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

// Allowlist is the allowlist checked in beside this file. Each line is a
// function as SSA names it ("strings.Index", "(*bytes.Reader).Read"), a
// package or receiver with ".*" for all of its functions ("math/bits.*",
// "(*sync.Mutex).*"), or an interface method ("context.Context.Err") for a
// call whose concrete callee comes from outside the module, or "append: "
// and a function that appends to its first argument as append does. Text
// after # is a comment.
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
	// once: what its line allocates and, unless its kind is LineOnly, what
	// its line calls that no root reaches another way.
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
}

// Check runs the checker.
func Check(cfg Config) (*Result, error) {
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
		calls   []runtimeCall
		starts  map[string]asmPos
		err     error
	}
	done := make(chan compiled, 1)
	go func() {
		e, notes, asm, err := compilerEscapes(cfg, env)
		if err != nil {
			done <- compiled{err: err}
			return
		}
		done <- compiled{e, notes, asm.calls, asm.starts, nil}
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

	c := &checker{cfg: cfg, prog: prog, fset: prog.Fset, allow: parseAllow(cfg.Allow),
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
	c.graph()
	c.reach(groupRoots(cfg.Groups, roots))
	c.cover()

	res := &Result{}
	for _, r := range roots {
		res.Roots = append(res.Roots, r.String())
	}
	found := map[string]map[int]*Finding{}
	add := func(fn *ssa.Function, pos token.Position, reason string) {
		if is, ok := c.coveredBy[fn]; ok {
			for _, i := range is {
				c.addCover(i, fn, reason)
			}
			return
		}
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
		if !slices.Contains(f.Reasons, reason) {
			f.Reasons = append(f.Reasons, reason)
		}
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
	for i := range c.exemptions {
		if i < len(c.covered) {
			c.exemptions[i].Covers = slices.Sorted(maps.Keys(c.covered[i]))
		}
	}
	res.Exemptions = c.exemptions

	for _, byLine := range found {
		for _, f := range byLine {
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

// Report is res as text: a summary, the count of findings in each file, and
// each finding with the call path that reaches its function.
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
	return b.String()
}

// listModule returns the module's packages the patterns reach.
func listModule(cfg Config, env []string) (ours []string, err error) {
	graph, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports |
		packages.NeedDeps, Dir: cfg.Dir, Env: env}, cfg.Patterns...)
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
	covered     []map[string]bool
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
	return fn.Pkg != nil && c.inModule(fn.Pkg.Pkg.Path())
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
	return !c.ours(fn) && fn.Pkg != nil
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
					} else if f := in.Call.StaticCallee(); f != nil && (c.allow.appends[funcName(f)] || c.appendRoot(f)) {
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
	return c.errorEnds(call.Block(), err, ok, false)
}

// errorEnds reports whether every path from b on which err is not nil, and
// not a package-level sentinel, ends without err being dropped: in a block
// not in ok, which is a failure, in a panic, or in a return of err as b's
// function's error, or, with nonNil, of any error that cannot be nil. A nil
// ok counts no block as a failure. It follows err through the phis it
// reaches and into a module helper that returns it, and gives up on a path
// that comes back to b or runs past a fixed number of steps.
func (c *checker) errorEnds(b *ssa.BasicBlock, err ssa.Value, ok map[*ssa.BasicBlock]bool, nonNil bool) bool {
	idx := errorResult(b.Parent())
	steps := 0
	onPath := map[*ssa.BasicBlock]bool{}
	var walk func(x, from *ssa.BasicBlock, is map[ssa.Value]bool) bool
	walk = func(x, from *ssa.BasicBlock, is map[ssa.Value]bool) bool {
		if ok != nil && !ok[x] {
			return true
		}
		// Coming back to b runs it again with err dropped. Another loop's
		// way out is a path of its own.
		if x == b && from != nil {
			return false
		}
		if onPath[x] {
			return true
		}
		if steps++; steps > errorEndsSteps {
			return false
		}
		is = maps.Clone(is)
		for _, in := range x.Instrs {
			switch in := in.(type) {
			case *ssa.Phi:
				if from != nil && is[in.Edges[slices.Index(x.Preds, from)]] {
					is[in] = true
				}
			case *ssa.Call:
				if out := c.carried(in, is); out != nil {
					is[out] = true
				}
			}
		}
		succs := x.Succs
		switch last := x.Instrs[len(x.Instrs)-1].(type) {
		case *ssa.Panic:
			return true
		case *ssa.Return:
			if idx < 0 {
				return false
			}
			v := last.Results[idx]
			return is[v] || nonNil && c.definitelyNonNil(v, x, map[ssa.Value]bool{})
		case *ssa.If:
			if arm, decided := errArm(last, is); decided {
				succs = succs[arm : arm+1]
			}
		}
		if len(succs) == 0 {
			return false
		}
		onPath[x] = true
		defer delete(onPath, x)
		for _, s := range succs {
			if !walk(s, x, is) {
				return false
			}
		}
		return true
	}
	return walk(b, nil, map[ssa.Value]bool{err: true})
}

// errorEndsSteps bounds the blocks errorEnds visits for one error.
const errorEndsSteps = 4096

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
	default:
		return 0, false
	}
	if cmp.Op == token.NEQ {
		return 0, true
	}
	return 1, true
}

// carried is the error result of call when call passes one of is to a
// module function that returns it whenever it is a failure, or nil.
func (c *checker) carried(call *ssa.Call, is map[ssa.Value]bool) ssa.Value {
	callee := call.Call.StaticCallee()
	if callee == nil || !c.ours(callee) || callee.Blocks == nil {
		return nil
	}
	idx := errorResult(callee)
	if idx < 0 {
		return nil
	}
	for k, a := range call.Call.Args {
		if !is[a] || k >= len(callee.Params) || !c.carries(callee, k) {
			continue
		}
		if callee.Signature.Results().Len() == 1 {
			return call
		}
		for _, r := range *call.Referrers() {
			if x, isExtract := r.(*ssa.Extract); isExtract && x.Index == idx {
				return x
			}
		}
		return nil
	}
	return nil
}

// carries reports whether fn returns a non-nil error whenever its k-th
// parameter is a non-nil error other than a package-level sentinel.
func (c *checker) carries(fn *ssa.Function, k int) bool {
	key := carryKey{fn, k}
	if v, seen := c.carry[key]; seen {
		return v
	}
	c.carry[key] = false
	v := types.Identical(fn.Params[k].Type(), errorType) && c.errorEnds(fn.Blocks[0], fn.Params[k], nil, true)
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
			next := []*ssa.Function{callee}
			fails := []failures{c.calleeFailures(fn, e.Site, callee, ok)}
			if c.outside(callee) {
				if callee.Pkg != nil && callee.Pkg.Pkg.Path() == "sync" {
					continue
				}
				for _, f := range c.handedOver(e.Site) {
					next = append(next, f)
					fails = append(fails, 0)
				}
			}
			for i, f := range next {
				if c.roots[f] {
					fails[i] = failOnce
				}
				if _, seen := c.parent[f]; !seen {
					c.parent[f] = fn
					c.failing[f] = fails[i]
					queue = append(queue, f)
				} else if was := c.failuresOf(f); was&fails[i] != was {
					c.failing[f] = was & fails[i]
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

// addCover records that exemption i lets reason through in fn.
func (c *checker) addCover(i int, fn *ssa.Function, reason string) {
	for len(c.covered) <= i {
		c.covered = append(c.covered, nil)
	}
	if c.covered[i] == nil {
		c.covered[i] = map[string]bool{}
	}
	c.covered[i][c.stableName(fn)+": "+reason] = true
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
		next := []*ssa.Function{callee}
		fails := []failures{c.calleeFailures(fn, e.Site, callee, ok)}
		if c.outside(callee) {
			if callee.Pkg != nil && callee.Pkg.Pkg.Path() == "sync" {
				return
			}
			for _, f := range c.handedOver(e.Site) {
				next = append(next, f)
				fails = append(fails, 0)
			}
		}
		for i, f := range next {
			if _, reached := c.parent[f]; reached {
				continue
			}
			if c.roots[f] {
				fails[i] = failOnce
			}
			have := c.coveredBy[f]
			_, seen := c.coveredBy[f]
			if !seen {
				c.coverOrder = append(c.coverOrder, f)
			}
			stricter := false
			if was := c.failuresOf(f); !seen {
				c.failing[f] = fails[i]
			} else if was&fails[i] != was {
				c.failing[f] = was & fails[i]
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
// where that branch stores to it.
func lazyInit(fn *ssa.Function) []*ssa.BasicBlock {
	var out []*ssa.BasicBlock
	for _, p := range fn.Blocks {
		ifi, ok := p.Instrs[len(p.Instrs)-1].(*ssa.If)
		if !ok {
			continue
		}
		x, op := nilCompare(ifi.Cond)
		load, ok := x.(*ssa.UnOp)
		if !ok || load.Op != token.MUL {
			continue
		}
		switch load.X.(type) {
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
				if st, ok := in.(*ssa.Store); ok && sameAddr(st.Addr, load.X) {
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
func (c *checker) check(fn *ssa.Function, add func(*ssa.Function, token.Position, string)) {
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
					c.addCover(i, fn, r)
				}
				continue
			}
			for _, r := range c.instrAllocs(in) {
				add(fn, c.fset.Position(pos), r)
			}
		}
	}
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
		if in.Op == token.ADD && isString(in.Type()) {
			out = append(out, "string concatenation")
		}
	case *ssa.Convert:
		if conv := stringConversion(in); conv != "" && !compilerAvoidsCopy(in) {
			out = append(out, conv)
		}
	case *ssa.MakeSlice:
		if !isConst(in.Len) || !isConst(in.Cap) {
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
		if desc := callDesc(cm); !c.allow.exact[desc] {
			return []string{"call with no known callee: " + desc}
		}
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
		if c.ours(callee) {
			continue
		}
		name := funcName(callee)
		if c.allow.appends[name] {
			if dst := appendedTo(cm, callee); dst == nil || !c.reused(dst) {
				out = append(out, "calls "+name)
			} else if !c.ownAppendCall(call, dst) {
				out = append(out, "calls "+name+", which may grow reused memory")
			}
			continue
		}
		if !c.allow.has(callee) {
			out = append(out, "calls "+name)
		}
	}
	return out
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
		if callee != nil && (c.allow.appends[funcName(callee)] || c.appendRoot(callee)) {
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
	}
	return false
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
func (c *checker) matchEscapes(escapes, notes []escape, add func(*ssa.Function, token.Position, string)) {
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
		if !e.inlined && constantText(e.msg) && c.constantAt(e.file, pos) {
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
			c.addCover(i, in.fns[0], "escapes: "+e.msg)
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
		add(fn, token.Position{Filename: e.file, Line: e.line}, "escapes: "+e.msg)
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

// constantAt reports whether the expression the compiler reports an escape
// of at pos in file is a constant, which the compiler converts to an
// interface as read-only data: the escape allocates nothing.
func (c *checker) constantAt(file string, pos token.Pos) bool {
	f, info := c.files[file], c.fileInfo[file]
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
}

var diagLine = regexp.MustCompile(`^(.+\.go):(\d+):(\d+): (.*)$`)

// compilerEscapes builds each pattern with the compiler's escape report and
// assembly for the module's packages, and returns the heap allocations it
// reports and the runtime calls their compiled code makes.
func compilerEscapes(cfg Config, env []string) ([]escape, []escape, *asmReader, error) {
	var escapes, notes []escape
	asm := &asmReader{seen: map[string]bool{}, starts: map[string]asmPos{}}
	seen := map[escape]bool{}
	var overlay []string
	if len(cfg.Overlay) > 0 {
		dir, err := os.MkdirTemp("", "noalloc-overlay")
		if err != nil {
			return nil, nil, nil, err
		}
		defer os.RemoveAll(dir)
		replace := map[string]string{}
		for path, b := range cfg.Overlay {
			f := filepath.Join(dir, strconv.Itoa(len(replace))+".go")
			if err := os.WriteFile(f, b, 0o644); err != nil {
				return nil, nil, nil, err
			}
			replace[path] = f
		}
		j, _ := json.Marshal(map[string]any{"Replace": replace})
		f := filepath.Join(dir, "overlay.json")
		if err := os.WriteFile(f, j, 0o644); err != nil {
			return nil, nil, nil, err
		}
		overlay = []string{"-overlay", f}
	}
	const flags = "-m -S"
	for _, pat := range cfg.Patterns {
		args := append([]string{"build"}, overlay...)
		for _, m := range append([]string{cfg.Module}, cfg.SourceModules...) {
			args = append(args, "-gcflags="+m+"/...="+flags)
		}
		args = append(args, "-o", os.DevNull, pat)
		cmd := exec.Command("go", args...)
		cmd.Dir, cmd.Env = cfg.Dir, env
		// The assembly of a large module runs to hundreds of megabytes, so it
		// is read as it is written.
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		if err := cmd.Start(); err != nil {
			return nil, nil, nil, err
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
		inlined := map[escape]bool{}
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
			file := m[1]
			if !filepath.IsAbs(file) {
				file = filepath.Join(cfg.Dir, file)
			}
			line, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			at, msg := escape{file: filepath.Clean(file), line: line, col: col}, m[4]
			if !strings.HasPrefix(msg, "inlining call to ") && !strings.HasPrefix(msg, "can inline ") {
				notes = append(notes, at)
			}
			switch {
			case strings.HasPrefix(msg, "inlining call to "):
				inlined[at] = true
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
			return nil, nil, nil, fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, tail(strings.Join(last, "\n"), 2000))
		}
		if scanErr != nil {
			return nil, nil, nil, scanErr
		}
		for _, e := range found {
			e.inlined = inlined[escape{file: e.file, line: e.line, col: e.col}]
			if !seen[e] {
				seen[e] = true
				escapes = append(escapes, e)
			}
		}
	}
	return escapes, notes, asm, nil
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

// allowlist matches functions outside the module known not to allocate,
// and, in appends, those that append to their first argument and return it
// as the builtin append does.
type allowlist struct {
	exact    map[string]bool
	prefixes []string
	appends  map[string]bool
}

func parseAllow(lines []string) allowlist {
	a := allowlist{exact: map[string]bool{}, appends: map[string]bool{}}
	for _, l := range lines {
		if i := strings.Index(l, "#"); i >= 0 {
			l = l[:i]
		}
		l = strings.TrimSpace(l)
		switch {
		case l == "":
		case strings.HasPrefix(l, "append:"):
			a.appends[strings.TrimSpace(strings.TrimPrefix(l, "append:"))] = true
		case strings.HasSuffix(l, ".*"):
			a.prefixes = append(a.prefixes, strings.TrimSuffix(l, "*"))
		default:
			a.exact[l] = true
		}
	}
	return a
}

func (a allowlist) has(fn *ssa.Function) bool { return a.hasName(funcName(fn)) }

// hasName reports whether the function SSA names name is on the list.
func (a allowlist) hasName(name string) bool {
	if a.exact[name] {
		return true
	}
	for _, p := range a.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
