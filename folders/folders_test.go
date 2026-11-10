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
