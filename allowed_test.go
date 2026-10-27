package noalloc

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
)

// TestNoBlockEveryNormalReturnRunsIsAllowed checks every allowance at once:
// in each function reached in the fixture and in bounded, a block that every
// path to a normal return runs is checked, so no allowance covers code that
// runs on every call.
func TestNoBlockEveryNormalReturnRunsIsAllowed(t *testing.T) {
	defer func() { testHookAllowed = nil }()
	for _, m := range []struct{ dir, module string }{
		{"testdata/fixture", "fixture"},
		{"bounded", "github.com/theconsensuslabs/noalloc/bounded"},
	} {
		dir, err := filepath.Abs(m.dir)
		if err != nil {
			t.Fatal(err)
		}
		checked := 0
		testHookAllowed = func(c *checker, fn *ssa.Function, ok map[*ssa.BasicBlock]bool) {
			checked++
			for _, b := range fn.Blocks {
				if !ok[b] && everyNormalReturnRuns(c, fn, b) {
					t.Errorf("%s: block %d of %s runs on every normal return but is allowed to allocate",
						m.dir, b.Index, fn)
				}
			}
		}
		if _, err := Check(Config{Dir: dir, Patterns: []string{"./..."}, Module: m.module,
			Allow: strings.Split(Allowlist, "\n"), Root: dir}); err != nil {
			t.Fatal(err)
		}
		if checked < 20 {
			t.Errorf("%s: %d functions checked, want at least 20", m.dir, checked)
		}
	}
}

// everyNormalReturnRuns reports whether fn has a normal return, one that does
// not return a non-nil error, and every path from its entry to one runs skip.
func everyNormalReturnRuns(c *checker, fn *ssa.Function, skip *ssa.BasicBlock) bool {
	errIdx := errorResult(fn)
	normal := func(b *ssa.BasicBlock) bool {
		r, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return)
		return ok && (errIdx < 0 || !c.definitelyNonNil(r.Results[errIdx], b, map[ssa.Value]bool{}))
	}
	found := false
	for _, b := range fn.Blocks {
		found = found || normal(b)
	}
	if !found || fn.Blocks[0] == skip {
		return found
	}
	seen := map[*ssa.BasicBlock]bool{fn.Blocks[0]: true}
	stack := []*ssa.BasicBlock{fn.Blocks[0]}
	for len(stack) > 0 {
		b := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if normal(b) {
			return false
		}
		for _, s := range b.Succs {
			if s != skip && !seen[s] {
				seen[s] = true
				stack = append(stack, s)
			}
		}
	}
	return true
}
