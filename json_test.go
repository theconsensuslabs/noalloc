package noalloc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// jsonModule is this repository's json package.
const jsonModule = "github.com/theconsensuslabs/noalloc/encoding/json"

// jsonRuntimeFiles are the json package's files, but its tests.
func jsonRuntimeFiles(t *testing.T) []string {
	paths, err := filepath.Glob("encoding/json/*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, p := range paths {
		if !strings.HasSuffix(p, "_test.go") {
			files = append(files, filepath.Base(p))
		}
	}
	if len(files) == 0 {
		t.Fatal("no files in encoding/json")
	}
	return files
}

// jsonRuntimeExemptions are the only exemptions the runtime may hold, as
// function: reason: the calls into the caller's code that hand it memory or
// input.
var jsonRuntimeExemptions = []string{
	"(*" + jsonModule + ".Arena).grow: response: the caller's function, which hands over memory it chose.",
	"(*" + jsonModule + ".Reader).more: response: the caller's reader, which may allocate as it reads.",
}

// markedRoots are the functions files of package pkg in dir mark
// //noalloc:root or //noalloc:append, as SSA names them, and of those the
// generic ones, which are checked through their instances.
func markedRoots(t *testing.T, dir, pkg string, files []string) (roots, generic []string) {
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Doc == nil || !slices.ContainsFunc(fn.Doc.List, func(c *ast.Comment) bool {
				return c.Text == "//noalloc:root" || c.Text == "//noalloc:append"
			}) {
				continue
			}
			if fn.Recv == nil {
				roots = append(roots, pkg+"."+fn.Name.Name)
				if fn.Type.TypeParams != nil {
					generic = append(generic, pkg+"."+fn.Name.Name)
				}
				continue
			}
			recv := fn.Recv.List[0].Type.(*ast.StarExpr).X.(*ast.Ident).Name
			roots = append(roots, "(*"+pkg+"."+recv+")."+fn.Name.Name)
		}
	}
	return roots, generic
}

// TestJSONRuntimeAndGeneratedCodeAllocateNothing checks, from source, every
// root the json package marks and the methods jsongen generates for the
// gentest types: none may allocate, and the runtime's only exemptions are
// its calls into the caller's reader and Arena.More.
func TestJSONRuntimeAndGeneratedCodeAllocateNothing(t *testing.T) {
	dir, err := filepath.Abs("encoding/json")
	if err != nil {
		t.Fatal(err)
	}
	allow := strings.Split(Allowlist, "\n")
	// bounded is checked from source, as a caller of this package checks it.
	sources := []string{boundedModule}
	files := jsonRuntimeFiles(t)
	roots, generic := markedRoots(t, dir, jsonModule, files)
	generated, _ := markedRoots(t, filepath.Join(dir, "internal/gentest"), jsonModule+"/internal/gentest", []string{"json_gen.go", "locals.go"})
	if len(generated) == 0 {
		t.Fatal("no //noalloc:root in the generated code")
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{".", "./internal/gentest"}, Module: jsonModule, SourceModules: sources,
		Root: dir, Allow: allow})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range append(slices.Clone(roots), generated...) {
		if !slices.Contains(res.Roots, r) && !slices.Contains(generic, r) {
			t.Errorf("root %s was not checked; checked %q", r, res.Roots)
		}
	}
	for _, f := range res.Findings {
		t.Errorf("allocates: %s (path %s)", f, strings.Join(f.Path, " > "))
	}
	var exempt []string
	for _, e := range res.Exemptions {
		if slices.Contains(files, e.File) {
			exempt = append(exempt, e.Func+": "+e.Reason)
		}
	}
	slices.Sort(exempt)
	if !slices.Equal(exempt, jsonRuntimeExemptions) {
		t.Errorf("the runtime's exemptions are\n%s\nwant\n%s", strings.Join(exempt, "\n"), strings.Join(jsonRuntimeExemptions, "\n"))
	}
}

// TestSourceModulesAreCheckedFromSource checks gentest's generated code with
// and without the json package as a source module: without it, a call into
// the package is a call out of the module that no allowlist line covers; with
// it, the calls are followed and the package's exemptions listed.
func TestSourceModulesAreCheckedFromSource(t *testing.T) {
	dir, err := filepath.Abs("encoding/json")
	if err != nil {
		t.Fatal(err)
	}
	gentest := jsonModule + "/internal/gentest"
	allow := strings.Split(Allowlist, "\n")
	cfg := Config{Dir: dir, Patterns: []string{"./internal/gentest"}, Module: gentest, Root: dir, Allow: allow}
	res, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	unvouched := 0
	for _, f := range res.Findings {
		for _, r := range f.Reasons {
			if strings.Contains(r, "calls (*"+jsonModule+".Writer)") {
				unvouched++
			}
		}
	}
	if unvouched == 0 {
		t.Fatal("control: with json out of the module, no call into its Writer was reported")
	}

	cfg.SourceModules = []string{jsonModule, boundedModule}
	if res, err = Check(cfg); err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings {
		t.Errorf("allocates: %s (path %s)", f, strings.Join(f.Path, " > "))
	}
	files := jsonRuntimeFiles(t)
	var exempt []string
	for _, e := range res.Exemptions {
		if slices.Contains(files, e.File) {
			exempt = append(exempt, e.Func+": "+e.Reason)
		}
	}
	slices.Sort(exempt)
	if !slices.Equal(exempt, jsonRuntimeExemptions) {
		t.Errorf("the json package's exemptions are\n%s\nwant\n%s", strings.Join(exempt, "\n"), strings.Join(jsonRuntimeExemptions, "\n"))
	}
}
