package main

import (
	"errors"
	"go/ast"
	gobuild "go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/staysup/agentbus"

// The package rule of one program: three roles in one codebase. Control
// (api, jobs, queue, agents, store, the gateway, replay, settings) keeps the
// record, the queue, the settings and the saved agents, and never touches a
// person's folder: it knows a folder by its reference only. The folder side
// (folders, connector) reads and writes the person's folders. The runner
// side (runner, run, isolation) works in copies, and never imports the
// folder side: on one machine it reaches the connector through interfaces
// the command wires (plans/IDENTITIES.md and plans/ARCHITECTURE.md in the
// notes).
//
// Within that: harness, what differs per harness, and
// settings, the person's settings, are at the bottom and use nothing of the
// module; the gateway and the record know
// nothing about jobs; run, which starts one turn, knows nothing about queues
// or saved agents; agents, the saved agents, may use run and nothing of the
// queue side; isolation, the session's copies, may use workspace and
// nothing of the turn or queue side; the queue may use run and workspace,
// never the reverse. folders, the connector's folders by path, uses nothing
// of the module, and the control side (api, jobs, queue, agents, store, the
// gateway, replay, settings) never uses it: control knows a folder by its
// reference only (plans/IDENTITIES.md in the notes).
// Checked over every import, direct or not.
func TestPackageLayers(t *testing.T) {
	jobSide := []string{"queue", "jobs", "runner"}
	controlSide := controlPackages
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
		{[]string{"folders"}, append([]string{"harness", "run", "agents", "workspace", "isolation", "api", "gateway", "adapter", "event",
			"cost", "replay", "store", "body", "logfile", "settings", "cmd"}, jobSide...)},
		{controlSide, []string{"folders", "connector", "isolation", "runner"}},
		// The folder side uses the queue's types for its answers (and
		// through them harness), and nothing of the runner side or of the
		// record.
		{[]string{"connector"}, []string{"run", "agents", "isolation", "api", "gateway", "adapter", "event",
			"cost", "replay", "store", "body", "logfile", "settings", "cmd", "jobs", "runner"}},
		{[]string{"isolation", "run"}, []string{"folders", "connector"}},
		{[]string{"runner"}, []string{"connector"}},
		// credentials, where control's gateway gets a credential, uses
		// nothing of the module.
		{[]string{"credentials"}, append([]string{"harness", "run", "agents", "workspace", "isolation", "api", "gateway", "adapter", "event",
			"cost", "replay", "store", "body", "logfile", "settings", "cmd", "folders", "connector", "blobs"}, jobSide...)},
		{[]string{"blobs"}, append([]string{"harness", "run", "agents", "workspace", "isolation", "api", "gateway", "adapter", "event",
			"cost", "replay", "store", "body", "logfile", "settings", "cmd", "folders", "connector"}, jobSide...)},
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

// controlPackages are the control side: they never touch a person's folder.
var controlPackages = []string{"api", "jobs", "queue", "agents", "store", "gateway", "replay", "settings", "credentials"}

// The calls that touch files or start programs. A control package makes
// none of them, but where an entry of controlAllowed names it: its own data
// folder's files (the record, the saved agents, the settings). The list only
// shrinks: an entry no call needs any more fails the test, so it is removed.
var touchCalls = map[string][]string{
	"os": {"Stat", "Lstat", "Open", "OpenFile", "ReadDir", "ReadFile", "OpenRoot", "OpenInRoot", "Readlink", "Remove", "RemoveAll",
		"Mkdir", "MkdirAll", "MkdirTemp", "WriteFile", "Rename", "Chmod", "Chtimes", "Chown", "Lchown", "Symlink", "Link",
		"Create", "CreateTemp", "Truncate", "StartProcess", "DirFS", "CopyFS"},
	"path/filepath": {"EvalSymlinks", "Walk", "WalkDir", "Glob"},
	"os/exec":       {"Command", "CommandContext", "LookPath"},
	"syscall":       {"Exec", "ForkExec", "Open", "Stat", "Lstat"},
}

// controlAllowed are "file function package.call" in control packages that
// may touch files: each reads or writes the control plane's own data
// folder, never a person's.
var controlAllowed = []string{
	"agents/agents.go Open os.ReadFile",
	// The vendor's own login, which prints a subscription's token, and the
	// folder control keeps those tokens in.
	"credentials/connect.go Manager.Connect exec.Command",
	"credentials/connect.go Run exec.Command",
	"credentials/subscriptions.go Subscriptions.Put os.CreateTemp",
	"credentials/subscriptions.go Subscriptions.Put os.MkdirAll",
	"credentials/subscriptions.go Subscriptions.Put os.Remove",
	"credentials/subscriptions.go Subscriptions.Put os.Rename",
	"credentials/subscriptions.go Subscriptions.Remove os.Remove",
	"credentials/subscriptions.go Subscriptions.read os.ReadFile",
	"agents/agents.go Store.save os.CreateTemp",
	"agents/agents.go Store.save os.Remove",
	"agents/agents.go Store.save os.Rename",
	"settings/files.go Files.write filepath.EvalSymlinks",
	"settings/files.go Files.write os.CreateTemp",
	"settings/files.go Files.write os.Remove",
	"settings/files.go Files.write os.Rename",
	"settings/files.go Files.write os.Stat",
	"settings/files.go isDir os.Stat",
	"settings/files.go makeStarter os.OpenFile",
	"settings/files.go readFile os.Lstat",
	"settings/files.go readFile os.ReadFile",
	"store/store.go Open os.OpenFile",
}

// Control never touches a person's folder: no control package calls a
// function that reads or writes files or starts a program, but those its
// own data folder needs (controlAllowed). An import rule cannot see an
// os.Stat on a folder; this reads every call.
func TestControlTouchesNoFolder(t *testing.T) {
	seen := map[string]bool{}
	allowed := map[string]bool{}
	for _, a := range controlAllowed {
		allowed[a] = true
	}
	for _, top := range controlPackages {
		for _, pkg := range packagesUnder(t, top) {
			for _, call := range touches(t, pkg) {
				seen[call] = true
				if !allowed[call] {
					t.Errorf("%s: a control package touches files or starts a program; the folder side does that, or add it to controlAllowed if it is the data folder's own", call)
				}
			}
		}
	}
	for _, a := range controlAllowed {
		if !seen[a] {
			t.Errorf("controlAllowed lists %s, which no call needs any more: remove it", a)
		}
	}
}

// The check itself must see a call: the folder side makes many.
func TestControlTouchesSeesCalls(t *testing.T) {
	calls := touches(t, module+"/connector")
	found := false
	for _, c := range calls {
		if strings.HasSuffix(c, " os.OpenRoot") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the connector's calls %v lack os.OpenRoot", calls)
	}
}

// touches are the calls of touchCalls in pkg's files, tests left out, as
// "file function package.call" with the file from the module's folder.
func touches(t *testing.T, pkg string) []string {
	t.Helper()
	dir := filepath.Join(root(t), filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")))
	bp, err := gobuild.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("%s: %v", pkg, err)
	}
	var out []string
	for _, name := range append(append([]string{}, bp.GoFiles...), bp.CgoFiles...) {
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		local := map[string]string{} // the name a file gives an import, to its path
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			n := path.Base(p)
			if im.Name != nil {
				n = im.Name.Name
			}
			local[n] = p
		}
		rel, _ := filepath.Rel(root(t), filepath.Join(dir, name))
		rel = filepath.ToSlash(rel)
		for _, d := range f.Decls {
			fn := "init"
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) > 0 {
					rt := fd.Recv.List[0].Type
					if st, ok := rt.(*ast.StarExpr); ok {
						rt = st.X
					}
					if id, ok := rt.(*ast.Ident); ok {
						fn = id.Name + "." + fn
					}
				}
			}
			ast.Inspect(d, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				p, ok := local[id.Name]
				if !ok {
					return true
				}
				for _, c := range touchCalls[p] {
					if sel.Sel.Name == c {
						out = append(out, rel+" "+fn+" "+path.Base(p)+"."+c)
					}
				}
				return true
			})
		}
	}
	return out
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
