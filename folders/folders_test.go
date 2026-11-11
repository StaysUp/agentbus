package folders

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder keeps its reference: the same path, cleaned, gives the same one,
// across a restart; the file is the owner's only; the path never shows in
// what List reports.
func TestRegistry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "folders.json")
	r, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !connectorShape(r.Connector()) {
		t.Fatalf("connector %q", r.Connector())
	}
	dir := t.TempDir()
	ref, err := r.Register(dir)
	if err != nil || !strings.HasPrefix(ref, "fld_") || len(ref) != 20 || ref != Ref(dir) {
		t.Fatalf("%q %v", ref, err)
	}
	if again, _ := r.Register(dir + "/."); again != ref {
		t.Fatalf("the cleaned path got %q, want %q", again, ref)
	}
	if _, err := r.Register("relative/path"); err == nil {
		t.Fatal("a relative path was kept")
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("folders.json: %v %v", info.Mode(), err)
	}
	r2, err := Open(p)
	if err != nil || r2.Connector() != r.Connector() {
		t.Fatalf("reopened: %v", err)
	}
	if got, ok := r2.Path(ref); !ok || got != filepath.Clean(dir) {
		t.Fatalf("path %q %v", got, ok)
	}
	if _, ok := r2.Path("fld_0000000000000000"); ok {
		t.Fatal("an unknown reference resolved")
	}
	list := r2.List(10)
	if len(list) != 1 || list[0].Ref != ref || list[0].Name != filepath.Base(dir) {
		t.Fatalf("%+v", list)
	}
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); err == nil {
		t.Fatal("a broken file was read")
	}
}

// A session copy is never listed as a folder: one inside the data folder's
// copies, and a 0.2 copy (worktrees/job_...) that 0.3.0's migration
// registered from a data folder since moved. Each stays registered, so a
// job that names it still finds it, and nothing on disk is touched.
func TestListLeavesOutCopies(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "folders.json")
	r, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	r.CopiesIn(filepath.Join(home, "sessions"), filepath.Join(home, "worktrees"))
	mine := t.TempDir()
	keep := []string{mine, "/work/worktrees", "/work/worktrees/job_2f2e608012a9ca1", "/work/worktrees/job_2F2E608012A9CA19",
		filepath.Join(home, "sessions"), home + "-other/sessions/ses_1"}
	hide := []string{filepath.Join(home, "sessions", "ses_0123456789abcdef"), filepath.Join(home, "sessions", "ses_0123456789abcdef", "sub"),
		filepath.Join(home, "worktrees", "job_0123456789abcdef"), "/old/home/worktrees/job_2f2e608012a9ca19",
		"/old/home/worktrees/job_2f2e608012a9ca19/sub"}
	refs := map[string]string{}
	for _, f := range append(append([]string{}, keep...), hide...) {
		ref, err := r.Register(f)
		if err != nil {
			t.Fatal(err)
		}
		refs[ref] = f
	}
	listed := map[string]bool{}
	for _, f := range r.List(100) {
		listed[refs[f.Ref]] = true
	}
	for _, f := range keep {
		if !listed[f] {
			t.Errorf("%s is not listed", f)
		}
	}
	for _, f := range hide {
		if listed[f] {
			t.Errorf("the copy %s is listed", f)
		}
		if got, ok := r.Path(Ref(f)); !ok || got != f {
			t.Errorf("the copy %s is no longer registered: %q %v", f, got, ok)
		}
	}
	// max counts listed folders only.
	if got := r.List(len(keep)); len(got) != len(keep) {
		t.Fatalf("List(%d) gave %d", len(keep), len(got))
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal(err)
	}
}
