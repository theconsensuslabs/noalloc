package noalloc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An exemption on a line that one of noalloc's allowances already lets
// through covers nothing, so the ratchet reports it as empty. One row per
// allowance: a field set on first use, the same through sync/atomic, a
// failure return, a panic, and a branch that makes an error and goes on.
func TestAnExemptionOnAnAllowedLineIsEmpty(t *testing.T) {
	dir, err := filepath.Abs("testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "fixture.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	over := strings.Replace(string(src), "import (\n", "import (\n\t\"sync/atomic\"\n", 1) + `
type allowedBox struct {
	m    map[int]int
	done atomic.Value
}

//noalloc:root
func (b *allowedBox) FirstUse(k int) int {
	if b.m == nil {
		//noalloc:exempt setup: first use
		b.m = map[int]int{}
	}
	return b.m[k]
}

//noalloc:root
func (b *allowedBox) Atomic() chan struct{} {
	d := b.done.Load()
	if d == nil {
		//noalloc:exempt setup: first use through an atomic
		d = make(chan struct{})
		b.done.Store(d)
	}
	return d.(chan struct{})
}

//noalloc:root
func AllowedFailure(n int) error {
	if n < 0 {
		//noalloc:exempt setup: a failure return
		return fmt.Errorf("negative %d", n)
	}
	return nil
}

//noalloc:root
func AllowedPanic(n int) int {
	if n < 0 {
		//noalloc:exempt setup: a panic
		panic(fmt.Sprintf("negative %d", n))
	}
	return n
}

type allowedRec struct{ errs int }

func (r *allowedRec) fail(err error) { r.errs++ }

//noalloc:root
func AllowedOnce(r *allowedRec, n int) int {
	allowedRecordIfNegative(r, n)
	return n
}

func allowedRecordIfNegative(r *allowedRec, n int) {
	if n < 0 {
		//noalloc:exempt setup: a failure that goes on
		r.fail(fmt.Errorf("negative %d", n))
	}
}
`
	check := func(src string) *Result {
		t.Helper()
		res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "fixture", Scratch: []string{"fixture.Scratch"},
			Allow: strings.Split(Allowlist, "\n"), Root: dir, Overlay: map[string][]byte{file: []byte(src)}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	ours := func(f Finding) bool {
		return strings.HasPrefix(f.Func, "(*fixture.allowedBox)") || strings.HasPrefix(f.Func, "fixture.Allowed") ||
			f.Func == "fixture.allowedRecordIfNegative"
	}
	// Control: with every exemption stripped, the allowances alone keep each line from being a finding.
	for _, f := range check(strings.ReplaceAll(over, "//noalloc:exempt", "// stripped")).Findings {
		if ours(f) {
			t.Errorf("control: with no exemption, an allowed line is a finding: %s", f)
		}
	}
	res := check(over)
	d, err := (&Ratchet{}).Compare(res)
	if err != nil {
		t.Fatal(err)
	}
	empty := map[string]bool{}
	for _, e := range d.Empty {
		empty[e.Reason] = true
	}
	for _, reason := range []string{"setup: first use", "setup: first use through an atomic", "setup: a failure return",
		"setup: a panic", "setup: a failure that goes on"} {
		if !empty[reason] {
			for _, e := range res.Exemptions {
				if e.Reason == reason {
					t.Errorf("%q is not reported empty: reached %v, covers %q", reason, e.Reached, e.Covers)
				}
			}
		}
	}
}
