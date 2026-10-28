// Command noalloc reports each line that allocates on the heap in the code
// reachable from functions marked //noalloc:root.
//
// Usage:
//
//	noalloc [flags] [packages]
//
// The packages default to ./... and must be in one module. With -baseline,
// only allocations missing from the baseline fail, and so does a baseline line
// that no longer allocates. See the README for the directives.
package main

import (
	"bytes"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/theconsensuslabs/noalloc"
)

const baselineHeader = `# Allocations noalloc reports that are not fixed yet, as
# file | function | reason | the line's Go tokens.
# Delete the lines you fix, or run noalloc -prune. Never add one.
`

const exemptionsHeader = `# Every //noalloc:exempt comment, as file: function: reason.
# Regenerate with noalloc -write-exemptions.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("noalloc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: noalloc [flags] [packages]")
		fs.PrintDefaults()
	}
	var scratch, allow, source list
	dir := fs.String("C", ".", "run in `dir`, which relative paths are read from")
	fs.Var(&scratch, "scratch", "a `type` whose slice and map fields are reused memory, as pkg/path.Name (repeatable)")
	fs.Var(&allow, "allow", "a `file` of functions outside the module to take to allocate nothing, each as \"kind: function\", added to the built-in list (repeatable)")
	fs.Var(&source, "source", "an import path `prefix` of packages to check from source as the module is, such as noalloc's bounded (repeatable)")
	baseline := fs.String("baseline", "", "hold findings to the allocations listed in `file`")
	exemptions := fs.String("exemptions", "", "hold //noalloc:exempt comments to the list in `file`")
	kinds := fs.String("kinds", "", "comma-separated `words` an exemption's reason must start with, before a colon")
	lineOnly := fs.String("line-only", "", "comma-separated exemption `kinds` that cover their own line but not what it calls")
	start := fs.Bool("start", false, "write the baseline from what is found now; refuses if it exists")
	prune := fs.Bool("prune", false, "delete the baseline lines that no longer allocate")
	restate := fs.Bool("restate", false, "update baseline lines whose statement was reworded")
	writeExemptions := fs.Bool("write-exemptions", false, "rewrite the exemptions list from the code")
	report := fs.Bool("report", false, "print every finding with the call path that reaches it, and every exemption")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*start || *prune || *restate) && *baseline == "" {
		fmt.Fprintln(stderr, "noalloc: -start, -prune, and -restate need -baseline")
		return 2
	}
	if *writeExemptions && *exemptions == "" {
		fmt.Fprintln(stderr, "noalloc: -write-exemptions needs -exemptions")
		return 2
	}
	if btoi(*start)+btoi(*prune)+btoi(*restate) > 1 {
		fmt.Fprintln(stderr, "noalloc: use one of -start, -prune, and -restate at a time")
		return 2
	}
	*baseline, *exemptions = in(*dir, *baseline), in(*dir, *exemptions)
	if *start {
		if _, err := os.Stat(*baseline); err == nil {
			fmt.Fprintf(stderr, "noalloc: %s exists; delete it to start over\n", *baseline)
			return 2
		}
	}

	module, root, err := goModule(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "noalloc: %v\n", err)
		return 2
	}
	allowed := strings.Split(noalloc.Allowlist, "\n")
	for _, f := range allow {
		b, err := os.ReadFile(in(*dir, f))
		if err != nil {
			fmt.Fprintf(stderr, "noalloc: %v\n", err)
			return 2
		}
		allowed = append(allowed, strings.Split(string(b), "\n")...)
	}
	patterns := fs.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	res, err := noalloc.Check(noalloc.Config{Dir: *dir, Patterns: patterns, Module: module, SourceModules: source, Scratch: scratch,
		Allow: allowed, Root: root, LineOnly: split(*lineOnly)})
	if err != nil {
		fmt.Fprintf(stderr, "noalloc: %v\n", err)
		return 2
	}
	if *report {
		fmt.Fprint(stdout, noalloc.Report(res))
	}

	r := noalloc.Ratchet{Header: baselineHeader, Exemptions: *exemptions, ExemptionsHeader: exemptionsHeader, Kinds: split(*kinds)}
	if *start {
		if err := startBaseline(&r, res, *baseline); err != nil {
			fmt.Fprintf(stderr, "noalloc: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "wrote %d allocations to %s\n", len(r.Lines(res, "")), *baseline)
	}
	if *baseline != "" {
		r.Baselines = []noalloc.Baseline{{File: *baseline}}
	}
	d, err := r.Compare(res)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "noalloc: %v; run with -start to write it from what is found now\n", err)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "noalloc: %v\n", err)
		return 2
	}

	failed := false
	if *baseline == "" {
		for _, f := range res.Findings {
			fmt.Fprintln(stdout, f)
			failed = true
		}
	}
	for _, b := range d.Baselines {
		for _, l := range b.Added {
			fmt.Fprintf(stdout, "%s:%d: %s: %s\n", l.File, l.Number, l.Func, l.Reason)
			failed = true
		}
	}
	if *restate {
		got, err := r.Restate(d)
		if err != nil {
			fmt.Fprintf(stderr, "noalloc: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "updated %d lines in %s\n", len(got), *baseline)
	}
	if *prune {
		n := len(d.Baselines[0].Stale)
		if err := r.Prune(d); err != nil {
			fmt.Fprintf(stderr, "noalloc: %s is unchanged: fix or exempt the allocations above first\n", *baseline)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %d lines from %s\n", n, *baseline)
	} else {
		for _, b := range d.Baselines {
			for _, l := range b.Stale {
				fmt.Fprintf(stdout, "%s: no longer allocates: %s\n", b.File, l)
				failed = true
			}
		}
	}
	if !*restate {
		for _, b := range d.Baselines {
			for _, x := range b.Restated {
				fmt.Fprintf(stderr, "%s: %s in %s now reads %q; -restate updates the line\n", b.File, x.Old.Reason, x.Old.Func, x.New.Source)
			}
		}
	}

	for _, e := range d.Unkinded {
		fmt.Fprintf(stdout, "%s:%d: //noalloc:exempt %s: the reason must start with one of %s and a colon\n", e.File, e.Line, e.Reason, *kinds)
		failed = true
	}
	for _, e := range d.Empty {
		fmt.Fprintf(stdout, "%s:%d: //noalloc:exempt %s: exempts nothing; its line and what it calls allocate nothing\n", e.File, e.Line, e.Reason)
		failed = true
	}
	if *writeExemptions {
		if err := r.WriteExemptions(res); err != nil {
			fmt.Fprintf(stderr, "noalloc: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "wrote %d exemptions to %s\n", len(res.Exemptions), *exemptions)
	} else {
		for _, k := range d.Unlisted {
			fmt.Fprintf(stdout, "%s: missing: %s\n", *exemptions, k)
			failed = true
		}
		for _, k := range d.Gone {
			fmt.Fprintf(stdout, "%s: no longer in the code: %s\n", *exemptions, k)
			failed = true
		}
	}
	if x := res.CrossCheck; x != nil {
		for _, c := range x.Calls {
			fmt.Fprintf(stdout, "%s: but noalloc found nothing on the line\n", c)
			failed = true
		}
		for _, u := range x.Unmatched {
			fmt.Fprintf(stdout, "%s: compiled, but noalloc has no function by that name to check it against\n", u)
			failed = true
		}
		if x.Elsewhere > 0 || x.Mixed > 0 || len(x.Buffered) > 0 || len(x.Unsorted) > 0 {
			fmt.Fprintf(stderr, "cross-check: %d allocating calls inlined from other packages and %d on lines that also hold unchecked code were not compared",
				x.Elsewhere, x.Mixed)
			if len(x.Buffered) > 0 {
				fmt.Fprintf(stderr, "; %d calls allocate only without a stack buffer", len(x.Buffered))
			}
			if len(x.Unsorted) > 0 {
				fmt.Fprintf(stderr, "; calls to runtime functions not known either way: %v", x.Unsorted)
			}
			fmt.Fprintln(stderr)
		}
	}
	if failed {
		return 1
	}
	return 0
}

// goModule returns the path and directory of the module dir is in.
func goModule(dir string) (path, root string, err error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Path}}\t{{.Dir}}")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("go list -m: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		return "", "", fmt.Errorf("found %d modules in the workspace; run in one module with GOWORK=off", len(lines))
	}
	path, root, _ = strings.Cut(lines[0], "\t")
	return path, root, nil
}

// startBaseline writes every allocation res holds to a new baseline.
func startBaseline(r *noalloc.Ratchet, res *noalloc.Result, path string) error {
	lines := r.Lines(res, "")
	slices.SortFunc(lines, func(a, b noalloc.Line) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Func, b.Func), cmp.Compare(a.Reason, b.Reason), cmp.Compare(a.Source, b.Source))
	})
	var b strings.Builder
	b.WriteString(r.Header)
	for _, l := range lines {
		b.WriteString(l.String() + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// in is path p as read from dir.
func in(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
