package noalloc

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Line is one reason a line of a function allocates, as a baseline lists it.
// Its key is its file, function, reason, and the line's tokens, never the
// line's number or an SSA register, so edits elsewhere in a file leave it
// alone. A line whose key repeats in one function is listed once for each time
// it does. A baseline holds a function to how many of its lines allocate for
// each reason; the tokens say which lines, for whoever reads it, and a line
// whose tokens changed is a Restatement rather than drift.
type Line struct {
	Owner, File, Func, Reason, Source string
	// Number is the line Check found it on. A baseline does not record it.
	Number int
}

// Key is the line as a baseline matches it.
func (l Line) Key() string { return l.File + " | " + l.Func + " | " + l.Reason + " | " + l.Source }

// String is the line as a baseline lists it.
func (l Line) String() string {
	if l.Owner == "" {
		return l.Key()
	}
	return l.Owner + " | " + l.Key()
}

// Baseline is the file listing the allocations a group's roots reach that are
// not fixed yet.
type Baseline struct {
	Group, File string
}

// Ratchet holds what Check finds to checked-in lists, so that allocations are
// only ever removed: each one found must be in its group's baseline, each line
// of a baseline must still allocate, and each exemption must be in the
// exemptions list.
type Ratchet struct {
	// Baselines lists each group's file. A finding in a group with none here
	// is not held to one.
	Baselines []Baseline
	// Owner names who owns a file, never empty, written as the first field of
	// each line; nil writes lines without one.
	Owner func(file string) string
	// Name rewrites a function's SSA name as the lines list it; nil keeps it.
	Name func(fn string) string
	// Header is written atop each baseline.
	Header string
	// Exemptions is the file listing each exemption by its Key; empty holds
	// exemptions to no list.
	Exemptions       string
	ExemptionsHeader string
	// Kinds are the words an exemption's reason must start with, before a
	// colon; nil allows any reason.
	Kinds []string
}

// Drift is how what Check found differs from the checked-in lists.
type Drift struct {
	// Baselines holds each baseline's difference, in Ratchet.Baselines order.
	Baselines []BaselineDrift
	// Moves are lines that left one baseline and turned up in another, because
	// another group's roots now reach them first. A move is in neither
	// baseline's Added or Stale.
	Moves []Move
	// Unlisted are exemptions the list lacks, Gone listed ones the tree no
	// longer has, each as its Entry, so an exemption whose coverage changed
	// is in both; Unkinded the exemptions whose reason starts with none of
	// Kinds; and Empty those of the module a root reaches that cover nothing.
	Unlisted, Gone []string
	Unkinded       []Exemption
	Empty          []Exemption
}

// BaselineDrift is how one baseline differs from what Check found.
type BaselineDrift struct {
	File string
	// Listed is what the file holds, Added what was found and is not listed,
	// and Stale what is listed and was not found. Restated are found lines
	// paired with listed ones of the same function and reason whose text
	// differs, which are in neither Added nor Stale.
	Listed, Added, Stale []Line
	Restated             []Restatement
}

// Move is a line that left Drift.Baselines[From] for Drift.Baselines[To],
// keeping its owner.
type Move struct {
	Line     Line
	From, To int
}

// Restatement is a line whose statement's text changed: the same function and
// reason, Old as the baseline lists it and New as Check found it.
type Restatement struct {
	File     string
	Old, New Line
}

// Clean reports whether the lists hold exactly what Check found.
func (d *Drift) Clean() bool {
	for _, b := range d.Baselines {
		if len(b.Added) > 0 || len(b.Stale) > 0 {
			return false
		}
	}
	return len(d.Moves) == 0 && len(d.Unlisted) == 0 && len(d.Gone) == 0 && len(d.Unkinded) == 0 && len(d.Empty) == 0
}

// Lines are the findings of group's roots, a line for each reason.
func (r *Ratchet) Lines(res *Result, group string) []Line {
	var out []Line
	for _, f := range res.Findings {
		if f.Group != group {
			continue
		}
		fn := f.Func
		if r.Name != nil {
			fn = r.Name(fn)
		}
		for _, reason := range f.Reasons {
			l := Line{File: f.File, Func: fn, Reason: reason, Source: f.Source, Number: f.Line}
			if r.Owner != nil {
				l.Owner = r.Owner(f.File)
			}
			out = append(out, l)
		}
	}
	return out
}

// Compare reads the lists and returns how res differs from them.
func (r *Ratchet) Compare(res *Result) (*Drift, error) {
	d := &Drift{Baselines: make([]BaselineDrift, len(r.Baselines))}
	added, stale := make([][]Line, len(r.Baselines)), make([][]Line, len(r.Baselines))
	for i, b := range r.Baselines {
		listed, err := r.readBaseline(b.File)
		if err != nil {
			return nil, err
		}
		d.Baselines[i] = BaselineDrift{File: b.File, Listed: listed}
		added[i], stale[i] = diffLines(r.Lines(res, b.Group), listed)
	}
	d.Moves = moves(added, stale)
	for i := range d.Baselines {
		for _, p := range restated(&added[i], &stale[i]) {
			d.Baselines[i].Restated = append(d.Baselines[i].Restated, Restatement{File: d.Baselines[i].File, Old: p[0], New: p[1]})
		}
		d.Baselines[i].Added, d.Baselines[i].Stale = added[i], stale[i]
	}

	var keys []string
	for _, e := range res.Exemptions {
		keys = append(keys, e.Entry())
		if kind, _, ok := strings.Cut(e.Reason, ":"); r.Kinds != nil && (!ok || !slices.Contains(r.Kinds, kind)) {
			d.Unkinded = append(d.Unkinded, e)
		}
		if e.Reached && len(e.Covers) == 0 && !e.InSource {
			d.Empty = append(d.Empty, e)
		}
	}
	if r.Exemptions == "" {
		return d, nil
	}
	b, err := os.ReadFile(r.Exemptions)
	if err != nil {
		return nil, err
	}
	// An entry is a line and the indented lines under it.
	var listed []string
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case l == "" || strings.HasPrefix(l, "#"):
		case strings.HasPrefix(l, "\t") && len(listed) > 0:
			listed[len(listed)-1] += "\n" + l
		default:
			listed = append(listed, l)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if i := slices.Index(listed, k); i >= 0 {
			listed = slices.Delete(listed, i, i+1)
		} else {
			d.Unlisted = append(d.Unlisted, k)
		}
	}
	d.Gone = listed
	return d, nil
}

// Prune rewrites each baseline without its stale lines and with its moves
// applied. It refuses while anything was added, which says the tree is
// mid-change rather than fixed.
func (r *Ratchet) Prune(d *Drift) error {
	for _, b := range d.Baselines {
		if len(b.Added) > 0 {
			return errors.New("an allocation is in no baseline, so the tree is mid-change or unfixed")
		}
	}
	for i, b := range d.Baselines {
		kept, _ := diffLines(b.Listed, b.Stale)
		for _, m := range d.Moves {
			if m.From == i {
				kept, _ = diffLines(kept, []Line{m.Line})
			}
			if m.To == i {
				kept = append(kept, m.Line)
			}
		}
		if err := r.writeBaseline(b.File, kept); err != nil {
			return err
		}
	}
	return nil
}

// Restate rewrites each baseline with the new text of its Restated lines,
// and returns them. A pairing is a guess, so what it pairs needs review.
func (r *Ratchet) Restate(d *Drift) ([]Restatement, error) {
	var out []Restatement
	for i := range d.Baselines {
		b := &d.Baselines[i]
		kept := slices.Clone(b.Listed)
		for _, p := range b.Restated {
			out = append(out, p)
			j := slices.IndexFunc(kept, func(l Line) bool { return l.Key() == p.Old.Key() })
			kept[j].Source = p.New.Source
		}
		b.Restated = nil
		if err := r.writeBaseline(b.File, kept); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// WriteExemptions rewrites the exemptions list from res, each exemption with
// what it covers.
func (r *Ratchet) WriteExemptions(res *Result) error {
	var keys []string
	for _, e := range res.Exemptions {
		keys = append(keys, e.Entry())
	}
	slices.Sort(keys)
	body := r.ExemptionsHeader + strings.Join(keys, "\n")
	if len(keys) > 0 {
		body += "\n"
	}
	return os.WriteFile(r.Exemptions, []byte(body), 0o644)
}

func (r *Ratchet) readBaseline(path string) ([]Line, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fields, want := 4, "file | function | reason | source"
	if r.Owner != nil {
		fields, want = 5, "owner | "+want
	}
	var out []Line
	for i, l := range strings.Split(string(b), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.SplitN(l, " | ", fields)
		if len(f) != fields {
			return nil, fmt.Errorf("%s:%d: want %s, got %q", path, i+1, want, l)
		}
		if r.Owner == nil {
			f = append([]string{""}, f...)
		}
		out = append(out, Line{Owner: f[0], File: f[1], Func: f[2], Reason: f[3], Source: f[4]})
	}
	return out, nil
}

func (r *Ratchet) writeBaseline(path string, lines []Line) error {
	lines = slices.Clone(lines)
	slices.SortFunc(lines, func(a, b Line) int {
		return cmp.Or(cmp.Compare(a.Owner, b.Owner), cmp.Compare(a.File, b.File), cmp.Compare(a.Func, b.Func),
			cmp.Compare(a.Reason, b.Reason), cmp.Compare(a.Source, b.Source))
	})
	var b strings.Builder
	b.WriteString(r.Header)
	for _, l := range lines {
		b.WriteString(l.String() + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// diffLines returns the found lines the baseline doesn't hold and the
// baseline's lines nothing found matches, counting repeats.
func diffLines(found, base []Line) (added, stale []Line) {
	left := map[string]int{}
	for _, l := range base {
		left[l.Key()]++
	}
	for _, l := range found {
		if left[l.Key()] > 0 {
			left[l.Key()]--
		} else {
			added = append(added, l)
		}
	}
	for _, l := range base {
		if left[l.Key()] > 0 {
			left[l.Key()]--
			stale = append(stale, l)
		}
	}
	return added, stale
}

// moves takes out of each baseline's added and stale lines the ones that are
// one line changing baseline, keeping its owner, and returns them.
func moves(added, stale [][]Line) []Move {
	var out []Move
	for to := range added {
		var still []Line
		for _, l := range added[to] {
			moved := false
			for from := range stale {
				if from == to {
					continue
				}
				if j := slices.IndexFunc(stale[from], func(s Line) bool { return s.Key() == l.Key() }); j >= 0 {
					l.Owner = stale[from][j].Owner
					stale[from] = slices.Delete(stale[from], j, j+1)
					out = append(out, Move{l, from, to})
					moved = true
					break
				}
			}
			if !moved {
				still = append(still, l)
			}
		}
		added[to] = still
	}
	return out
}

// restated pairs each new line with a stale line of the same function and
// reason: the same allocation in a statement whose text changed. It takes the
// pairs out of added and stale and returns them, old line first.
func restated(added, stale *[]Line) [][2]Line {
	var pairs [][2]Line
	var still []Line
	for _, l := range *added {
		j := slices.IndexFunc(*stale, func(s Line) bool { return s.File == l.File && s.Func == l.Func && s.Reason == l.Reason })
		if j < 0 {
			still = append(still, l)
			continue
		}
		old := (*stale)[j]
		*stale = slices.Delete(*stale, j, j+1)
		pairs = append(pairs, [2]Line{old, l})
	}
	*added = still
	return pairs
}
