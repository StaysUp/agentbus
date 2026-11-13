package settings

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Date(2026, 11, 12, 1, 2, 3, 0, time.UTC) }

func openTestDB(t *testing.T, dir string) *DB {
	t.Helper()
	d, err := OpenDB(filepath.Join(dir, DBFile), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// stored is the raw value of a setting's row.
func stored(t *testing.T, d *DB, name string) string {
	t.Helper()
	var v string
	if err := d.db.QueryRow(`SELECT value FROM settings WHERE name = ?`, name).Scan(&v); err != nil {
		return ""
	}
	return v
}

// The defaults in code are DefaultPolicy: a change of fill() that is not
// also a change of the defaults, with its upgrade step, fails here.
func TestDefaultsAreDefaultPolicy(t *testing.T) {
	if !reflect.DeepEqual(flatten(defaultPolicy()), flatten(DefaultPolicy())) {
		t.Fatalf("the defaults differ from DefaultPolicy: change a default in defaults.go, with an upgrade step")
	}
}

// The defaults write out every key: a key they leave out would take fill()'s
// value, so a later change of fill() would change a default with no upgrade
// step to keep the old value.
func TestDefaultsWrittenInFull(t *testing.T) {
	d := defaultPolicy()
	filled := d
	filled.fill()
	as, _ := json.Marshal(d)
	fs, _ := json.Marshal(filled)
	if string(as) != string(fs) {
		t.Errorf("the defaults leave out a key:\n written %s\n filled  %s", as, fs)
	}
}

// Each upgrade step names a policy key and an old value that differs from
// today's default, or a retired key the policy no longer has, at its top or
// one table down; before the steps, a turn token applies, as in 0.4. A file
// or a body that still holds a retired key, agent_api.continue too, is read
// with it left out.
func TestUpgradeSteps(t *testing.T) {
	now := flatten(defaultPolicy())
	var last *upgradeStep
	for i, st := range upgradeSteps {
		_, ok := now[st.Key]
		switch {
		case st.Retired && (ok || strings.Count(st.Key, ".") > 1):
			t.Errorf("retired step %s: the policy has the key, or it is deeper than one table", st.Key)
		case !st.Retired && !ok:
			t.Errorf("step %s: no such key", st.Key)
		case !st.Retired:
			last = &upgradeSteps[i]
		}
	}
	if last != nil && string(now[last.Key]) == string(last.Old) {
		t.Errorf("the last step %s keeps the default it changes", last.Key)
	}
	if strings.Join(retiredKeys, ",") != "commands,subscriptions_on_runners,agent_api.continue,sandbox.read" {
		t.Errorf("retired keys %v", retiredKeys)
	}
	for _, m := range movedKeys {
		if _, ok := now[m.To]; !ok {
			t.Errorf("moved step %s: no key %s", m.From, m.To)
		}
	}
	if p, err := ParsePolicy([]byte("[agent_api]\ncontinue = false\nstart = false\n")); err != nil || *p.AgentAPI.Start {
		t.Errorf("a file with agent_api.continue: %+v %v", p.AgentAPI, err)
	}
	if p, err := DecodePolicy([]byte(`{"agent_api":{"continue":false,"start":false}}`)); err != nil || *p.AgentAPI.Start {
		t.Errorf("a body with agent_api.continue: %+v %v", p.AgentAPI, err)
	}
	if _, err := DecodePolicy([]byte(`{"agent_api":{"bogus":true}}`)); err == nil {
		t.Error("an unknown key in a table was taken")
	}
	if p := policyBefore(0); !*p.AgentAPI.Apply {
		t.Error("before the steps, apply is off")
	}
	if p := policyBefore(len(upgradeSteps)); !reflect.DeepEqual(flatten(p), now) {
		t.Error("after every step, the policy is not the defaults")
	}
}

// A new database reads as the defaults and holds nothing. A change keeps
// only the keys that differ from the defaults, names them in its change, and
// moves the settings version up by one. A stale version is refused; a write
// that changes nothing makes no change.
func TestDBPolicyKeepsOnlyChanges(t *testing.T) {
	dir := t.TempDir()
	d := openTestDB(t, dir)
	p, v0, err := d.Policy()
	if err != nil || !reflect.DeepEqual(flatten(p), flatten(DefaultPolicy())) || v0 == "" {
		t.Fatalf("new: %+v %q %v", p, v0, err)
	}
	if got := stored(t, d, SettingPermissions); got != "" {
		t.Fatalf("a new database holds %s", got)
	}
	depth := 1
	want := Policy{Forbid: []string{"git push"}, Limits: Limits{MaxDepth: &depth}}
	if _, _, err := d.SetPolicy(want, "stale"); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale: %v", err)
	}
	got, v1, err := d.SetPolicy(want, v0)
	if err != nil || got.Forbid[0] != "git push" || *got.Limits.MaxDepth != 1 || *got.Limits.MaxJobsPerTurn != 10 || v1 == v0 {
		t.Fatalf("set: %+v %q %v", got, v1, err)
	}
	var overlay map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stored(t, d, SettingPermissions)), &overlay); err != nil || len(overlay) != 2 ||
		string(overlay["forbid"]) != `["git push"]` || string(overlay["limits.max_depth"]) != "1" {
		t.Fatalf("stored %s", stored(t, d, SettingPermissions))
	}
	// The same policy again: nothing changes.
	if _, v, err := d.SetPolicy(want, Any); err != nil || v != v1 {
		t.Fatalf("again: %q %v", v, err)
	}
	cs, _, _ := d.Changes()
	if len(cs) != 1 || cs[0].Setting != SettingPermissions || strings.Join(cs[0].Keys, ",") != "forbid,limits.max_depth" || cs[0].Version != 1 {
		t.Fatalf("changes %+v", cs)
	}
	if b, _ := json.Marshal(cs); strings.Contains(string(b), "git push") {
		t.Fatalf("a change holds a value: %s", b)
	}
	// Kept: a reopened database reads the same, at the same version.
	d.Close()
	d = openTestDB(t, dir)
	if p, v, err := d.Policy(); err != nil || v != v1 || p.Forbid[0] != "git push" {
		t.Fatalf("reopened: %+v %q %v", p, v, err)
	}
	// Back to the defaults: nothing is kept.
	if _, _, err := d.SetPolicy(Policy{}, v1); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, d, SettingPermissions); got != "{}" {
		t.Fatalf("after the defaults: %s", got)
	}
	if in, _ := d.Info(); in.Version != 2 || !in.Changed.Equal(fixedNow()) {
		t.Fatalf("info %+v", in)
	}
}

// The instructions: the default text until changed; the default text again
// keeps nothing.
func TestDBInstructions(t *testing.T) {
	d := openTestDB(t, t.TempDir())
	text, v0, err := d.Instructions()
	if err != nil || text != DefaultInstructions {
		t.Fatalf("new: %q %v", text, err)
	}
	if _, err := d.SetInstructions("a\x00b", v0); err == nil {
		t.Fatal("a NUL was stored")
	}
	v1, err := d.SetInstructions("Be brief.", v0)
	if err != nil || v1 == v0 {
		t.Fatalf("set: %q %v", v1, err)
	}
	if _, err := d.SetInstructions("x", v0); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale: %v", err)
	}
	if text, v, _ := d.Instructions(); text != "Be brief." || v != v1 {
		t.Fatalf("read: %q %q", text, v)
	}
	if _, err := d.SetInstructions(DefaultInstructions, v1); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, d, SettingInstructions); got != "{}" {
		t.Fatalf("after the default text: %s", got)
	}
	s, err := d.Snapshot()
	if err != nil || s.Revision != 2 || s.Instructions != DefaultInstructions || s.PolicyVersion == "" {
		t.Fatalf("snapshot %+v %v", s, err)
	}
}

// A change and its event are one: a change whose event row cannot be stored
// is not kept, and a stored event the record refused waits in the database
// and is handed over again, the same id, until the record takes it, across
// a restart too.
func TestChangeAndEventSavedTogether(t *testing.T) {
	old := RetryEvery
	RetryEvery = 20 * time.Millisecond
	defer func() { RetryEvery = old }()
	dir := t.TempDir()
	d := openTestDB(t, dir)

	// The changes table refuses rows: the setting is not kept either.
	if _, err := d.db.Exec(`CREATE TRIGGER refuse BEFORE INSERT ON changes BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetInstructions("kept?", Any); err == nil {
		t.Fatal("a change was stored without its event")
	}
	if text, _, _ := d.Instructions(); text != DefaultInstructions {
		t.Fatalf("the setting changed without its event: %q", text)
	}
	if in, _ := d.Info(); in.Version != 0 {
		t.Fatalf("version %d", in.Version)
	}
	if _, err := d.db.Exec(`DROP TRIGGER refuse`); err != nil {
		t.Fatal(err)
	}

	// The record refuses: the change is kept, its event waits.
	var mu sync.Mutex
	var got []Change
	var down = true
	sink := func(c Change) error {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return errors.New("record down")
		}
		got = append(got, c)
		return nil
	}
	d.SetSink(sink, nil)
	if _, err := d.SetInstructions("one", Any); err != nil {
		t.Fatal(err)
	}
	if n := d.Publish(); n != 1 {
		t.Fatalf("%d waiting, want 1", n)
	}
	// Closed with the event still waiting, and opened again.
	d.Close()
	d = openTestDB(t, dir)
	if _, pub, _ := d.Changes(); len(pub) != 1 || pub[0] {
		t.Fatalf("published %v", pub)
	}
	mu.Lock()
	down = false
	mu.Unlock()
	d.SetSink(sink, nil)
	if _, err := d.SetInstructions("two", Any); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(got) != 2 || got[0].Version != 1 || got[1].Version != 2 || got[0].Setting != SettingInstructions || got[0].ID == "" || got[0].ID == got[1].ID {
		t.Fatalf("handed over %+v", got)
	}
	mu.Unlock()
	if _, pub, _ := d.Changes(); len(pub) != 2 || !pub[0] || !pub[1] {
		t.Fatalf("published %v", pub)
	}

	// The retry hands over what a refusal left, without another write.
	mu.Lock()
	down = true
	mu.Unlock()
	if _, err := d.SetInstructions("three", Any); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	down = false
	mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the retry never handed the change over")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A stored policy that a build cannot use is an *Error with a version, so a
// PUT can replace it.
func TestDBBrokenPolicy(t *testing.T) {
	d := openTestDB(t, t.TempDir())
	if _, err := d.db.Exec(`INSERT INTO settings(name, value, updated) VALUES ('permissions', '{"edit":"everywhere"}', '')`); err != nil {
		t.Fatal(err)
	}
	_, v, err := d.Policy()
	var se *Error
	if !errors.As(err, &se) || !strings.Contains(se.Why, "edit") || v == "" {
		t.Fatalf("broken: %q %v", v, err)
	}
	if _, err := Read(d); !errors.As(err, &se) {
		t.Fatalf("snapshot of a broken policy: %v", err)
	}
	if _, _, err := d.SetPolicy(Policy{}, v); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, _, err := d.Policy(); err != nil {
		t.Fatal(err)
	}
}

// A stored policy that holds commands, the switch retired in 0.5, reads with
// the rest of its changes, and the stored row is not rewritten by reading.
func TestDBRetiredCommandsKey(t *testing.T) {
	d := openTestDB(t, t.TempDir())
	row := `{"commands":false,"subagents":false}`
	if _, err := d.db.Exec(`INSERT INTO settings(name, value, updated) VALUES ('permissions', ?, '')`, row); err != nil {
		t.Fatal(err)
	}
	p, _, err := d.Policy()
	if err != nil || *p.Subagents {
		t.Fatalf("policy: %+v %v", p, err)
	}
	if got := stored(t, d, SettingPermissions); got != row {
		t.Errorf("stored %s, want %s", got, row)
	}
}
