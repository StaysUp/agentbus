package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// testAgents reads an agents.json as the agents package does, as rows of
// its own: every agent kept whole.
func testAgents(b []byte) ([]AgentRow, error) {
	var f struct {
		Agents []json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	var out []AgentRow
	for _, a := range f.Agents {
		var id struct{ ID string }
		if err := json.Unmarshal(a, &id); err != nil || id.ID == "" {
			return nil, errors.New("an agent without an id")
		}
		out = append(out, AgentRow{ID: id.ID, Data: a})
	}
	return out, nil
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// home is a data folder in a parent of its own, so its copy lands beside it
// in the test's folder.
func home(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "agentbus")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// copyOf is where the upgrade copies dir: beside its real path.
func copyOf(t *testing.T, dir string) string {
	t.Helper()
	real, err := realPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	return CopyName(real, fixedNow())
}

func upgrade(dir string) Upgrade {
	return Upgrade{Dir: dir, Lock: filepath.Join(dir, "events.db.lock"), Skip: []string{"sessions", "blobs"}, Agents: testAgents, Now: fixedNow}
}

func copies(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(filepath.Dir(dir), "*"+CopySuffix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A new data folder: no copy, nothing imported, the import marked done, and
// no settings file written.
func TestUpgradeNewFolder(t *testing.T) {
	dir := home(t)
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if rep.Copy != "" || len(rep.Imported) != 0 || !rep.Done || len(copies(t, dir)) != 0 {
		t.Fatalf("report %+v, copies %v", rep, copies(t, dir))
	}
	for _, f := range []string{PolicyFile, InstructionsFile, AgentsFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s was written", f)
		}
	}
	in, err := d.Info()
	if err != nil || in.Imported.IsZero() || len(in.Files) != 0 || in.Version != 0 {
		t.Fatalf("info %+v %v", in, err)
	}
	if p, _, err := d.Policy(); err != nil || *p.Limits.MaxDepth != DefaultMaxDepth {
		t.Fatalf("policy %+v %v", p, err)
	}
}

// A new data folder starts with the agent rows Starters gives, written with
// the import, also on a start after one that stopped before the import. A
// later start, and a folder with a service's files already, never ask.
func TestUpgradeStarters(t *testing.T) {
	rows := []AgentRow{{ID: "codex", Removed: true}, {ID: "agy", Removed: true}}
	ids := func(d *DB) string {
		t.Helper()
		got, err := d.Agents()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range got {
			if !r.Removed {
				t.Fatalf("row %+v is not removed", r)
			}
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	for _, stopped := range []bool{false, true} {
		dir := home(t)
		if stopped {
			// The database made, and the start stopped before the import.
			d, err := OpenDB(filepath.Join(dir, DBFile), fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
		}
		u := upgrade(dir)
		u.Starters = func() []AgentRow { return rows }
		d, _, err := u.Run()
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(d); got != "codex,agy" {
			t.Fatalf("stopped %v: agents %q", stopped, got)
		}
		if in, _ := d.Info(); in.Version != 0 {
			t.Fatalf("stopped %v: version %d, want 0", stopped, in.Version)
		}
		d.Close()
		u.Starters = func() []AgentRow { t.Fatal("asked again"); return nil }
		if d, _, err = u.Run(); err != nil {
			t.Fatal(err)
		}
		if got := ids(d); got != "codex,agy" {
			t.Fatalf("stopped %v: the next start changed the agents to %q", stopped, got)
		}
		d.Close()
	}

	dir := home(t)
	write(t, filepath.Join(dir, "events.db"), "record")
	u := upgrade(dir)
	u.Starters = func() []AgentRow { t.Fatal("asked for a folder of 0.4"); return nil }
	d, _, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got := ids(d); got != "" {
		t.Fatalf("a folder of 0.4 got agent rows %q", got)
	}
}

// A data folder of 0.4: copied beside it first, the session copies and blobs
// left out; then its files imported, each as a change naming its file and
// keys. The files stay, and are never read again.
func TestUpgradeImportsFiles(t *testing.T) {
	dir := home(t)
	write(t, filepath.Join(dir, PolicyFile), "forbid = [\"git push\"]\n[limits]\nmax_depth = 1\n")
	write(t, filepath.Join(dir, InstructionsFile), "Be brief.\n")
	write(t, filepath.Join(dir, AgentsFile), `{"agents":[{"id":"a_1","name":"Mine"}]}`)
	write(t, filepath.Join(dir, "events.db"), "record")
	write(t, filepath.Join(dir, "token"), "tok")
	write(t, filepath.Join(dir, "logs", "service.log"), "log")
	write(t, filepath.Join(dir, "sessions", "s1", "big"), "copy")
	write(t, filepath.Join(dir, "blobs", "b1"), "blob")
	if err := os.Symlink("token", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	want := copyOf(t, dir)
	if rep.Copy != want || !strings.HasSuffix(want, "agentbus-2026-11-12-before-settings-db") || !rep.Done ||
		strings.Join(rep.Imported, ",") != "permissions.toml,instructions.md,agents.json" {
		t.Fatalf("report %+v", rep)
	}
	for _, f := range []string{PolicyFile, InstructionsFile, AgentsFile, "events.db", "token", "logs/service.log"} {
		a, _ := os.ReadFile(filepath.Join(dir, f))
		b, err := os.ReadFile(filepath.Join(want, f))
		if err != nil || string(a) != string(b) {
			t.Errorf("copy of %s: %q %v", f, b, err)
		}
	}
	if to, err := os.Readlink(filepath.Join(want, "link")); err != nil || to != "token" {
		t.Errorf("the link: %q %v", to, err)
	}
	for _, f := range []string{"sessions", "blobs", DBFile} {
		if _, err := os.Lstat(filepath.Join(want, f)); err == nil {
			t.Errorf("%s was copied", f)
		}
	}
	if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the copy's mode: %v %v", fi, err)
	}

	p, _, err := d.Policy()
	if err != nil || p.Forbid[0] != "git push" || *p.Limits.MaxDepth != 1 || *p.Limits.MaxJobsPerTurn != 10 {
		t.Fatalf("policy %+v %v", p, err)
	}
	if text, _, _ := d.Instructions(); text != "Be brief.\n" {
		t.Fatalf("instructions %q", text)
	}
	if rows, _ := d.Agents(); len(rows) != 1 || rows[0].ID != "a_1" {
		t.Fatalf("agents %+v", rows)
	}
	cs, pub, _ := d.Changes()
	if len(cs) != 3 || cs[0].File != PolicyFile || strings.Join(cs[0].Keys, ",") != "agent_api.apply,forbid,limits.max_depth" ||
		cs[1].File != InstructionsFile || cs[2].File != AgentsFile || cs[2].Keys[0] != "a_1" || cs[2].Version != 3 || pub[0] {
		t.Fatalf("changes %+v %v", cs, pub)
	}
	if b, _ := json.Marshal(cs); strings.Contains(string(b), "git push") || strings.Contains(string(b), "brief") {
		t.Fatalf("a change holds a value: %s", b)
	}
	// The files stay where they were.
	if b, _ := os.ReadFile(filepath.Join(dir, InstructionsFile)); string(b) != "Be brief.\n" {
		t.Fatalf("the file changed: %q", b)
	}
	d.Close()

	// The second start: no copy, no import, and a hand edit of a file is
	// never read.
	write(t, filepath.Join(dir, PolicyFile), "forbid = [\"rm\"]\n")
	d, rep, err = upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if rep.Copy != "" || rep.Done || len(rep.Imported) != 0 || len(copies(t, dir)) != 1 {
		t.Fatalf("second start %+v %v", rep, copies(t, dir))
	}
	if p, _, _ := d.Policy(); p.Forbid[0] != "git push" {
		t.Fatalf("the file was read again: %+v", p.Forbid)
	}
	if cs, _, _ := d.Changes(); len(cs) != 3 {
		t.Fatalf("%d changes after the second start", len(cs))
	}
}

// A start that stopped anywhere starts over: a half-made copy is made again,
// a whole copy made before the database is kept, and an import that failed
// stored nothing and runs again once the file is fixed.
func TestUpgradeInterrupted(t *testing.T) {
	dir := home(t)
	write(t, filepath.Join(dir, "events.db"), "record")
	want := copyOf(t, dir)

	// Stopped half way through the copy.
	write(t, filepath.Join(want+".partial", "stale"), "half")
	// A file the import cannot use stops it after the copy.
	write(t, filepath.Join(dir, PolicyFile), "allow = go test\n")
	if _, _, err := upgrade(dir).Run(); err == nil || !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), PolicyFile) {
		t.Fatalf("a broken file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(want, "stale")); err == nil {
		t.Fatal("the half-made copy was kept")
	}
	if _, err := os.Stat(filepath.Join(want, "events.db")); err != nil {
		t.Fatal("no copy made")
	}
	if _, err := os.Stat(want + ".partial"); err == nil {
		t.Fatal("the partial copy is still there")
	}
	// Nothing of the import is stored.
	d, err := OpenDB(filepath.Join(dir, DBFile), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if in, _ := d.Info(); !in.Imported.IsZero() || in.Version != 0 {
		t.Fatalf("a failed import stored %+v", in)
	}
	d.Close()

	// Fixed: the next start imports, and keeps the copy it has.
	write(t, filepath.Join(dir, "marker"), "after the copy")
	write(t, filepath.Join(dir, PolicyFile), "allow = [\"go test\"]\n")
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if rep.Copy != "" || !rep.Done || len(copies(t, dir)) != 1 {
		t.Fatalf("after the fix %+v %v", rep, copies(t, dir))
	}
	if _, err := os.Stat(filepath.Join(want, "marker")); err == nil {
		t.Fatal("the copy was made again")
	}
	if p, _, _ := d.Policy(); len(p.Allow) != 1 || p.Allow[0] != "go test" {
		t.Fatalf("policy %+v", p.Allow)
	}
}

// Stopped after the copy was whole but before the database was made: the
// copy is kept, and not made again.
func TestUpgradeCopyBeforeDatabase(t *testing.T) {
	dir := home(t)
	write(t, filepath.Join(dir, "events.db"), "record")
	want := copyOf(t, dir)
	write(t, filepath.Join(want, "events.db"), "the first copy")
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if rep.Copy != want {
		t.Fatalf("report %+v", rep)
	}
	if b, _ := os.ReadFile(filepath.Join(want, "events.db")); string(b) != "the first copy" {
		t.Fatalf("the copy was made again: %q", b)
	}
}

// Another service on the data folder holds its lock: the upgrade does not
// start.
func TestUpgradeRefusedWhileAnotherServiceRuns(t *testing.T) {
	dir := home(t)
	write(t, filepath.Join(dir, PolicyFile), "")
	f, err := os.OpenFile(filepath.Join(dir, "events.db.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, _, err := upgrade(dir).Run(); err == nil || !strings.Contains(err.Error(), "another agentbus service") {
		t.Fatalf("got %v", err)
	}
	if len(copies(t, dir)) != 0 {
		t.Fatal("a copy was made")
	}
	if _, err := os.Stat(filepath.Join(dir, DBFile)); err == nil {
		t.Fatal("the database was made")
	}
}

// A data folder reached through a link is copied as a folder of its own:
// the copy holds its files, not a link back to the live folder, so a write
// after the upgrade never shows in the copy.
func TestUpgradeLinkedFolder(t *testing.T) {
	real := home(t)
	write(t, filepath.Join(real, PolicyFile), "forbid = [\"git push\"]\n")
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	d, rep, err := upgrade(link).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if rep.Copy != copyOf(t, real) {
		t.Fatalf("copy %q, want beside the real folder", rep.Copy)
	}
	if fi, err := os.Lstat(rep.Copy); err != nil || !fi.IsDir() {
		t.Fatalf("the copy is not a folder: %v %v", fi, err)
	}
	write(t, filepath.Join(real, "later"), "after the upgrade")
	if _, err := os.Stat(filepath.Join(rep.Copy, "later")); err == nil {
		t.Fatal("a write after the upgrade is in the copy")
	}
	if _, err := os.Stat(filepath.Join(rep.Copy, DBFile)); err == nil {
		t.Fatal("the new database is in the copy")
	}
	if b, err := os.ReadFile(filepath.Join(rep.Copy, PolicyFile)); err != nil || !strings.Contains(string(b), "git push") {
		t.Fatalf("the copy's file: %q %v", b, err)
	}
}

// A data folder given as "." is copied beside itself, by its own name.
func TestUpgradeRelativeFolder(t *testing.T) {
	dir := home(t)
	write(t, filepath.Join(dir, PolicyFile), "")
	t.Chdir(dir)
	u := upgrade(".")
	u.Lock = "events.db.lock"
	d, rep, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if rep.Copy != copyOf(t, dir) || filepath.Base(rep.Copy) != "agentbus-2026-11-12-before-settings-db" {
		t.Fatalf("copy %q, want beside %q", rep.Copy, dir)
	}
	if _, err := os.Stat(filepath.Join(rep.Copy, PolicyFile)); err != nil {
		t.Fatal(err)
	}
}

// A data folder named through a link inside itself (alias -> data,
// data/current -> ., AGENTBUS_HOME=alias/current) is copied beside the real
// folder, never inside it.
func TestUpgradeFolderNamedThroughItself(t *testing.T) {
	data := home(t)
	write(t, filepath.Join(data, PolicyFile), "")
	if err := os.Symlink(".", filepath.Join(data, "current")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(data, alias); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(alias, "current")
	u := upgrade(named)
	d, rep, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if rep.Copy != copyOf(t, data) {
		t.Fatalf("copy %q, want beside the real folder", rep.Copy)
	}
	if _, err := os.Stat(filepath.Join(rep.Copy, PolicyFile)); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(data, "*"+CopySuffix+"*")); len(m) != 0 {
		t.Fatalf("a copy inside the folder: %v", m)
	}
}

// A data folder from before settings.db keeps the defaults 0.4 ran under,
// so a turn token still applies: the old value is written out as the
// person's own. A key its permissions.toml sets keeps the file's value. A new
// folder starts on the defaults as they are, where it does not.
func TestUpgradeKeepsDefaultsOf04(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		apply bool
	}{
		{"new", nil, false},
		{"record only", map[string]string{"events.db": "record"}, true},
		{"file without apply", map[string]string{PolicyFile: "forbid = [\"rm\"]\n"}, true},
		{"file with apply off", map[string]string{PolicyFile: "[agent_api]\napply = false\n"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := home(t)
			for f, s := range tc.files {
				write(t, filepath.Join(dir, f), s)
			}
			d, _, err := upgrade(dir).Run()
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			p, _, err := d.Policy()
			if err != nil || *p.AgentAPI.Apply != tc.apply {
				t.Fatalf("apply %v, %v; want %v", *p.AgentAPI.Apply, err, tc.apply)
			}
			if strings.Contains(stored(t, d, SettingPermissions), `"agent_api.apply":true`) != tc.apply {
				t.Fatalf("stored %s", stored(t, d, SettingPermissions))
			}
			if n, _ := upgradesDone(d.db); n != len(upgradeSteps) {
				t.Fatalf("%d steps done", n)
			}
		})
	}
}

// legacyDB is a settings database as the build before this one left it: on
// defaults version dv, imported, with overlay as the stored policy.
func legacyDB(t *testing.T, dir string, dv int, overlay string) {
	t.Helper()
	d, err := openDB(filepath.Join(dir, DBFile), fixedNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM meta WHERE key = 'upgrades'`,
		`INSERT INTO meta(key, value) VALUES ('defaults', '` + strconv.Itoa(dv) + `'), ('imported', '2026-11-12T00:00:00Z'), ('imported_files', '[]')`,
		`INSERT INTO settings(name, value, updated) VALUES ('permissions', '` + overlay + `', '')`,
	} {
		if _, err := d.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
}

// A settings.db on defaults version 1 (0.4's), where the person never set
// apply: the first start writes apply = true once, as a change of its own
// with its settings.changed event, and the version is gone from storage.
// A second start does nothing. The person can then turn it off, and it
// stays off.
func TestUpgradeFromDefaultsVersion1(t *testing.T) {
	dir := home(t)
	legacyDB(t, dir, 1, `{"forbid":["rm"]}`)
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Kept, ",") != "agent_api.apply" {
		t.Fatalf("kept %v", rep.Kept)
	}
	var got []Change
	d.SetSink(func(c Change) error { got = append(got, c); return nil }, nil)
	p, v, err := d.Policy()
	if err != nil || !*p.AgentAPI.Apply || p.Forbid[0] != "rm" {
		t.Fatalf("after the upgrade: apply %v forbid %v %v", *p.AgentAPI.Apply, p.Forbid, err)
	}
	if s := stored(t, d, SettingPermissions); s != `{"agent_api.apply":true,"forbid":["rm"]}` {
		t.Fatalf("stored %s", s)
	}
	if len(got) != 1 || got[0].Setting != SettingPermissions || strings.Join(got[0].Keys, ",") != "agent_api.apply" || got[0].File != "" || got[0].Version != 1 {
		t.Fatalf("events %+v", got)
	}
	if dv, _ := meta(d.db, "defaults"); dv != "" {
		t.Fatalf("the defaults version is still stored: %q", dv)
	}
	d.Close()

	d, rep, err = upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Kept) != 0 {
		t.Fatalf("second start kept %v", rep.Kept)
	}
	if cs, _, _ := d.Changes(); len(cs) != 1 {
		t.Fatalf("second start: %d changes", len(cs))
	}
	off := false
	p.AgentAPI.Apply = &off
	if _, _, err := d.SetPolicy(p, v); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, _, err = upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if p, _, _ := d.Policy(); *p.AgentAPI.Apply {
		t.Fatal("turned off, then on again by a start")
	}
	if cs, _, _ := d.Changes(); len(cs) != 2 {
		t.Fatalf("%d changes, want the upgrade and the person's", len(cs))
	}
}

// A settings.db on version 1 where the person set apply off keeps it, and
// one on version 2 (made new by the build before) is on today's defaults:
// neither writes a change.
func TestUpgradeFromDefaultsVersionNothingToKeep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dv      int
		overlay string
	}{
		{"version 1, apply off", 1, `{"agent_api.apply":false}`},
		{"version 2", 2, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := home(t)
			legacyDB(t, dir, tc.dv, tc.overlay)
			d, rep, err := upgrade(dir).Run()
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if p, _, _ := d.Policy(); *p.AgentAPI.Apply || len(rep.Kept) != 0 {
				t.Fatalf("apply %v kept %v", *p.AgentAPI.Apply, rep.Kept)
			}
			if cs, _, _ := d.Changes(); len(cs) != 0 {
				t.Fatalf("%d changes", len(cs))
			}
			if dv, _ := meta(d.db, "defaults"); dv != "" {
				t.Fatalf("the defaults version is still stored: %q", dv)
			}
		})
	}
}

// storedDB is a settings database as the build before this one left it (one
// upgrade step written, imported), with overlay as the stored policy.
func storedDB(t *testing.T, dir, overlay string) {
	t.Helper()
	d, err := openDB(filepath.Join(dir, DBFile), fixedNow, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO meta(key, value) VALUES ('imported', '2026-11-12T00:00:00Z'), ('imported_files', '[]')`,
		`INSERT INTO settings(name, value, updated) VALUES ('permissions', '` + overlay + `', '')`,
	} {
		if _, err := d.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
}

// A stored policy that holds commands, retired in 0.5, loses it on the first
// start, as one change whose settings.changed event names the key, and keeps
// the rest. A second start does nothing.
func TestUpgradeRemovesRetiredKey(t *testing.T) {
	dir := home(t)
	storedDB(t, dir, `{"commands":false,"forbid":["rm"]}`)
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Removed, ",") != "commands" || len(rep.Kept) != 0 {
		t.Fatalf("removed %v kept %v", rep.Removed, rep.Kept)
	}
	var got []Change
	d.SetSink(func(c Change) error { got = append(got, c); return nil }, nil)
	if s := stored(t, d, SettingPermissions); s != `{"forbid":["rm"]}` {
		t.Fatalf("stored %s", s)
	}
	if len(got) != 1 || got[0].Setting != SettingPermissions || strings.Join(got[0].Keys, ",") != "commands" || got[0].File != "" || got[0].Version != 1 {
		t.Fatalf("events %+v", got)
	}
	if p, _, err := d.Policy(); err != nil || p.Forbid[0] != "rm" {
		t.Fatalf("policy %+v %v", p, err)
	}
	if n, _ := upgradesDone(d.db); n != len(upgradeSteps) {
		t.Fatalf("%d steps done", n)
	}
	d.Close()

	d, rep, err = upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if len(rep.Removed) != 0 || len(rep.Kept) != 0 {
		t.Fatalf("second start: removed %v kept %v", rep.Removed, rep.Kept)
	}
	if cs, _, _ := d.Changes(); len(cs) != 1 {
		t.Fatalf("second start: %d changes", len(cs))
	}
}

// A settings.db that 0.5 wrote, with its two steps done and
// subscriptions_on_runners stored (the cockpit's "Turn on for my runners"),
// loses the key on the first start of 0.5.1: one change whose
// settings.changed event names it, the rest kept. A second start does
// nothing.
func TestUpgradeRemovesSubscriptionsOnRunners(t *testing.T) {
	dir := home(t)
	storedDB(t, dir, `{"subscriptions_on_runners":"own","forbid":["rm"]}`)
	d, err := openDB(filepath.Join(dir, DBFile), fixedNow, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`INSERT INTO meta(key, value) VALUES ('upgrades', '2') ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Removed, ",") != "subscriptions_on_runners" || len(rep.Kept) != 0 {
		t.Fatalf("removed %v kept %v", rep.Removed, rep.Kept)
	}
	var got []Change
	d.SetSink(func(c Change) error { got = append(got, c); return nil }, nil)
	if s := stored(t, d, SettingPermissions); s != `{"forbid":["rm"]}` {
		t.Fatalf("stored %s", s)
	}
	if len(got) != 1 || got[0].Setting != SettingPermissions || strings.Join(got[0].Keys, ",") != "subscriptions_on_runners" || got[0].File != "" {
		t.Fatalf("events %+v", got)
	}
	if p, _, err := d.Policy(); err != nil || p.Forbid[0] != "rm" {
		t.Fatalf("policy %+v %v", p, err)
	}
	d.Close()

	d, rep, err = upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if len(rep.Removed) != 0 || len(rep.Kept) != 0 {
		t.Fatalf("second start: removed %v kept %v", rep.Removed, rep.Kept)
	}
	if cs, _, _ := d.Changes(); len(cs) != 1 {
		t.Fatalf("second start: %d changes", len(cs))
	}
}

// A 0.4 permissions.toml that sets subscriptions_on_runners imports without
// error, and the stored policy never holds it.
func TestUpgradeImportsFileWithSubscriptionsOnRunners(t *testing.T) {
	for _, v := range []string{`"own"`, `"off"`} {
		dir := home(t)
		write(t, filepath.Join(dir, PolicyFile), "subscriptions_on_runners = "+v+"\nforbid = [\"rm\"]\n")
		d, rep, err := upgrade(dir).Run()
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if strings.Join(rep.Imported, ",") != PolicyFile || len(rep.Removed) != 0 {
			t.Fatalf("%s: imported %v removed %v", v, rep.Imported, rep.Removed)
		}
		if s := stored(t, d, SettingPermissions); strings.Contains(s, "subscriptions") || !strings.Contains(s, `"forbid":["rm"]`) {
			t.Fatalf("%s: stored %s", v, s)
		}
		d.Close()
	}
}

// A stored policy without a retired key is not written, and makes no change.
func TestUpgradeNoRetiredKey(t *testing.T) {
	dir := home(t)
	storedDB(t, dir, `{"forbid":["rm"]}`)
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if len(rep.Removed) != 0 || len(rep.Kept) != 0 {
		t.Fatalf("removed %v kept %v", rep.Removed, rep.Kept)
	}
	if cs, _, _ := d.Changes(); len(cs) != 0 {
		t.Fatalf("%d changes", len(cs))
	}
	if s := stored(t, d, SettingPermissions); s != `{"forbid":["rm"]}` {
		t.Fatalf("stored %s", s)
	}
	if n, _ := upgradesDone(d.db); n != len(upgradeSteps) {
		t.Fatalf("%d steps done", n)
	}
}

// A settings.db on 0.4's defaults version that holds commands and never set
// apply: both steps run in one change naming both keys.
func TestUpgradeKeepsAndRemovesInOneChange(t *testing.T) {
	dir := home(t)
	legacyDB(t, dir, 1, `{"commands":false}`)
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if strings.Join(rep.Kept, ",") != "agent_api.apply" || strings.Join(rep.Removed, ",") != "commands" {
		t.Fatalf("kept %v removed %v", rep.Kept, rep.Removed)
	}
	if s := stored(t, d, SettingPermissions); s != `{"agent_api.apply":true}` {
		t.Fatalf("stored %s", s)
	}
	cs, _, err := d.Changes()
	if err != nil || len(cs) != 1 || strings.Join(cs[0].Keys, ",") != "agent_api.apply,commands" {
		t.Fatalf("changes %+v %v", cs, err)
	}
}

// A 0.4 permissions.toml that still sets commands imports as if it did not:
// the stored policy never holds it, and no step removes anything after.
func TestUpgradeImportsFileWithRetiredKey(t *testing.T) {
	dir := home(t)
	write(t, filepath.Join(dir, PolicyFile), "commands = true\nforbid = [\"rm\"]\n")
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if strings.Join(rep.Imported, ",") != PolicyFile || len(rep.Removed) != 0 {
		t.Fatalf("imported %v removed %v", rep.Imported, rep.Removed)
	}
	if s := stored(t, d, SettingPermissions); strings.Contains(s, "commands") || !strings.Contains(s, `"forbid":["rm"]`) {
		t.Fatalf("stored %s", s)
	}
	cs, _, err := d.Changes()
	if err != nil || len(cs) != 1 || cs[0].File != PolicyFile || slices.Contains(cs[0].Keys, "commands") {
		t.Fatalf("changes %+v %v", cs, err)
	}
}

// A settings.db that 0.5.1 wrote, with its four steps done and sandbox.read
// stored, has the value moved to home_files on the first start of 0.5.2:
// one change whose settings.changed event names both keys, the rest kept.
// A second start does nothing, and the policy keeps the files.
func TestUpgradeMovesSandboxRead(t *testing.T) {
	dir := home(t)
	storedDB(t, dir, `{"sandbox.mode":"on","sandbox.read":["~/.aws/config"],"forbid":["rm"]}`)
	d, err := openDB(filepath.Join(dir, DBFile), fixedNow, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`INSERT INTO meta(key, value) VALUES ('upgrades', '4') ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Removed, ",") != "sandbox.read" || strings.Join(rep.Moved, ",") != "home_files" || len(rep.Kept) != 0 {
		t.Fatalf("removed %v moved %v kept %v", rep.Removed, rep.Moved, rep.Kept)
	}
	var got []Change
	d.SetSink(func(c Change) error { got = append(got, c); return nil }, nil)
	if s := stored(t, d, SettingPermissions); s != `{"forbid":["rm"],"home_files":["~/.aws/config"],"sandbox.mode":"on"}` {
		t.Fatalf("stored %s", s)
	}
	if len(got) != 1 || got[0].Setting != SettingPermissions || strings.Join(got[0].Keys, ",") != "sandbox.read,home_files" {
		t.Fatalf("events %+v", got)
	}
	if p, _, err := d.Policy(); err != nil || strings.Join(p.HomeFiles, ",") != "~/.aws/config" || p.Sandbox.Mode != SandboxOn {
		t.Fatalf("policy %+v %v", p, err)
	}
	d.Close()

	d, rep, err = upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if len(rep.Removed) != 0 || len(rep.Moved) != 0 || len(rep.Kept) != 0 {
		t.Fatalf("second start: removed %v moved %v kept %v", rep.Removed, rep.Moved, rep.Kept)
	}
	if cs, _, _ := d.Changes(); len(cs) != 1 {
		t.Fatalf("second start: %d changes", len(cs))
	}
	if p, _, err := d.Policy(); err != nil || strings.Join(p.HomeFiles, ",") != "~/.aws/config" {
		t.Fatalf("second start: policy %+v %v", p, err)
	}
}

// A 0.4 permissions.toml with [sandbox] read imports with the value as
// home_files, and the stored policy never holds sandbox.read.
func TestUpgradeImportsFileWithSandboxRead(t *testing.T) {
	dir := home(t)
	write(t, filepath.Join(dir, PolicyFile), "[sandbox]\nread = [\"~/.aws/config\"]\n")
	d, rep, err := upgrade(dir).Run()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if strings.Join(rep.Imported, ",") != PolicyFile || len(rep.Removed) != 0 || len(rep.Moved) != 0 {
		t.Fatalf("imported %v removed %v moved %v", rep.Imported, rep.Removed, rep.Moved)
	}
	if s := stored(t, d, SettingPermissions); strings.Contains(s, "sandbox.read") || !strings.Contains(s, `"home_files":["~/.aws/config"]`) {
		t.Fatalf("stored %s", s)
	}
}
