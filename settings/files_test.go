package settings

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The starter is the default policy written out: FormatPolicy of the
// defaults is the starter, byte for byte.
func TestFormatPolicyStarter(t *testing.T) {
	if got := string(FormatPolicy(DefaultPolicy())); got != PolicyStarter {
		t.Fatalf("FormatPolicy(DefaultPolicy()) differs from the starter:\n%s", got)
	}
}

// A policy written by FormatPolicy reads back the same, every value set
// away from its default.
func TestFormatPolicyRoundTrip(t *testing.T) {
	no, two, half := false, 7, 0.5
	p := Policy{ReadFolders: []string{"~/notes", "/tmp/a b"}, Allow: []string{"go test", `say "hi"`}, Forbid: []string{"git push"},
		Edit: EditNone, Subagents: &no,
		Limits:   Limits{MaxDepth: &two, MaxJobsPerTurn: &two, MaxJobsPerTree: &two, MaxCostUSDPerTree: &half},
		AgentAPI: AgentAPI{Scope: ScopeSession, Read: &no, ListAgents: &no, Start: &no, Continue: &no, Review: &no, Cancel: &no, Rename: &no}}
	b := FormatPolicy(p)
	q, err := ParsePolicy(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	p.fill()
	if !reflect.DeepEqual(p, q) {
		t.Fatalf("read back %+v, wrote %+v\n%s", q, p, b)
	}
	// Whole-number costs stay floats, so the file reads back.
	one := 1.0
	p.Limits.MaxCostUSDPerTree = &one
	if q, err := ParsePolicy(FormatPolicy(p)); err != nil || *q.Limits.MaxCostUSDPerTree != 1 {
		t.Fatalf("cost 1: %v %v", q.Limits.MaxCostUSDPerTree, err)
	}
}

// The file store, end to end: a missing file is the defaults; a write is
// read back; a hand edit is read on the next call, and changes the version;
// a write at an old version is refused; a broken file is an Error that says
// where and what, with its version.
func TestFilesPolicy(t *testing.T) {
	dir := t.TempDir()
	f := NewFiles(dir)
	path := filepath.Join(dir, PolicyFile)

	p, v0, err := f.Policy()
	if err != nil || v0 != noFile || !reflect.DeepEqual(p, withWhere(DefaultPolicy())) {
		t.Fatalf("missing: %+v %q %v", p, v0, err)
	}
	p.Forbid = []string{"git push"}
	stored, v1, err := f.SetPolicy(p, v0)
	if err != nil || v1 == v0 || !slices.Equal(stored.Forbid, []string{"git push"}) {
		t.Fatalf("SetPolicy: %+v %q %v", stored, v1, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the file: %v %v", info, err)
	}
	got, v, err := f.Policy()
	if err != nil || v != v1 || !reflect.DeepEqual(got, stored) {
		t.Fatalf("read back: %+v %q %v", got, v, err)
	}

	// By hand: read on the next call.
	writeFile(t, path, "forbid = [\"rm\"]\nedit = \"none\"\n")
	got, v2, err := f.Policy()
	if err != nil || v2 == v1 || !slices.Equal(got.Forbid, []string{"rm"}) || got.Edit != EditNone {
		t.Fatalf("after a hand edit: %+v %q %v", got, v2, err)
	}
	if _, _, err := f.SetPolicy(DefaultPolicy(), v1); !errors.Is(err, ErrChanged) {
		t.Fatalf("a write over a hand edit: %v, want ErrChanged", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "forbid = [\"rm\"]\nedit = \"none\"\n" {
		t.Fatalf("the refused write changed the file: %q", b)
	}
	// A value out of range is Invalid, and writes nothing.
	bad := DefaultPolicy()
	bad.Edit = "everywhere"
	var inv *Invalid
	if _, _, err := f.SetPolicy(bad, Any); !errors.As(err, &inv) || !strings.Contains(inv.Why, "edit must be") {
		t.Fatalf("an edit value out of range: %v", err)
	}

	// Broken: an Error naming the file and the line, with the version of
	// what is there.
	writeFile(t, path, "# mine\nallow = go test\n")
	_, _, err = f.Policy()
	var se *Error
	if !errors.As(err, &se) || se.Setting != "permissions" || !strings.Contains(se.Where, PolicyFile) || !strings.Contains(se.Why, "line 2") ||
		se.Version == "" || strings.Contains(err.Error(), dir) {
		t.Fatalf("a broken file: %#v", err)
	}
	if _, _, err := f.SetPolicy(DefaultPolicy(), se.Version); err != nil {
		t.Fatalf("replacing a broken file by its version: %v", err)
	}

	// A link: the write goes where it points, and the link stays.
	target := filepath.Join(t.TempDir(), "mine.toml")
	writeFile(t, target, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.SetPolicy(Policy{Allow: []string{"go test"}}, Any); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), `allow = ["go test"]`) {
		t.Fatalf("the link's target: %q", b)
	}
	// No temporary file is left beside it.
	if ents, _ := os.ReadDir(filepath.Dir(target)); len(ents) != 1 {
		t.Errorf("files beside the target: %v", ents)
	}
}

// withWhere is p as the file store hands it out.
func withWhere(p Policy) Policy {
	p.where, p.fix = policyWhere, policyFix
	return p
}

// The instructions: the default when missing, the file's text, a hand edit
// read on the next call, and text that cannot be used refused.
func TestFilesInstructions(t *testing.T) {
	dir := t.TempDir()
	f := NewFiles(dir)
	path := filepath.Join(dir, InstructionsFile)
	text, v0, err := f.Instructions()
	if err != nil || text != DefaultInstructions || v0 != noFile {
		t.Fatalf("missing: %q %q %v", text, v0, err)
	}
	v1, err := f.SetInstructions("Be brief.\n", v0)
	if err != nil {
		t.Fatal(err)
	}
	if text, v, err := f.Instructions(); err != nil || text != "Be brief.\n" || v != v1 {
		t.Fatalf("read back: %q %q %v", text, v, err)
	}
	writeFile(t, path, "")
	if text, v, err := f.Instructions(); err != nil || text != "" || v == v1 {
		t.Fatalf("an empty file: %q %q %v", text, v, err)
	}
	if _, err := f.SetInstructions("x", v1); !errors.Is(err, ErrChanged) {
		t.Fatalf("a write over a hand edit: %v", err)
	}
	var inv *Invalid
	if _, err := f.SetInstructions("a\x00b", Any); !errors.As(err, &inv) {
		t.Fatalf("a NUL: %v", err)
	}
	writeFile(t, path, "a\xffb")
	var se *Error
	if _, _, err := f.Instructions(); !errors.As(err, &se) || !strings.Contains(se.Why, "UTF-8") || se.Fix == "" {
		t.Fatalf("not UTF-8: %v", err)
	}
}

// Init writes each missing file once, mode 0600, with the starter text, and
// never writes an existing one again. Protected names the data folder's own
// files.
func TestFilesInit(t *testing.T) {
	dir := t.TempDir()
	f := NewFiles(dir)
	if err := f.Init(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ file, text string }{{PolicyFile, PolicyStarter}, {InstructionsFile, DefaultInstructions}} {
		path := filepath.Join(dir, tc.file)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", tc.file, info, err)
		}
		if b, _ := os.ReadFile(path); string(b) != tc.text {
			t.Errorf("%s: starter %q", tc.file, b)
		}
		writeFile(t, path, "mine")
	}
	if err := f.Init(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{PolicyFile, InstructionsFile} {
		if b, _ := os.ReadFile(filepath.Join(dir, file)); string(b) != "mine" {
			t.Errorf("%s: an existing file was written: %q", file, b)
		}
	}
	want := []string{PolicyFile, InstructionsFile, AgentsFile, "token", "turns_token", "enroll_token", "events.db", "jobs.db", "runner.json", "folders.json", "bin/" + APIClient}
	for i := range want {
		want[i] = filepath.Join(dir, want[i])
	}
	if got := f.Protected(); !slices.Equal(got, want) {
		t.Errorf("Protected %q, want %q", got, want)
	}
}

// A runner's store reads the server's settings with its own token, refuses
// writes, and turns every way of not having them into an Error that says
// why: not enrolled, a server that cannot be reached, one without the route,
// and one whose settings cannot be used.
func TestRemote(t *testing.T) {
	var status int
	var sawToken string
	var gone bool // the server has no settings route
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("Authorization")
		if gone || r.URL.Path != "/api/v0/runner/settings" || r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		if status != 200 {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"the permissions file permissions.toml in the data folder cannot be used: line 2"}`))
			return
		}
		p := DefaultPolicy()
		p.Forbid = []string{"git push"}
		p.ReadFolders = []string{"~"}
		json.NewEncoder(w).Encode(RunnerSettings{Permissions: p, PermissionsVersion: "v1", Instructions: "Hi {session}", InstructionsVersion: "v2"})
	}))
	defer srv.Close()
	r := &Remote{Server: srv.URL, Local: "/data"}
	var se *Error
	if _, _, err := r.Policy(); !errors.As(err, &se) || !strings.Contains(se.Why, "not enrolled") {
		t.Fatalf("before enrolling: %v", err)
	}
	r.Enrolled("rn_1", "rt_secret")
	status = 200
	p, v, err := r.Policy()
	if err != nil || v != "v1" || !slices.Equal(p.Forbid, []string{"git push"}) || sawToken != "Bearer rt_secret" {
		t.Fatalf("Policy: %+v %q %v %q", p, v, err, sawToken)
	}
	// ~ is this machine's home; a folder that is not here says so.
	if q, err := p.Resolve("/"); err != nil || !slices.Equal(q.ReadFolders, []string{"/"}) {
		t.Fatalf("Resolve: %+v %v", q, err)
	}
	if _, err := p.Resolve(filepath.Join(t.TempDir(), "nope")); !errors.As(err, &se) || !strings.Contains(se.Where, "on this runner") {
		t.Fatalf("a folder not on this runner: %v", err)
	}
	if text, v, err := r.Instructions(); err != nil || text != "Hi {session}" || v != "v2" {
		t.Fatalf("Instructions: %q %q %v", text, v, err)
	}
	if _, _, err := r.SetPolicy(p, Any); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SetPolicy: %v", err)
	}
	if _, err := r.SetInstructions("x", Any); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SetInstructions: %v", err)
	}
	if got := r.Protected(); len(got) == 0 || got[0] != filepath.Join("/data", PolicyFile) {
		t.Fatalf("Protected: %q", got)
	}
	status = 503
	if _, _, err := r.Policy(); !errors.As(err, &se) || !strings.Contains(se.Why, "line 2") || se.Where != remoteWhere {
		t.Fatalf("broken on the server: %v", err)
	}
	gone = true
	if _, _, err := r.Instructions(); !errors.As(err, &se) || !strings.Contains(se.Why, "404") {
		t.Fatalf("no route: %v", err)
	}
	srv.Close()
	if _, _, err := r.Policy(); !errors.As(err, &se) || !strings.Contains(se.Why, "could not be reached") || strings.Contains(err.Error(), "rt_secret") {
		t.Fatalf("unreachable: %v", err)
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}
