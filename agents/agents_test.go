package agents

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/settings"
)

// The harnesses the tests save agents on.
const (
	Claude = "claude"
	Codex  = "codex"
)

func fixed() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }

// openAt opens the agents of the settings database in dir.
func openAt(t *testing.T, dir string) (*Store, *settings.DB) {
	t.Helper()
	db, err := settings.OpenDB(filepath.Join(dir, settings.DBFile), fixed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db, fixed)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

// The defaults are in code: a new database holds no agent, and lists them.
// A removed default stays removed; the database holds that one row.
func TestOpenListsDefaults(t *testing.T) {
	dir := t.TempDir()
	s, db := openAt(t, dir)
	if rows, err := db.Agents(); err != nil || len(rows) != 0 {
		t.Fatalf("a new database holds %v %v", rows, err)
	}
	l := s.List()
	if len(l) != 3 || l[0].ID != Claude || l[0].Harness != Claude || l[0].Access != Subscription ||
		l[1].ID != Codex || l[1].Name != "Codex" || l[1].Model != "" || l[2].ID != "agy" || l[2].Access != Subscription {
		t.Fatalf("defaults %+v", l)
	}
	if err := s.Remove(Claude); err != nil {
		t.Fatal(err)
	}
	// A removed default is not listed again on the next open.
	s, db = openAt(t, dir)
	if l := s.List(); len(l) != 2 || l[0].ID != Codex {
		t.Fatalf("after reopen %+v", l)
	}
	if rows, _ := db.Agents(); len(rows) != 1 || rows[0].ID != Claude || !rows[0].Removed {
		t.Fatalf("rows %+v", rows)
	}
}

func TestCreateUpdateRemove(t *testing.T) {
	dir := t.TempDir()
	s, db := openAt(t, dir)
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
	// Kept in the database.
	s2, _ := openAt(t, dir)
	if g, err := s2.Get(a.ID); err != nil || g.KeyEnv != "MY_OR_KEY" {
		t.Fatalf("reopened %+v %v", g, err)
	}
	if err := s2.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s2.Remove(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	// Each change is a settings change naming the agent, and the fields an
	// update changed, never a value.
	cs, _, err := db.Changes()
	if err != nil || len(cs) != 3 {
		t.Fatalf("changes %+v %v", cs, err)
	}
	if cs[0].Setting != settings.SettingAgents || cs[0].Keys[0] != a.ID || cs[0].Version != 1 {
		t.Errorf("create: %+v", cs[0])
	}
	if strings.Join(cs[1].Keys, ",") != a.ID+".harness,"+a.ID+".key_env" || cs[1].Version != 2 {
		t.Errorf("update: %+v", cs[1])
	}
	if b, _ := json.Marshal(cs); strings.Contains(string(b), "MY_OR_KEY") || strings.Contains(string(b), "deepseek") {
		t.Errorf("a change holds a value: %s", b)
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
	s, _ := openAt(t, t.TempDir())
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

// An agents.json of 0.4 is read as rows: a default as it was is left out,
// a changed default and an added agent are kept whole, a default the file
// lacks is removed. A file that cannot be used is refused.
func TestImportRows(t *testing.T) {
	added := Agent{ID: "a_1", Name: "DeepSeek", Harness: Claude, Access: OpenRouter, Model: "m", KeyEnv: DefaultKeyEnv, Created: fixed(), Updated: fixed()}
	changed := Agent{ID: Codex, Name: "Codex · fast", Harness: Codex, Access: Subscription, Model: "gpt-5", Created: fixed(), Updated: fixed()}
	asIs := Agent{ID: Claude, Name: "Claude Code", Harness: Claude, Access: Subscription, Created: fixed(), Updated: fixed()}
	b, _ := json.Marshal(file{Agents: []Agent{asIs, changed, added}})
	rows, err := ImportRows(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].ID != Codex || rows[1].ID != "a_1" || rows[2].ID != "agy" || !rows[2].Removed {
		t.Fatalf("rows %+v", rows)
	}
	// Stored and opened, they list as the file did.
	db, err := settings.OpenDB(filepath.Join(t.TempDir(), settings.DBFile), fixed)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, r := range rows {
		if err := db.SetAgent(r, []string{r.ID}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(db, fixed)
	if err != nil {
		t.Fatal(err)
	}
	l := s.List()
	if len(l) != 3 || l[0].ID != Claude || l[1] != changed || l[2] != added {
		t.Fatalf("listed %+v", l)
	}
	for _, bad := range []string{"{", `{"agents":[{"id":"../x"}]}`, `{"agents":[{"id":"a_1"},{"id":"a_1"}]}`} {
		if _, err := ImportRows([]byte(bad)); err == nil {
			t.Errorf("%s was read", bad)
		}
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
