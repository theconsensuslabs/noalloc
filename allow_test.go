package noalloc

import (
	"hash/maphash"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/theconsensuslabs/noalloc/alloctest"
)

// sinkInt is what the exercisers write to, so the compiler keeps the calls.
var sinkInt int

// allowExercisers runs each allowlist entry that a test can vouch for the way
// the code it covers calls it, with its arguments made beforehand, so a run
// allocates only if the entry does.
func allowExercisers() map[string]func() {
	var mu sync.Mutex
	var rw sync.RWMutex
	var wg sync.WaitGroup
	text := "the quick brown fox jumps"
	seed := maphash.MakeSeed()
	n := 7
	return map[string]func(){
		"(*sync.Mutex).*": func() { mu.Lock(); sinkInt = n; mu.Unlock() },
		"(*sync.RWMutex).*": func() {
			rw.RLock()
			sinkInt = n
			rw.RUnlock()
			rw.Lock()
			sinkInt = n
			rw.Unlock()
		},
		"(*sync.WaitGroup).*":     func() { wg.Add(1); wg.Done(); wg.Wait() },
		"hash/maphash.Comparable": func() { sinkInt = int(maphash.Comparable(seed, text)) },
	}
}

// TestAllowlistedFunctionsAllocateNothing parses the allowlist, and calls
// every entry whose kind a test vouches for, measured or amortized, counting
// its allocations: a release that makes one allocate, or an entry of those
// kinds with nothing here that calls it, fails.
func TestAllowlistedFunctionsAllocateNothing(t *testing.T) {
	a, err := parseAllow(strings.Split(Allowlist, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	cases := allowExercisers()
	for name, kind := range a.assumed {
		if kind != "measured" && kind != "amortized" {
			continue
		}
		run, ok := cases[name]
		if !ok {
			t.Errorf("%s is allowlisted as %s with nothing that calls it; add a case to allowExercisers", name, kind)
			continue
		}
		alloctest.Zero(t, name+", which is allowlisted,", run)
	}
	for name := range cases {
		if _, ok := a.assumed[name]; !ok {
			t.Errorf("allowExercisers has a case for %s, which allow.txt does not list", name)
		}
	}
}

// TestAnUnlabeledAllowlistLineIsAnError checks that an allowlist line must
// say why its function allocates nothing, since what a function allocates is
// otherwise worked out from its code.
func TestAnUnlabeledAllowlistLineIsAnError(t *testing.T) {
	for _, l := range []string{"strings.Index", "append: strconv.AppendInt", "cache: (*sync.Map).Load", "amortized:"} {
		if _, err := parseAllow([]string{l}); err == nil {
			t.Errorf("%q parses", l)
		}
	}
	if _, err := parseAllow([]string{"amortized: (*sync.Mutex).* # the sudog cache", "debug: runtime.print*"}); err != nil {
		t.Error(err)
	}
}

// boundedModule is this repository's bounded package, which a caller checks
// from source by listing it in SourceModules.
const boundedModule = "github.com/theconsensuslabs/noalloc/bounded"

// TestEscaperModulesAllocateNothing checks html/template and net/url from
// source, with each call out of them judged by what the called code
// allocates rather than by the allowlist: each append function they mark
// allocates only to grow its destination, and neither holds an exemption.
func TestEscaperModulesAllocateNothing(t *testing.T) {
	for _, mod := range []struct{ dir, path string }{
		{"html/template", "github.com/theconsensuslabs/noalloc/html/template"},
		{"net/url", "github.com/theconsensuslabs/noalloc/net/url"},
	} {
		dir, err := filepath.Abs(mod.dir)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: mod.path, Root: dir,
			SummaryCache: summaryCacheDir})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Roots) == 0 {
			t.Errorf("%s: no root was checked", mod.dir)
		}
		for _, f := range res.Findings {
			t.Errorf("%s allocates: %s", mod.dir, f)
		}
		for _, e := range res.Exemptions {
			t.Errorf("%s holds an exemption: %s", mod.dir, e.Key())
		}
	}
}

// TestBoundedAllocatesNothing checks bounded from source, every root it
// marks: none allocates outside the setup and responses it exempts.
func TestBoundedAllocatesNothing(t *testing.T) {
	dir, err := filepath.Abs("bounded")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: boundedModule, Root: dir,
		Allow: strings.Split(Allowlist, "\n")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Roots) == 0 {
		t.Fatal("no root of bounded was checked")
	}
	for _, f := range res.Findings {
		t.Errorf("bounded allocates: %s", f)
	}
}
