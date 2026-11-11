package agents

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The harnesses the tests save agents on.
const (
	Claude = "claude"
	Codex  = "codex"
)

func fixed() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }

func TestOpenSeedsDefaultsOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, fixed)
	if err != nil {
		t.Fatal(err)
	}
	l := s.List()
	if len(l) != 3 || l[0].ID != Claude || l[0].Harness != Claude || l[0].Access != Subscription ||
		l[1].ID != Codex || l[1].Name != "Codex" || l[1].Model != "" || l[2].ID != "agy" || l[2].Access != Subscription {
		t.Fatalf("defaults %+v", l)
	}
	if err := s.Remove(Claude); err != nil {
		t.Fatal(err)
	}
	// A removed default is not saved again on the next open.
	s, err = Open(dir, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if l := s.List(); len(l) != 2 || l[0].ID != Codex {
		t.Fatalf("after reopen %+v", l)
	}
}

func TestCreateUpdateRemove(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, fixed)
	a, err := s.Create(Fields{Name: "  DeepSeek ", Harness: Claude, Access: OpenRouter, Model: "deepseek/deepseek-v4.1-flash"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.ID, "a_") || a.Name != "DeepSeek" || a.KeyEnv != DefaultKeyEnv || !a.Created.Equal(fixed()) {
		t.Fatalf("created %+v", a)
	}
	var bad *Invalid
	if _, err := s.Create(Fields{Name: "deepseek", Harness: Codex, Access: Subscription}); !errors.As(err, &bad) {
		t.Fatalf("a taken name, ignoring case: %v", err)
	}
	u, err := s.Update(a.ID, Fields{Name: "DeepSeek", Harness: Codex, Access: OpenRouter, Model: "deepseek/deepseek-v4.1-flash", KeyEnv: "MY_OR_KEY"})
	if err != nil || u.Harness != Codex || u.KeyEnv != "MY_OR_KEY" || u.ID != a.ID {
		t.Fatalf("update %+v %v", u, err)
	}
	if _, err := s.Update("a_none", Fields{Name: "x", Harness: Claude, Access: Subscription}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of none: %v", err)
	}
	// Kept on disk.
	s2, err := Open(dir, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := s2.Get(a.ID); err != nil || g.KeyEnv != "MY_OR_KEY" {
		t.Fatalf("reopened %+v %v", g, err)
	}
	if err := s2.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s2.Remove(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
}

func TestCheckRefuses(t *testing.T) {
	for _, tc := range []struct {
		f    Fields
		want string
	}{
		{Fields{Harness: Claude, Access: Subscription}, "name must be"},
		{Fields{Name: "a\nb", Harness: Claude, Access: Subscription}, "name must be"},
		{Fields{Name: strings.Repeat("a", 101), Harness: Claude, Access: Subscription}, "name must be"},
		{Fields{Name: "x", Harness: "opencode", Access: Subscription}, "not available yet"},
		{Fields{Name: "x", Harness: "gemini", Access: Subscription}, "harness must be"},
		{Fields{Name: "x", Harness: "agy", Access: OpenRouter, Model: "m"}, "cannot use access openrouter"},
		{Fields{Name: "x", Harness: Claude, Access: API}, "not available yet"},
		{Fields{Name: "x", Harness: Claude, Access: "local"}, "access must be"},
		{Fields{Name: "x", Harness: Claude, Access: Subscription, Model: "--tools"}, "model must match"},
		{Fields{Name: "x", Harness: Claude, Access: Subscription, KeyEnv: "OPENROUTER_API_KEY"}, "for openrouter agents only"},
		{Fields{Name: "x", Harness: Claude, Access: OpenRouter}, "needs a model"},
		{Fields{Name: "x", Harness: Claude, Access: OpenRouter, Model: "m", KeyEnv: "HOME"}, "ending in _KEY"},
		{Fields{Name: "x", Harness: Claude, Access: OpenRouter, Model: "m", KeyEnv: "lower_KEY"}, "ending in _KEY"},
	} {
		if got := Check(&tc.f); !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
	ok := Fields{Name: "x", Harness: Codex, Access: OpenRouter, Model: "qwen/qwen3-coder:free"}
	if got := Check(&ok); got != "" || ok.KeyEnv != DefaultKeyEnv {
		t.Errorf("valid fields refused: %q %+v", got, ok)
	}
}

func TestResolveFallsBackToHarness(t *testing.T) {
	s, _ := Open(t.TempDir(), fixed)
	s.Remove(Codex)
	a, err := s.Resolve(Codex)
	if err != nil || a.Harness != Codex || a.Access != Subscription || a.ID != "" {
		t.Fatalf("removed default: %+v %v", a, err)
	}
	if a, err := s.Resolve(Claude); err != nil || a.ID != Claude {
		t.Fatalf("saved default: %+v %v", a, err)
	}
	if _, err := s.Resolve("a_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestOpenRefusesABrokenFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600)
	if _, err := Open(dir, fixed); err == nil {
		t.Fatal("a broken file was read")
	}
	os.WriteFile(filepath.Join(dir, FileName), []byte(`{"agents":[{"id":"../x"}]}`), 0o600)
	if _, err := Open(dir, fixed); err == nil {
		t.Fatal("a bad id was read")
	}
}

func TestLookup(t *testing.T) {
	saved := Agent{ID: "a1", Harness: Claude, Access: Subscription}
	resolve := func(id string) (Agent, error) {
		if id == saved.ID {
			return saved, nil
		}
		return Agent{}, ErrNotFound
	}
	yes := func() (bool, error) { return true, nil }
	no := func() (bool, error) { return false, nil }
	broken := errors.New("store")
	fails := func() (bool, error) { return false, broken }

	if ag, why, err := Lookup("a1", resolve, fails); ag != saved || why != nil || err != nil {
		t.Fatalf("saved agent: %+v %v %v", ag, why, err)
	}
	if ag, why, err := Lookup("gone", resolve, nil); ag != (Agent{}) || why != ErrNotFound || err != nil {
		t.Fatalf("no retry: %+v %v %v", ag, why, err)
	}
	if ag, why, err := Lookup("gone", resolve, no); ag != (Agent{}) || why != ErrNotFound || err != nil {
		t.Fatalf("new key: %+v %v %v", ag, why, err)
	}
	if ag, why, err := Lookup("gone", resolve, yes); ag != (Agent{ID: "gone", Harness: "gone"}) || why != nil || err != nil {
		t.Fatalf("retry: %+v %v %v", ag, why, err)
	}
	if ag, why, err := Lookup("gone", resolve, fails); ag != (Agent{}) || why != nil || err != broken {
		t.Fatalf("key check fails: %+v %v %v", ag, why, err)
	}
}
