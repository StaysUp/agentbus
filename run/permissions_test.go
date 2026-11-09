package run

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

func writePerm(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The exact arguments each agent gets for a policy, with and without
// read_only, an edit rule, resume and model.
func TestTurnArgsPermissions(t *testing.T) {
	perm := settings.Policy{ReadFolders: []string{"/home/me/.agents", "/home/me/notes"},
		Allow: []string{"go test", "make test"}, Forbid: []string{"git push", "git checkout"}}
	const reads = "Read(//home/me/.agents/**),Grep(//home/me/.agents/**),Glob(//home/me/.agents/**)," +
		"Read(//home/me/notes/**),Grep(//home/me/notes/**),Glob(//home/me/notes/**)"
	allow := []string{"--allowedTools", reads + ",Bash(go test:*),Bash(make test:*)"}
	forbid := []string{"--disallowedTools", "Bash(git push:*),Bash(git checkout:*)"}
	// No edit rule: every tool that edits files is denied, whatever the
	// person's own settings allow.
	noEdit := []string{"--disallowedTools", "Bash(git push:*),Bash(git checkout:*),Edit,Write,NotebookEdit"}
	noEditOnly := []string{"--disallowedTools", "Edit,Write,NotebookEdit"}
	readOnly := []string{"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob," + reads, "--strict-mcp-config"}
	cat := func(parts ...[]string) []string { return slices.Concat(parts...) }
	head := []string{"-p", "--output-format", "json"}
	edit := Writes{Edit: []string{"/w/repo"}, Protect: []string{"/data/permissions.toml", "/data/token"}}
	ro := Writes{ReadOnly: true, Protect: edit.Protect}
	for _, tc := range []struct {
		name, agent, resume, model string
		w                          Writes
		perm                       settings.Policy
		want                       []string
	}{
		{"claude, no policy, no edits", "claude", "", "", Writes{}, settings.Policy{}, cat(head, noEditOnly, []string{"--", "hi"})},
		{"claude", "claude", "", "", Writes{}, perm, cat(head, allow, noEdit, []string{"--", "hi"})},
		// The edit rule: Edit and Write in the session's folder, and a deny
		// on each protected file, which wins over it and over forbid's
		// neighbours in the same list.
		{"claude edits its folder", "claude", "", "", edit, perm, cat(head,
			[]string{"--allowedTools", reads + ",Edit(//w/repo/**),Bash(go test:*),Bash(make test:*)",
				"--disallowedTools", "Bash(git push:*),Bash(git checkout:*),Edit(//data/permissions.toml),Edit(//data/token)", "--", "hi"})},
		{"claude edits, no other policy", "claude", "", "", edit, settings.Policy{}, cat(head,
			[]string{"--allowedTools", "Edit(//w/repo/**)", "--disallowedTools", "Edit(//data/permissions.toml),Edit(//data/token)", "--", "hi"})},
		// read_folders never becomes --add-dir: that would let a turn write
		// there under the person's acceptEdits mode.
		{"claude, folders only", "claude", "", "", Writes{}, settings.Policy{ReadFolders: []string{"/x"}},
			cat(head, []string{"--allowedTools", "Read(//x/**),Grep(//x/**),Glob(//x/**)"}, noEditOnly, []string{"--", "hi"})},
		{"claude read-only, no policy", "claude", "", "", ro, settings.Policy{},
			cat(head, []string{"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob", "--strict-mcp-config", "--", "hi"})},
		{"claude, allow only", "claude", "", "", Writes{}, settings.Policy{Allow: []string{"go test"}},
			cat(head, []string{"--allowedTools", "Bash(go test:*)"}, noEditOnly, []string{"--", "hi"})},
		// allow never widens a read-only turn; folders and forbid still apply.
		{"claude read-only", "claude", "", "", ro, perm, cat(head, readOnly, forbid, []string{"--", "hi"})},
		{"claude resume and model", "claude", "r-1", "opus", Writes{}, perm,
			cat(head, []string{"--resume", "r-1"}, allow, noEdit, []string{"--model", "opus", "--", "hi"})},
		{"claude read-only, resume and model", "claude", "r-1", "opus", ro, perm,
			cat(head, []string{"--resume", "r-1"}, readOnly, forbid, []string{"--model", "opus", "--", "hi"})},
		// codex writes, with network, only with the edit rule.
		{"codex edits", "codex", "", "", edit, perm, []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "workspace-write",
			"-c", "sandbox_workspace_write.network_access=true", "--", "hi"}},
		{"codex, no edit rule", "codex", "", "", Writes{}, perm, []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "read-only", "--", "hi"}},
		{"codex read-only", "codex", "", "", ro, perm, []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "read-only", "--", "hi"}},
		{"codex resume and model", "codex", "r-1", "gpt-5", edit, perm, []string{"exec", "--skip-git-repo-check", "--sandbox", "workspace-write",
			"-c", "sandbox_workspace_write.network_access=true", "resume", "--json", "--model", "gpt-5", "--", "r-1", "hi"}},
	} {
		got := turnArgs(hn(tc.agent), "", tc.resume, tc.model, tc.w, tc.perm, "hi")
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// writesFor gives the edit rule from edit = "session", never to a read-only
// turn; a folder that cannot go in a rule gets none; codex gets none for a
// folder that holds a protected file, which its sandbox could not keep out.
func TestWritesFor(t *testing.T) {
	folder := t.TempDir()
	real, _ := filepath.EvalSymlinks(folder)
	data := filepath.Join(t.TempDir(), "home")
	protect := []string{filepath.Join(data, settings.PolicyFile)}
	def := settings.DefaultPolicy()
	w, why := writesFor(hn("claude"), folder, false, def, protect)
	if why != "" || len(w.Edit) == 0 || w.Edit[0] != folder || !slices.Contains(w.Edit, real) || len(w.Protect) == 0 || w.ReadOnly {
		t.Fatalf("claude: %+v %q", w, why)
	}
	if w, _ := writesFor(hn("codex"), folder, false, def, protect); len(w.Edit) == 0 {
		t.Fatalf("codex: %+v", w)
	}
	if w, _ := writesFor(hn("claude"), folder, true, def, protect); !w.ReadOnly || len(w.Edit)+len(w.Protect) != 0 {
		t.Fatalf("read-only: %+v", w)
	}
	none := def
	none.Edit = settings.EditNone
	if w, _ := writesFor(hn("claude"), folder, false, none, protect); len(w.Edit) != 0 || len(w.Protect) == 0 {
		t.Fatalf("edit none: %+v", w)
	}
	odd := filepath.Join(t.TempDir(), "a,b")
	if err := os.Mkdir(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	if w, why := writesFor(hn("claude"), odd, false, def, protect); len(w.Edit) != 0 || why != "folder_shape" {
		t.Fatalf("a comma in the folder: %+v %q", w, why)
	}
	// codex takes any folder name: its sandbox needs no rule.
	if w, why := writesFor(hn("codex"), odd, false, def, protect); len(w.Edit) == 0 || why != "" {
		t.Fatalf("codex, a comma in the folder: %+v %q", w, why)
	}
	// A data folder named relative to the service's own folder is still
	// found inside the session's folder.
	wd, _ := os.Getwd()
	t.Chdir(filepath.Dir(data))
	if w, why := writesFor(hn("codex"), filepath.Dir(data), false, def, []string{filepath.Join("home", settings.PolicyFile)}); len(w.Edit) != 0 || why != "data_folder_inside" {
		t.Fatalf("codex, a relative data folder inside: %+v %q", w, why)
	}
	t.Chdir(wd)
	// The data folder inside the session's folder.
	parent := filepath.Dir(data)
	if w, why := writesFor(hn("codex"), parent, false, def, protect); len(w.Edit) != 0 || why != "data_folder_inside" {
		t.Fatalf("codex over the data folder: %+v %q", w, why)
	}
	if w, why := writesFor(hn("claude"), parent, false, def, protect); len(w.Edit) == 0 || why != "" || len(w.Protect) == 0 {
		t.Fatalf("claude over the data folder keeps the deny rules: %+v %q", w, why)
	}
	// A protected file that cannot go in a deny rule leaves edits out.
	if w, why := writesFor(hn("claude"), folder, false, def, []string{"/data/a(b"}); len(w.Edit) != 0 || why != "protected_file_shape" {
		t.Fatalf("an odd protected file: %+v %q", w, why)
	}
}

// A broken permissions.toml stops the turn before anything is stored, with
// the file and the line in the refusal. The file is read for every turn, so a
// fix applies to the next one.
func TestBrokenPermissionsRefusesTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), settings.PolicyFile)
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var wired atomic.Int32
	e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", Settings: settings.NewFiles(filepath.Dir(path)),
		Wire: func(agent, baseURL string, _ Access) (Launch, error) { wired.Add(1); return Launch{}, ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	start := func() *Refusal {
		_, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
		var r *Refusal
		if !errors.As(err, &r) {
			t.Fatalf("Start: %v, want a refusal", err)
		}
		return r
	}
	// Missing: empty lists, and the turn goes on to start its agent.
	if r := start(); r.Status != 400 || !strings.Contains(r.Msg, "is not on the service's PATH") || wired.Load() != 1 {
		t.Fatalf("missing file: %+v, wire %d", r, wired.Load())
	}
	writePerm(t, path, "# mine\nallow = go test\n")
	if r := start(); r.Status != 400 || !strings.Contains(r.Msg, "permissions.toml") || !strings.Contains(r.Msg, "line 2") {
		t.Fatalf("broken file: %+v, want 400 naming the file and line 2", r)
	}
	if wired.Load() != 1 {
		t.Errorf("broken file: the agent was wired")
	}
	if snap, _ := st.Snapshot(); len(snap.Sessions) != 0 {
		t.Errorf("broken file: %d sessions stored, want none", len(snap.Sessions))
	}
	writePerm(t, path, "allow = [\"go test\"]\n")
	if r := start(); r.Status != 400 || !strings.Contains(r.Msg, "is not on the service's PATH") || wired.Load() != 2 {
		t.Fatalf("fixed file: %+v, wire %d", r, wired.Load())
	}
}

// An access the engine does not know is refused before a slot is taken, so
// the refusal holds none: with one slot, the next turn still gets it.
func TestUnknownAccessHoldsNoSlot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", MaxRuns: 1,
		Wire: func(agent, baseURL string, _ Access) (Launch, error) { return Launch{}, ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	for i, access := range []string{"bogus", "bogus", ""} {
		_, err := e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi", Access: access})
		var r *Refusal
		if !errors.As(err, &r) {
			t.Fatalf("%d: %v", i, err)
		}
		want := "access must be"
		if access == "" {
			want = "is not on the service's PATH" // past the slot: it was free
		}
		if r.Status != 400 || !strings.Contains(r.Msg, want) {
			t.Fatalf("%d (%q): %+v, want 400 %q", i, access, r, want)
		}
	}
}

// A runner whose server cannot be reached starts no turn: the refusal says
// the server's settings could not be had, and nothing is stored.
func TestUnreachableSettingsRefuseTurn(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	remote := &settings.Remote{Server: "http://127.0.0.1:1", Local: t.TempDir()}
	remote.Enrolled("rn_1", "rt_x")
	var wired atomic.Int32
	e, err := New(Config{Store: st, BaseURL: "http://127.0.0.1:1", Settings: remote,
		Wire: func(agent, baseURL string, _ Access) (Launch, error) { wired.Add(1); return Launch{}, ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Start(Request{Harness: "claude", Folder: t.TempDir(), Prompt: "hi"})
	var r *Refusal
	if !errors.As(err, &r) || r.Status != 400 || !strings.Contains(r.Msg, "the server's settings cannot be used, so the turn did not start: the server could not be reached") {
		t.Fatalf("Start: %v", err)
	}
	if wired.Load() != 0 {
		t.Error("the agent was wired")
	}
	if snap, _ := st.Snapshot(); len(snap.Sessions) != 0 {
		t.Errorf("%d sessions stored, want none", len(snap.Sessions))
	}
}
