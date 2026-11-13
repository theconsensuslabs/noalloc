package noalloc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// launchModule is a module whose root calls into n packages of a module it
// depends on, each with a conversion whose compiled code a check reads from the
// package's archive.
func launchModule(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	var imports, calls strings.Builder
	for i := range n {
		fmt.Fprintf(&imports, "\t\"dep/p%d\"\n", i)
		fmt.Fprintf(&calls, " + p%d.F(b, m)", i)
		writeConversionFile(t, filepath.Join(dir, "dep", fmt.Sprintf("p%d", i)), "p.go",
			fmt.Sprintf("package p%d\n\n//go:noinline\nfunc F(b []byte, m map[string]int) int { return m[string(b)] + %d }\n", i, i))
	}
	writeConversionFile(t, dir, "go.mod", "module callers\n\ngo 1.27\n\nrequire dep v0.0.0\n\nreplace dep => ./dep\n")
	writeConversionFile(t, filepath.Join(dir, "dep"), "go.mod", "module dep\n\ngo 1.27\n")
	writeConversionFile(t, dir, "callers.go", "package callers\n\nimport (\n"+imports.String()+")\n\n//noalloc:root\nfunc R(b []byte, m map[string]int) int { return 0"+calls.String()+" }\n")
	return dir
}

// goShim puts a go command on PATH that logs each run to the file it returns, as
// "go <args>", and hands out, for go tool -n, a wrapper of the tool that logs
// each of its runs as "tool <name> <args>".
func goShim(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a shell script")
	}
	real, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	shim := fmt.Sprintf(`#!/bin/sh
echo "go $*" >> %[1]q
if [ "$1" = tool ] && [ "$2" = -n ]; then
	p=$(%[2]q tool -n "$3") || exit $?
	w=%[3]q/tool-"$3"
	printf '#!/bin/sh\necho "tool %%s $*" >> %[1]s\nexec %%s "$@"\n' "$3" "$p" > "$w.$$"
	chmod +x "$w.$$" && mv "$w.$$" "$w" && echo "$w"
	exit
fi
exec %[2]q "$@"
`, log, real, dir)
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// TestGoLaunchesStayFlatAsTheArchivesReadGrow counts the processes a cold check
// starts over a dependency of 2 and of 8 packages. The tools that read archives
// run once or more per archive, which is the control that more archives are
// read; the go command, which costs a start of its own each time, runs as
// often for 8 as for 2. Each check starts as a new process would, with no tool
// resolved.
func TestGoLaunchesStayFlatAsTheArchivesReadGrow(t *testing.T) {
	log := goShim(t)
	type count struct{ archives, gos, tools int }
	check := func(n int) count {
		dir := launchModule(t, n)
		os.Remove(log)
		toolBinaries.Clear()
		read := map[string]bool{}
		testHookDisassemble = func(a string) error {
			read[a] = true
			return nil
		}
		defer func() { testHookDisassemble = nil }()
		_, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Root: dir,
			Env: append(os.Environ(), "GOFLAGS=", "GOWORK=off"), Allow: strings.Split(Allowlist, "\n"), SummaryCache: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		c := count{archives: len(read)}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.HasPrefix(l, "tool ") {
				c.tools++
			} else {
				c.gos++
			}
		}
		t.Logf("%d packages: %d archives read, %d go runs, %d tool runs", n, c.archives, c.gos, c.tools)
		return c
	}
	small, large := check(2), check(8)
	if large.archives < small.archives+6 {
		t.Fatalf("control: %d archives read for 8 packages, %d for 2", large.archives, small.archives)
	}
	if large.gos != small.gos {
		t.Errorf("the go command ran %d times for 8 packages, %d for 2", large.gos, small.gos)
	}
}

// cacheFixture is a module depending on two packages, a and b, and the archives
// of its build.
func cacheFixture(t *testing.T, aBody string) (dir string, env []string, archives map[string]string) {
	t.Helper()
	dir = t.TempDir()
	writeConversionFile(t, dir, "go.mod", "module callers\n\ngo 1.27\n\nrequire dep v0.0.0\n\nreplace dep => ./dep\n")
	writeConversionFile(t, filepath.Join(dir, "dep"), "go.mod", "module dep\n\ngo 1.27\n")
	writeConversionFile(t, filepath.Join(dir, "dep", "b"), "b.go", "package b\n\nfunc B(b []byte, m map[string]int) int { return m[string(b)] }\n")
	writeConversionFile(t, dir, "callers.go", "package callers\n\nimport (\n\t\"dep/a\"\n\t\"dep/b\"\n)\n\nvar _, _ = a.A, b.B\n")
	env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	return dir, env, cacheArchives(t, dir, env, aBody)
}

// cacheArchives writes package a of the fixture at dir with body and builds it.
func cacheArchives(t *testing.T, dir string, env []string, body string) map[string]string {
	t.Helper()
	writeConversionFile(t, filepath.Join(dir, "dep", "a"), "a.go", "package a\n\n"+body)
	pkgs, err := buildPackages(Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Env: env, Root: dir}, env)
	if err != nil {
		t.Fatal(err)
	}
	archives := map[string]string{}
	for _, p := range pkgs {
		archives[p.path] = p.archive
	}
	return archives
}

// TestTheArchiveCacheMissesOnlyWhenTheReadingCanDiffer reads two archives through
// the archive cache, check after check, and counts the disassemblies: none once
// both are cached, one when one archive's contents change, both when the rules
// they are read by change, and one when an entry is cut short.
func TestTheArchiveCacheMissesOnlyWhenTheReadingCanDiffer(t *testing.T) {
	dir, env, archives := cacheFixture(t, "func A(s string) []byte { return []byte(s) }\n")
	cache := t.TempDir()
	read := func(archives map[string]string) (map[string]map[string]textSym, []string) {
		var disassembled []string
		testHookDisassemble = func(a string) error {
			for p, pa := range archives {
				if pa == a {
					disassembled = append(disassembled, p)
				}
			}
			return nil
		}
		defer func() { testHookDisassemble = nil }()
		m := newMachineCode(env, archives)
		m.cache = cache
		m.code = newCodeReader()
		defer m.code.spill.close()
		for _, p := range []string{"dep/a", "dep/b"} {
			m.symbols(p)
		}
		if m.err != nil {
			t.Fatal(m.err)
		}
		slices.Sort(disassembled)
		return m.text, disassembled
	}
	expect := func(step string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s: disassembled %q, want %q", step, got, want)
		}
	}

	first, got := read(archives)
	expect("an empty cache", got, []string{"dep/a", "dep/b"})
	again, got := read(archives)
	expect("the same archives", got, nil)
	if !reflect.DeepEqual(first, again) {
		t.Errorf("the cache read back\n%v\nwhere the archives held\n%v", again, first)
	}

	changed := cacheArchives(t, dir, env, "func A(s string) []byte { return append([]byte(s), 1) }\n")
	if changed["dep/a"] == archives["dep/a"] {
		t.Fatal("control: a's archive did not change")
	}
	_, got = read(changed)
	expect("a changed", got, []string{"dep/a"})

	rules := machineRules
	machineRules = func() string { return "other rules" }
	_, got = read(changed)
	machineRules = rules
	expect("other rules", got, []string{"dep/a", "dep/b"})

	entries, err := filepath.Glob(filepath.Join(cache, "*.code"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no entries: %v", err)
	}
	m := newMachineCode(env, changed)
	m.cache = cache
	e := m.archiveCache("dep/b", changed["dep/b"])
	b, err := os.ReadFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.file, b[:len(b)-len("E\n")], 0o644); err != nil {
		t.Fatal(err)
	}
	cut, got := read(changed)
	expect("b's entry cut short", got, []string{"dep/b"})
	if !reflect.DeepEqual(cut["dep/b"], first["dep/b"]) {
		t.Errorf("after an entry cut short, b reads as\n%v\nwant\n%v", cut["dep/b"], first["dep/b"])
	}
}

// TestACachedReadingIsTheReadingOfTheArchive reads every archive of a build twice,
// once disassembled and once replayed from the archive cache, and requires the
// same of each: its functions, those that never return, and what the code reader
// holds after it.
func TestACachedReadingIsTheReadingOfTheArchive(t *testing.T) {
	dir := compiledCodeModule(t)
	env := append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	pkgs, err := buildPackages(Config{Dir: dir, Patterns: []string{"."}, Module: "callers", Env: env, Root: dir}, env)
	if err != nil {
		t.Fatal(err)
	}
	archives := map[string]string{}
	for _, p := range pkgs {
		archives[p.path] = p.archive
	}
	cache := t.TempDir()
	disassembled := 0
	testHookDisassemble = func(string) error {
		disassembled++
		return nil
	}
	defer func() { testHookDisassemble = nil }()
	type reading struct {
		text     map[string]map[string]textSym
		noReturn map[string]bool
		index    codeIndex
		calls    []string
	}
	read := func(cache string, path string) reading {
		m := newMachineCode(env, archives)
		m.cache = cache
		m.code = newCodeReader()
		defer m.code.spill.close()
		m.symbols(path)
		if m.err != nil {
			t.Fatalf("%s: %v", path, m.err)
		}
		r := reading{text: m.text, noReturn: m.noReturn, index: m.code.index}
		if err := m.code.spill.each(func(f []string) { r.calls = append(r.calls, strings.Join(f, " ")) }); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, p := range pkgs {
		if p.archive == "" {
			continue
		}
		fresh := read("", p.path)
		read(cache, p.path)
		before := disassembled
		cached := read(cache, p.path)
		if disassembled != before {
			t.Errorf("%s: the cached reading disassembled the archive", p.path)
		}
		if !reflect.DeepEqual(fresh.text, cached.text) {
			t.Errorf("%s: functions differ", p.path)
		}
		if !reflect.DeepEqual(fresh.noReturn, cached.noReturn) {
			t.Errorf("%s: functions that never return %v, cached %v", p.path, fresh.noReturn, cached.noReturn)
		}
		if !slices.Equal(fresh.index, cached.index) {
			t.Errorf("%s: the code index differs", p.path)
		}
		if !slices.Equal(fresh.calls, cached.calls) {
			t.Errorf("%s: %d calls to the runtime, cached %d", p.path, len(fresh.calls), len(cached.calls))
		}
	}
}

// TestPruningKeepsTheEntriesUsedLast fills a cache directory past its limit and
// requires a prune to leave the entries used last within it, and to remove an
// entry left unfinished long ago.
func TestPruningKeepsTheEntriesUsedLast(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, size int, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 10 {
		write(fmt.Sprintf("%d.code", i), 100, time.Duration(i)*time.Minute)
	}
	write("old.x.tmp", 100, 2*time.Hour)
	write("new.x.tmp", 100, 0)
	pruneArchiveCache(dir, 450)
	var left []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{"0.code", "1.code", "2.code", "3.code", "new.x.tmp"}
	if !slices.Equal(left, want) {
		t.Errorf("left %q, want %q", left, want)
	}
}

// TestAnArchiveIsDisassembledOncePerCheck reads a package's lines and then its
// functions, as a check does when it judges a conversion in a package before a
// symbol of it, and requires one disassembly of its archive for both.
func TestAnArchiveIsDisassembledOncePerCheck(t *testing.T) {
	_, env, archives := cacheFixture(t, "func A(s string) []byte { return []byte(s) }\n")
	disassembled := 0
	testHookDisassemble = func(string) error {
		disassembled++
		return nil
	}
	defer func() { testHookDisassemble = nil }()
	m := newMachineCode(env, archives)
	m.code = newCodeReader()
	defer m.code.spill.close()
	m.lines("dep/b")
	if len(m.symbols("dep/b")) == 0 {
		t.Fatal("control: b has no functions")
	}
	if m.err != nil {
		t.Fatal(m.err)
	}
	if disassembled != 1 {
		t.Errorf("b's archive was disassembled %d times", disassembled)
	}
}
