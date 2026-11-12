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

// The defaults of the latest version are DefaultPolicy: a change of fill()
// that is not a new version of the defaults fails here.
func TestDefaultsLatestIsDefaultPolicy(t *testing.T) {
	d, ok := DefaultsFor(LatestDefaults())
	if !ok {
		t.Fatal("no latest defaults")
	}
	if !reflect.DeepEqual(flatten(d.Policy), flatten(DefaultPolicy())) || d.Instructions != DefaultInstructions {
		t.Fatalf("the latest defaults differ from DefaultPolicy: change the defaults by adding a version (defaults.go)")
	}
	if _, ok := DefaultsFor(0); ok {
		t.Error("version 0 exists")
	}
	if _, ok := DefaultsFor(LatestDefaults() + 1); ok {
		t.Error("a version past the latest exists")
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
	if in, _ := d.Info(); in.Version != 2 || in.Defaults != LatestDefaults() {
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

// A new build's defaults never reach a control by themselves. A control made
// on version 1 keeps reading version 1 when the build adds a wider version 2,
// until it moves to it; then the person's own changes stay, and every other
// key takes version 2's value.
func TestDefaultsVersionPinned(t *testing.T) {
	saved := defaultSets
	defer func() { defaultSets = saved }()
	defaultSets = []func() Defaults{defaultsV1}
	dir := t.TempDir()
	d := openTestDB(t, dir)
	if _, _, err := d.SetPolicy(Policy{Forbid: []string{"rm"}}, Any); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// The build adds version 2: deeper asking, and the agent tools on.
	defaultSets = append(defaultSets, func() Defaults {
		v := defaultsV1()
		depth, on := 5, true
		v.Policy.Limits.MaxDepth, v.Policy.AgentAPI.Tools = &depth, &on
		return v
	})
	d = openTestDB(t, dir)
	p, _, err := d.Policy()
	if err != nil || *p.Limits.MaxDepth != 2 || *p.AgentAPI.Tools || p.Forbid[0] != "rm" {
		t.Fatalf("after the upgrade: %+v %v", p, err)
	}
	in, _ := d.Info()
	if in.Defaults != 1 {
		t.Fatalf("defaults version %d", in.Defaults)
	}
	if _, err := d.SetDefaults(3, "*"); err == nil {
		t.Fatal("a version that does not exist was taken")
	}
	if _, err := d.SetDefaults(2, "0"); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale: %v", err)
	}
	in, err = d.SetDefaults(2, "1")
	if err != nil || in.Defaults != 2 || in.Version != 2 {
		t.Fatalf("move: %+v %v", in, err)
	}
	p, _, _ = d.Policy()
	if *p.Limits.MaxDepth != 5 || !*p.AgentAPI.Tools || p.Forbid[0] != "rm" {
		t.Fatalf("on version 2: %+v", p)
	}
	cs, _, _ := d.Changes()
	if last := cs[len(cs)-1]; last.Setting != SettingDefaults || strings.Join(last.Keys, ",") != "version,permissions.agent_api.tools,permissions.limits.max_depth" {
		t.Fatalf("change %+v", last)
	}
	// A new database starts on the latest.
	n := openTestDB(t, t.TempDir())
	if in, _ := n.Info(); in.Defaults != 2 {
		t.Fatalf("a new database is on %d", in.Defaults)
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
