package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture copies the package's fixture module into a new directory.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"go.mod", "fixture.go"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixture", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runIn runs the command in dir and returns its exit code and output.
func runIn(t *testing.T, dir string, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-C", dir, "-scratch", "fixture.Scratch"}, args...), &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func TestWithoutABaselineEveryAllocationFails(t *testing.T) {
	code, out := runIn(t, fixture(t))
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		// The cross-check's summary of what it did not compare.
		if strings.HasPrefix(l, "cross-check: ") {
			continue
		}
		if !strings.HasPrefix(l, "fixture.go:") {
			t.Errorf("a line that names no allocation: %q", l)
		}
	}
}

func TestABaselineOnlyShrinks(t *testing.T) {
	dir := fixture(t)
	base := filepath.Join(dir, "baseline.txt")
	if code, out := runIn(t, dir, "-baseline", "baseline.txt"); code != 2 || !strings.Contains(out, "-start") {
		t.Fatalf("with no baseline yet: exit %d, want 2 and a hint to -start:\n%s", code, out)
	}
	if code, out := runIn(t, dir, "-baseline", "baseline.txt", "-start"); code != 0 {
		t.Fatalf("-start: exit %d:\n%s", code, out)
	}
	if code, out := runIn(t, dir, "-baseline", "baseline.txt", "-start"); code != 2 {
		t.Fatalf("-start over an existing baseline: exit %d, want 2:\n%s", code, out)
	}
	if code, out := runIn(t, dir, "-baseline", "baseline.txt"); code != 0 {
		t.Fatalf("after -start: exit %d, want 0:\n%s", code, out)
	}
	started, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}

	// A line nothing allocates on any more fails until -prune deletes it.
	fixed := "fixture.go | fixture.gone | map write | m[k]=v\n"
	if err := os.WriteFile(base, append(bytes.Clone(started), fixed...), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runIn(t, dir, "-baseline", "baseline.txt"); code != 1 || !strings.Contains(out, "no longer allocates: "+strings.TrimSpace(fixed)) {
		t.Fatalf("with a fixed line listed: exit %d, want 1 naming it:\n%s", code, out)
	}
	if code, out := runIn(t, dir, "-baseline", "baseline.txt", "-prune"); code != 0 {
		t.Fatalf("-prune: exit %d:\n%s", code, out)
	}
	if b, _ := os.ReadFile(base); !bytes.Equal(b, started) {
		t.Fatalf("-prune left:\n%s\nwant what -start wrote:\n%s", b, started)
	}

	// An allocation the baseline lacks fails, and -prune leaves the baseline alone.
	lines := strings.SplitAfter(string(started), "\n")
	dropped := lines[len(lines)-2]
	without := strings.Join(lines[:len(lines)-2], "")
	if err := os.WriteFile(base, []byte(without), 0o644); err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(dropped, " | ")
	want := fields[1] + ": " + fields[2]
	if code, out := runIn(t, dir, "-baseline", "baseline.txt"); code != 1 || !strings.Contains(out, want) {
		t.Fatalf("with %q unlisted: exit %d, want 1 naming %q:\n%s", dropped, code, want, out)
	}
	if code, out := runIn(t, dir, "-baseline", "baseline.txt", "-prune"); code != 1 {
		t.Fatalf("-prune with an allocation unlisted: exit %d, want 1:\n%s", code, out)
	}
	if b, _ := os.ReadFile(base); string(b) != without {
		t.Errorf("-prune with an allocation unlisted rewrote the baseline:\n%s", b)
	}
}

func TestExemptionsAreHeldToTheirList(t *testing.T) {
	dir := fixture(t)
	if code, out := runIn(t, dir, "-baseline", "b.txt", "-start"); code != 0 {
		t.Fatalf("-start: exit %d:\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "e.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runIn(t, dir, "-baseline", "b.txt", "-exemptions", "e.txt")
	if code != 1 || !strings.Contains(out, "e.txt: missing: fixture.go: exempt: a trailing exemption") {
		t.Fatalf("with an empty list: exit %d, want 1 naming each exemption:\n%s", code, out)
	}
	if code, out := runIn(t, dir, "-baseline", "b.txt", "-exemptions", "e.txt", "-write-exemptions"); code != 0 {
		t.Fatalf("-write-exemptions: exit %d:\n%s", code, out)
	}
	if code, out := runIn(t, dir, "-baseline", "b.txt", "-exemptions", "e.txt"); code != 0 {
		t.Fatalf("after -write-exemptions: exit %d, want 0:\n%s", code, out)
	}
	if code, out := runIn(t, dir, "-baseline", "b.txt", "-kinds", "cursor"); code != 1 || !strings.Contains(out, "must start with one of cursor") {
		t.Fatalf("with -kinds the fixture's reasons don't use: exit %d, want 1:\n%s", code, out)
	}
}

func TestMisusedFlagsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"-prune"},
		{"-start"},
		{"-write-exemptions"},
		{"-baseline", "b.txt", "-prune", "-restate"},
		{"-no-such-flag"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 || stderr.Len() == 0 {
			t.Errorf("%q: exit %d, want 2 and a message", args, code)
		}
	}
}

func TestCrossCheckFindsNothingUnexplainedInTheFixture(t *testing.T) {
	dir := fixture(t)
	if code, out := runIn(t, dir, "-baseline", "b.txt", "-line-only", "response", "-start"); code != 0 {
		t.Fatalf("-start: exit %d:\n%s", code, out)
	}
	code, out := runIn(t, dir, "-baseline", "b.txt", "-line-only", "response")
	if code != 0 || strings.Contains(out, "noalloc found nothing") {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
}
