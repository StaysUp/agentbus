package main

import (
	"errors"
	gobuild "go/build"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/staysup/agentbus"

// The package rule of one program: harness, what differs per harness, and
// settings, the person's settings, are at the bottom and use nothing of the
// module; the gateway and the record know
// nothing about jobs; run, which starts one turn, knows nothing about queues
// or saved agents; agents, the saved agents, may use run and nothing of the
// queue side; isolation, the session's copies, may use workspace and
// nothing of the turn or queue side; the queue may use run and workspace,
// never the reverse.
// Checked over every import, direct or not.
func TestPackageLayers(t *testing.T) {
	jobSide := []string{"queue", "jobs", "runner"}
	for _, c := range []struct {
		pkgs      []string // these packages, and every subpackage
		mayNotUse []string
	}{
		{[]string{"harness"}, append([]string{"run", "agents", "workspace", "isolation", "api", "gateway", "adapter", "event",
			"cost", "replay", "store", "body", "logfile", "settings", "cmd"}, jobSide...)},
		{[]string{"settings"}, append([]string{"harness", "run", "agents", "workspace", "isolation", "api", "gateway", "adapter", "event",
			"cost", "replay", "store", "body", "logfile", "cmd"}, jobSide...)},
		{[]string{"gateway", "adapter", "event", "cost", "replay", "store", "body", "logfile"},
			append([]string{"run", "agents", "workspace", "isolation", "api"}, jobSide...)},
		{[]string{"run"}, append([]string{"agents", "workspace", "isolation", "api"}, jobSide...)},
		{[]string{"agents"}, append([]string{"workspace", "isolation", "api"}, jobSide...)},
		{[]string{"workspace"}, append([]string{"run", "agents", "isolation", "api"}, jobSide...)},
		{[]string{"isolation"}, append([]string{"run", "agents", "api", "gateway", "adapter", "event", "cost", "replay",
			"store", "body", "logfile", "harness", "settings"}, jobSide...)},
		{[]string{"queue"}, []string{"jobs", "runner", "api"}},
		{[]string{"jobs"}, []string{"runner", "api"}},
		{[]string{"runner"}, []string{"api"}},
	} {
		for _, top := range c.pkgs {
			pkgs := packagesUnder(t, top)
			if len(pkgs) == 0 {
				t.Fatalf("no package %s: the rule would check nothing", top)
			}
			for _, pkg := range pkgs {
				for _, dep := range deps(t, pkg) {
					for _, bad := range c.mayNotUse {
						if dep == module+"/"+bad || strings.HasPrefix(dep, module+"/"+bad+"/") {
							t.Errorf("%s uses %s", strings.TrimPrefix(pkg, module+"/"), strings.TrimPrefix(dep, module+"/"))
						}
					}
				}
			}
		}
	}
}

// The test itself must see a breach: run importing queue would be one.
func TestPackageLayersSeesTheGraph(t *testing.T) {
	got := deps(t, module+"/runner")
	for _, want := range []string{module + "/run", module + "/queue", module + "/store"} {
		if i := sort.SearchStrings(got, want); i == len(got) || got[i] != want {
			t.Fatalf("runner's imports %v lack %s", got, want)
		}
	}
}

// root is the module's folder: a test runs in its package's folder,
// cmd/agentbus.
func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// packagesUnder lists the module's packages at dir and at every depth below
// it. Folders Go's own tools skip (_ or . first, and testdata) are skipped.
func packagesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	top := filepath.Join(root(t), dir)
	err := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if name := d.Name(); path != top && (strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || name == "testdata") {
			return filepath.SkipDir
		}
		if matches, _ := filepath.Glob(filepath.Join(path, "*.go")); len(matches) > 0 {
			rel, _ := filepath.Rel(root(t), path)
			out = append(out, module+"/"+filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

// deps lists every package of the module that pkg imports, directly or not,
// sorted. Test files do not count.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	seen := map[string]bool{}
	var walk func(string)
	walk = func(p string) {
		dir := filepath.Join(root(t), filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(p, module), "/")))
		bp, err := gobuild.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, imp := range bp.Imports {
			if (imp == module || strings.HasPrefix(imp, module+"/")) && !seen[imp] {
				seen[imp] = true
				walk(imp)
			}
		}
	}
	walk(pkg)
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
