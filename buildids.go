package noalloc

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// The code a summary is worked out by, so that a change to it is a change
// to every summary.
//
//go:embed noalloc.go summary.go machine.go crosscheck.go growth.go buildids.go
var ruleSource embed.FS

// ruleHash hashes the code summaries are worked out by.
func ruleHash() string {
	h := sha256.New()
	fs.WalkDir(ruleSource, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := ruleSource.ReadFile(path)
			fmt.Fprintf(h, "%s %d\n", path, len(b))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// buildPackage is a package of the build being checked, as go list -export
// gives it: its archive, which holds its assembly with its compiled Go, and
// its build ID, whose first half hashes all that went into compiling it.
type buildPackage struct {
	path, archive, actionID string
}

// buildPackages compiles the packages of cfg's build as go build would, and
// returns each one they depend on, with the runtime and what it depends on.
// Every program links those, and assembly reaches them with no import, as
// sync/atomic's does, so go list -deps of a library can leave them out. They
// are listed apart because go list takes no import path beside .go files.
func buildPackages(cfg Config, env []string) ([]buildPackage, error) {
	args := []string{"list", "-deps", "-export", "-f", "{{.ImportPath}}\t{{.Export}}\t{{.BuildID}}"}
	if len(cfg.Overlay) > 0 {
		dir, err := os.MkdirTemp("", "noalloc-overlay")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		f, err := writeOverlay(dir, cfg.Overlay)
		if err != nil {
			return nil, err
		}
		args = append(args, "-overlay", f)
	}
	pkgs, err := listPackages(cfg.Dir, env, append(slices.Clone(args), cfg.Patterns...))
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(pkgs, func(p buildPackage) bool { return p.path == "runtime" }) {
		return pkgs, nil
	}
	linked, err := listPackages(cfg.Dir, env, append(args, "runtime"))
	if err != nil {
		return nil, err
	}
	for _, p := range linked {
		if !slices.ContainsFunc(pkgs, func(q buildPackage) bool { return q.path == p.path }) {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, nil
}

// listPackages runs go with args, a go list printing a buildPackage per line.
func listPackages(dir string, env, args []string) ([]buildPackage, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir, cmd.Env = dir, env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export: %v: %s", err, tail(stderr.String(), 2000))
	}
	var pkgs []buildPackage
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 {
			continue
		}
		action, _, _ := strings.Cut(f[2], "/")
		pkgs = append(pkgs, buildPackage{path: f[0], archive: f[1], actionID: action})
	}
	return pkgs, nil
}

// buildKey hashes what the summaries of cfg's build depend on: the code
// they are worked out by, which packages are the module's, the functions
// labeled to allocate nothing, and every package outside the module, by the
// action ID that changes with its source, its imports, the toolchain, the
// platform, and the flags, tags, and profile it is compiled with. It names the
// cache file; moduleKey, kept inside the file, covers the module's own code.
func buildKey(cfg Config, pkgs []buildPackage, assumed map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%q\n", summaryFormat, ruleHash(), append([]string{cfg.Module}, cfg.SourceModules...))
	var labels []string
	for f, kind := range assumed {
		labels = append(labels, kind+": "+f)
	}
	slices.Sort(labels)
	fmt.Fprintf(h, "%q\n", labels)
	hashPackages(h, cfg, pkgs, false)
	return hex.EncodeToString(h.Sum(nil))
}

// moduleKey hashes the module's own packages by their action IDs. A summary
// depends on module code too: what the module hands a function outside it to
// call decides what that function's indirect calls reach.
func moduleKey(cfg Config, pkgs []buildPackage) string {
	h := sha256.New()
	hashPackages(h, cfg, pkgs, true)
	return hex.EncodeToString(h.Sum(nil))
}

// hashPackages writes to h a sorted "path actionID" line for each of pkgs
// that is in cfg's module when inModule is true, and outside it when false.
func hashPackages(h io.Writer, cfg Config, pkgs []buildPackage, inModule bool) {
	var lines []string
	for _, p := range pkgs {
		if cfg.inModule(p.path) == inModule {
			lines = append(lines, p.path+" "+p.actionID)
		}
	}
	slices.Sort(lines)
	for _, l := range lines {
		fmt.Fprintln(h, l)
	}
}
