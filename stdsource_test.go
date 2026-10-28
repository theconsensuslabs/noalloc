package noalloc

import (
	"slices"
	"strings"
	"testing"
)

// TestAMethodOnlyReflectionCallsIsChecked checks that a method of a value the
// module hands to the standard library is checked when the library is checked
// from source, as when it is not: text/template calls it through reflection,
// which leaves no call to follow.
func TestAMethodOnlyReflectionCallsIsChecked(t *testing.T) {
	src := `
type reflected struct{ n int }

func (d reflected) Big() string {
	return fmt.Sprintf("%d is big", d.n)
}

var reflectedTmpl = template.Must(template.New("t").Parse("{{.Big}}"))

//noalloc:root
func RenderReflected(w io.Writer, n int) {
	for range n {
		_ = reflectedTmpl.Execute(w, reflected{n})
	}
}
`
	for _, source := range [][]string{nil, {"text/template", "reflect", "fmt", "internal/fmtsort"}} {
		res := checkFrom(t, []string{"io", "text/template"}, src, source)
		if !flagged(res, "(fixture.reflected).Big", `return fmt.Sprintf("%d is big",d.n)`) {
			t.Errorf("with %q from source, the method the template calls is not checked", source)
		}
	}
}

// TestAFunctionWithNoBodyIsJudgedByItsMachineCode checks that a function of
// a package checked from source that has no Go body, one written in
// assembly, is judged by its compiled code, as the same function outside the
// module is: internal/bytealg.IndexByte allocates nothing.
func TestAFunctionWithNoBodyIsJudgedByItsMachineCode(t *testing.T) {
	res := checkFrom(t, nil, `
//noalloc:root
func FindByte(b []byte) int {
	return bytes.IndexByte(b, '/')
}
`, []string{"bytes", "internal/bytealg"})
	if !slices.Contains(res.Roots, "fixture.FindByte") {
		t.Fatalf("control: fixture.FindByte was not checked; checked %q", res.Roots)
	}
	for _, f := range res.Findings {
		if strings.HasPrefix(f.Func, "bytes.") || f.Func == "fixture.FindByte" {
			t.Errorf("allocates: %s", f)
		}
	}
}

// TestSyncCheckedFromSourceChangesNothing checks that sync works as it does
// outside the module when it is checked from source, also when the standard
// library calls it: the function a sync.Once runs is setup that runs once, so
// nothing it reaches is a finding. hash/crc32 builds its tables under one.
func TestSyncCheckedFromSourceChangesNothing(t *testing.T) {
	res := checkFrom(t, []string{"hash/crc32"}, `
//noalloc:root
func Checksum(b []byte) uint32 {
	return crc32.ChecksumIEEE(b)
}
`, []string{"hash/crc32", "sync"})
	for _, f := range res.Findings {
		if strings.HasPrefix(f.Func, "sync.") || slices.ContainsFunc(f.Path, func(p string) bool { return strings.HasPrefix(p, "sync.") }) {
			t.Errorf("a finding behind sync: %s via %s", f, strings.Join(f.Path, " > "))
		}
	}
}

// TestAPackageOnlyTheOverlayImportsIsCheckedFromSource checks that a package
// the overlay's files import, and the files on disk do not, is checked from
// source when Config.SourceModules names it: its calls are followed rather
// than reported.
func TestAPackageOnlyTheOverlayImportsIsCheckedFromSource(t *testing.T) {
	res := checkFrom(t, []string{"html"}, `
//noalloc:root
func Escape(s string) string {
	return html.EscapeString(s)
}
`, []string{"html", "strings"})
	if slices.ContainsFunc(res.Findings, func(f Finding) bool {
		return f.Func == "fixture.Escape" && slices.Contains(f.Reasons, "calls html.EscapeString")
	}) {
		t.Error("html.EscapeString is reported as a call out, not followed into its source")
	}
}

// TestAFunctionHandedToAGenericIsChecked checks that a closure the module
// hands to an instance of a generic function outside it is checked, as one
// handed to any function outside it is: the instance belongs to no package
// of its own, only its generic function does.
func TestAFunctionHandedToAGenericIsChecked(t *testing.T) {
	res := checkFrom(t, []string{"slices"}, `
//noalloc:root
func SortByJoin(xs []string) {
	slices.SortFunc(xs, func(a, b string) int {
		return len(a + b)
	})
}
`, nil)
	if !slices.ContainsFunc(res.Findings, func(f Finding) bool {
		return strings.HasPrefix(f.Func, "fixture.SortByJoin$") && slices.Contains(f.Reasons, "string concatenation")
	}) {
		t.Error("the closure handed to slices.SortFunc is not checked")
	}
}
