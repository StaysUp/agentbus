package run

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// After a codex turn, the names its own models cache in the session's home
// lists are kept for the runner's report; a session with no cache, or a
// harness that keeps none, changes nothing.
func TestReadModels(t *testing.T) {
	homes := t.TempDir()
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	e := &Engine{cfg: Config{Homes: homes, Now: func() time.Time { return at }}}
	e.readModels("s1", "codex")
	if len(e.Models()) != 0 {
		t.Fatal("models with no cache")
	}
	dir := filepath.Join(homes, "s1", ".codex")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-6.1-sol","visibility":"list"},{"slug":"gpt-reserve","visibility":"hide"}]}`), 0o600)
	e.readModels("s1", "claude")
	if len(e.Models()) != 0 {
		t.Fatal("claude keeps no cache")
	}
	e.readModels("../s1", "codex")
	if len(e.Models()) != 0 {
		t.Fatal("a session id that is not one was read")
	}
	e.readModels("s1", "codex")
	got := e.Models()["codex"]
	if !slices.Equal(got.Names, []string{"gpt-6.1-sol"}) || !got.At.Equal(at) {
		t.Fatalf("%+v", got)
	}
	// A cache that is a link out of the home is not followed.
	os.Remove(filepath.Join(dir, "models_cache.json"))
	other := filepath.Join(t.TempDir(), "x.json")
	os.WriteFile(other, []byte(`{"models":[{"slug":"elsewhere"}]}`), 0o600)
	os.Symlink(other, filepath.Join(dir, "models_cache.json"))
	e.readModels("s1", "codex")
	if got := e.Models()["codex"]; !slices.Equal(got.Names, []string{"gpt-6.1-sol"}) {
		t.Fatalf("followed a link out of the home: %+v", got)
	}
}
