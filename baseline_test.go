package noalloc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// finding is one allocation of fn on its own line, in group.
func finding(group, fn, reason, source string) Finding {
	return Finding{File: "a.go", Line: 1, Source: source, Func: fn, Group: group, Reasons: []string{reason}}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestARatchetOnlyShrinks runs a ratchet over two baselines through a change:
// a line still allocating, one fixed, one new, and one that another group's
// roots now reach. Prune refuses while the new one is unlisted, then drops the
// fixed line and moves the other, after which the lists hold exactly what is
// found. It runs with and without an owner column.
func TestARatchetOnlyShrinks(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "no owner", true: "owner"}[owned], func(t *testing.T) {
			dir := t.TempDir()
			r := Ratchet{Baselines: []Baseline{{"a", filepath.Join(dir, "a.txt")}, {"b", filepath.Join(dir, "b.txt")}},
				Header: "# header\n"}
			prefix := ""
			if owned {
				r.Owner = func(string) string { return "found owner" }
				prefix = "listed owner | "
			}
			writeFile(t, r.Baselines[0].File, "# header\n"+prefix+"a.go | f | map write | m[k]=v\n"+prefix+"a.go | f | go statement | go g()\n")
			writeFile(t, r.Baselines[1].File, "# header\n"+prefix+"a.go | h | make chan | make(chan int)\n")
			res := &Result{Findings: []Finding{
				finding("a", "f", "map write", "m[k]=v"),
				finding("a", "h", "make chan", "make(chan int)"),
				finding("a", "n", "string concatenation", "s+t"),
				finding("", "ungrouped", "map write", "x[k]=v"),
			}}
			d, err := r.Compare(res)
			if err != nil {
				t.Fatal(err)
			}
			a, b := d.Baselines[0], d.Baselines[1]
			if len(a.Added) != 1 || a.Added[0].Func != "n" || len(a.Stale) != 1 || a.Stale[0].Func != "f" ||
				a.Stale[0].Reason != "go statement" || len(b.Added) != 0 || len(b.Stale) != 0 {
				t.Fatalf("a added %v stale %v, b added %v stale %v; want n added and f's go statement stale in a", a.Added, a.Stale, b.Added, b.Stale)
			}
			if len(d.Moves) != 1 || d.Moves[0].From != 1 || d.Moves[0].To != 0 || d.Moves[0].Line.Func != "h" {
				t.Fatalf("moves %+v, want h from b to a", d.Moves)
			}
			if owned && (a.Added[0].Owner != "found owner" || d.Moves[0].Line.Owner != "listed owner") {
				t.Errorf("owners %q and %q, want a new line owned as found and a move as listed", a.Added[0].Owner, d.Moves[0].Line.Owner)
			}
			if d.Clean() {
				t.Error("Clean with a line added, one stale, and one moved")
			}
			if err := r.Prune(d); err == nil {
				t.Fatal("Prune wrote the baselines with an allocation in none of them")
			}

			res.Findings = slices.DeleteFunc(res.Findings, func(f Finding) bool { return f.Func == "n" })
			if d, err = r.Compare(res); err != nil {
				t.Fatal(err)
			}
			if err := r.Prune(d); err != nil {
				t.Fatal(err)
			}
			want := "# header\n" + prefix + "a.go | f | map write | m[k]=v\n" + prefix + "a.go | h | make chan | make(chan int)\n"
			if got := readFile(t, r.Baselines[0].File); got != want {
				t.Errorf("a.txt after Prune:\n%s\nwant:\n%s", got, want)
			}
			if got := readFile(t, r.Baselines[1].File); got != "# header\n" {
				t.Errorf("b.txt after Prune:\n%s\nwant the header alone", got)
			}
			if d, err = r.Compare(res); err != nil {
				t.Fatal(err)
			}
			if !d.Clean() {
				t.Errorf("not Clean after Prune: %+v", d)
			}
		})
	}
}

// A line whose text changed, with its function, reason, and count the same,
// is not drift: the ratchet holds counts, and the text is for the reader.
// Another line of the same reason in the same function still is.
func TestARewordedLineIsNotDrift(t *testing.T) {
	r := Ratchet{Baselines: []Baseline{{"a", filepath.Join(t.TempDir(), "a.txt")}}}
	writeFile(t, r.Baselines[0].File, "a.go | f | map write | h.Set(k,v)\n")
	d, err := r.Compare(&Result{Findings: []Finding{finding("a", "f", "map write", "h[k]=v")}})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Clean() || len(d.Baselines[0].Restated) != 1 {
		t.Errorf("a reworded line drifted: added %v, stale %v, restated %v", d.Baselines[0].Added, d.Baselines[0].Stale,
			d.Baselines[0].Restated)
	}
	d, err = r.Compare(&Result{Findings: []Finding{finding("a", "f", "map write", "h[k]=v"),
		finding("a", "f", "map write", "g[k]=v")}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Clean() || len(d.Baselines[0].Added) != 1 {
		t.Errorf("a second map write in f is not drift: added %v", d.Baselines[0].Added)
	}
}

// TestARatchetRestatesAStatementWhoseTextChanged checks that Restate gives a
// listed line the new text of its statement, after which nothing drifts.
func TestARatchetRestatesAStatementWhoseTextChanged(t *testing.T) {
	r := Ratchet{Baselines: []Baseline{{"a", filepath.Join(t.TempDir(), "a.txt")}}}
	writeFile(t, r.Baselines[0].File, "a.go | f | map write | h.Set(k,v)\n")
	res := &Result{Findings: []Finding{finding("a", "f", "map write", "h[k]=v")}}
	d, err := r.Compare(res)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Restate(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Old.Source != "h.Set(k,v)" || got[0].New.Source != "h[k]=v" {
		t.Fatalf("restated %+v, want h.Set(k,v) as h[k]=v", got)
	}
	if d, err = r.Compare(res); err != nil {
		t.Fatal(err)
	}
	if !d.Clean() {
		t.Errorf("not Clean after Restate: %+v", d)
	}
}

// TestARatchetNamesAMalformedLine checks that a baseline line with the wrong
// number of fields is an error naming its file and line, with an owner column
// and without one.
func TestARatchetNamesAMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, path, "# header\na.go | f | map write\n")
	for _, r := range []Ratchet{{}, {Owner: func(string) string { return "o" }}} {
		r.Baselines = []Baseline{{"a", path}}
		if _, err := r.Compare(&Result{}); err == nil || !strings.Contains(err.Error(), path+":2:") {
			t.Errorf("Compare = %v, want an error at %s:2", err, path)
		}
	}
}

// TestARatchetHoldsExemptionsToTheirList checks the exemptions list both ways
// and each reason's kind, and that WriteExemptions brings the list level.
func TestARatchetHoldsExemptionsToTheirList(t *testing.T) {
	r := Ratchet{Exemptions: filepath.Join(t.TempDir(), "exemptions.txt"), ExemptionsHeader: "# exemptions\n",
		Kinds: []string{"cursor"}}
	writeFile(t, r.Exemptions, "# exemptions\na.go: f: cursor: kept\na.go: g: cursor: removed\n")
	res := &Result{Exemptions: []Exemption{
		{File: "a.go", Func: "f", Reason: "cursor: kept"},
		{File: "a.go", Func: "h", Reason: "response: new"},
	}}
	d, err := r.Compare(res)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Unlisted, []string{"a.go: h: response: new"}) || !slices.Equal(d.Gone, []string{"a.go: g: cursor: removed"}) ||
		len(d.Unkinded) != 1 || d.Unkinded[0].Func != "h" {
		t.Fatalf("unlisted %q, gone %q, unkinded %v; want h unlisted and of no kind, g gone", d.Unlisted, d.Gone, d.Unkinded)
	}
	if err := r.WriteExemptions(res); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, r.Exemptions), "# exemptions\na.go: f: cursor: kept\na.go: h: response: new\n"; got != want {
		t.Errorf("list after WriteExemptions:\n%s\nwant:\n%s", got, want)
	}
	r.Kinds = nil
	if d, err = r.Compare(res); err != nil {
		t.Fatal(err)
	}
	if !d.Clean() {
		t.Errorf("not Clean after WriteExemptions with any kind allowed: %+v", d)
	}
}
