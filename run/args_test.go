package run

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIDScanner(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	result := `{"type":"result","result":"hi","session_id":"` + id + `"}`
	big := `{"type":"result","result":"` + strings.Repeat("a", MaxStdoutLine) + `","session_id":"` + id + `"}`
	for _, tc := range []struct {
		name, agent string
		writes      []string
		want        string
	}{
		{"claude whole", "claude", []string{result + "\n"}, id},
		{"claude split across writes, no final newline", "claude", []string{result[:10], result[10:30], result[30:]}, id},
		{"claude line over the bound is skipped", "claude", []string{big + "\n"}, ""},
		{"a line after a skipped one still counts", "claude", []string{big + "\n" + result + "\n"}, id},
		{"claude takes only the result type", "claude", []string{result + "\n" + `{"type":"assistant","session_id":"other-1"}` + "\n"}, id},
		{"claude ignores codex's shape", "claude", []string{`{"type":"thread.started","thread_id":"` + id + `"}` + "\n"}, ""},
		{"codex takes the first thread.started", "codex", []string{`{"type":"thread.started","thread_id":"` + id + `"}` + "\n" +
			`{"type":"thread.started","thread_id":"other"}` + "\n"}, id},
		{"codex ignores claude's shape", "codex", []string{result + "\n"}, ""},
		{"an id not shaped like one", "claude", []string{`{"type":"result","session_id":"-x"}` + "\n"}, ""},
		{"an id with a slash", "claude", []string{`{"type":"result","session_id":"a/b"}` + "\n"}, ""},
	} {
		s := &idScanner{agent: tc.agent}
		for _, w := range tc.writes {
			if n, err := s.Write([]byte(w)); n != len(w) || err != nil {
				t.Fatalf("%s: Write %d %v", tc.name, n, err)
			}
		}
		if got := s.ID(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if cap(s.line) > MaxStdoutLine+64<<10 {
			t.Errorf("%s: held %d bytes", tc.name, cap(s.line))
		}
	}
}

// Every turn of a session gets the same instructions, naming that session,
// through the agent's own instruction flag and never inside the prompt: own
// replay and the vendor's prompt cache need the text the same on every turn.
// The default is the text turns had before the file existed.
func TestInstructionsDefault(t *testing.T) {
	e := &Engine{}
	got, err := e.instructions("abc")
	if err != nil {
		t.Fatal(err)
	}
	const want = "Your session's earlier turns are stored by AgentBus. To look something up, run:\n" +
		`agentbus-api GET "/api/v0/sessions/abc/search?q=<text>"` + "\n" +
		`with <text> URL-encoded. Each hit's "read" is a path you can GET the same way, for the whole of what matched.` + "\n" +
		`You can also ask other agents with the same command: start a turn on any saved agent, continue one, or ask for a review. ` +
		`Do not wait for them: each result comes back to you as a later turn. ` +
		`How: agentbus-api GET /docs/llms.txt, section "Ask other agents".`
	if got != want {
		t.Fatalf("instructions\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "$") {
		t.Errorf("instructions name a variable:\n%s", got)
	}
	for _, resume := range []string{"", "r-1"} {
		a, _ := turnArgs("claude", got, resume, "", Writes{}, Permissions{}, "hi")
		if i := slices.Index(a, "--append-system-prompt"); i < 0 || a[i+1] != want || a[len(a)-1] != "hi" {
			t.Errorf("claude resume %q: %q", resume, a)
		}
		a, _ = turnArgs("codex", got, resume, "", Writes{}, Permissions{}, "hi")
		if i := slices.Index(a, "developer_instructions="+tomlString(want)); i < 1 || a[i-1] != "-c" || a[len(a)-1] != "hi" {
			t.Errorf("codex resume %q: %q", resume, a)
		}
	}
	// A TOML basic string: quoted, with JSON's escapes, which TOML shares.
	if got := tomlString("a\"b\\c\n<d>"); got != `"a\"b\\c\n`+"\\u003cd\\u003e\"" {
		t.Errorf("tomlString %s", got)
	}
}

// instructions.md is the text: {session} filled in, trailing space cut; an
// empty file gives none; a missing one the default; one that cannot be used
// is an error that names no path.
func TestInstructionsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), InstructionsFile)
	e := &Engine{cfg: Config{Instructions: path}}
	if got, err := e.instructions("s1"); err != nil || !strings.Contains(got, "/api/v0/sessions/s1/search") {
		t.Fatalf("missing: %q %v", got, err)
	}
	for _, tc := range []struct{ file, want string }{
		{"Be brief. Session {session}.\n\n", "Be brief. Session s1."},
		{"", ""},
		{"  \n", ""},
	} {
		if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := e.instructions("s1"); err != nil || got != tc.want {
			t.Errorf("%q: %q %v, want %q", tc.file, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ file, want string }{
		{strings.Repeat("a", MaxInstructions+1), "over 64 KiB"},
		{"a\x00b", "NUL"},
		{"a\xffb", "UTF-8"},
	} {
		if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.instructions("s1"); err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), path) {
			t.Errorf("%.20q: %v, want %q", tc.file, err, tc.want)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".gone", path); err != nil {
		t.Fatal(err)
	}
	if _, err := e.instructions("s1"); err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("a dangling link: %v", err)
	}
}

// A developer_instructions the person set in codex's own config wins: the
// turn then gets no hint, since a -c value would replace theirs.
func TestCodexOwnInstructions(t *testing.T) {
	home, folder := t.TempDir(), t.TempDir()
	env := []string{"HOME=" + home}
	if codexOwnInstructions(env, folder) {
		t.Fatal("no config, yet own instructions")
	}
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(home, ".codex", "config.toml")
	write(cfg, "model = \"x\"\n[profiles.a]\ndeveloper_instructions = \"in a table\"\n")
	if codexOwnInstructions(env, folder) {
		t.Error("a key inside a table counted as top-level")
	}
	write(cfg, "# mine\ndeveloper_instructions = \"\"\"\nbe brief\n\"\"\"\n[x]\n")
	if !codexOwnInstructions(env, folder) {
		t.Error("a top-level key in ~/.codex/config.toml not seen")
	}
	other := t.TempDir()
	write(filepath.Join(other, "config.toml"), "developer_instructions = 'x'\n")
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir(), "CODEX_HOME=" + other}, folder) {
		t.Error("$CODEX_HOME/config.toml not read")
	}
	// A relative CODEX_HOME is the turn folder's, never the service's.
	rel := t.TempDir()
	write(filepath.Join(rel, "state", "codex", "config.toml"), "developer_instructions = \"x\"\n")
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir(), "CODEX_HOME=state/codex"}, rel) {
		t.Error("a relative CODEX_HOME not read against the turn's folder")
	}
	if codexOwnInstructions([]string{"HOME=" + t.TempDir(), "CODEX_HOME=state/codex"}, t.TempDir()) {
		t.Error("a relative CODEX_HOME read against another folder")
	}
	sub := filepath.Join(folder, "a", "b")
	if codexOwnInstructions([]string{"HOME=" + t.TempDir()}, sub) {
		t.Fatal("own instructions with no project config")
	}
	write(filepath.Join(folder, ".codex", "config.toml"), "developer_instructions = \"x\"\n")
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir()}, folder) {
		t.Error("the folder's .codex/config.toml not read")
	}
	// A project config above the folder counts too: codex reads it.
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir()}, sub) {
		t.Error("a parent folder's .codex/config.toml not read")
	}
	// A folder reached through a symlink: its real parents count.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if !codexOwnInstructions([]string{"HOME=" + t.TempDir()}, link) {
		t.Error("a symlinked folder's real parents not read")
	}
	if a, _ := turnArgs("codex", "", "", "", Writes{}, Permissions{}, "hi"); slices.ContainsFunc(a, func(s string) bool { return strings.HasPrefix(s, "developer_instructions=") }) {
		t.Errorf("no hint, yet %q", a)
	}
}

func TestTurnEnv(t *testing.T) {
	got := turnEnv(nil, []string{"A=1", "AGENTBUS_TOKEN=main", "AGENTBUS_URL=x"}, "http://127.0.0.1:1", "abt_1", "")
	if strings.Join(got, " ") != "A=1 AGENTBUS_URL=http://127.0.0.1:1 AGENTBUS_TOKEN=abt_1" {
		t.Errorf("turnEnv: %v", got)
	}
}

// A turn that may write may run "<path> api" without asking; a read-only one
// gets nothing new, and the person's own lists are not changed.
func TestWithAPI(t *testing.T) {
	own := Permissions{Allow: []string{"go test"}, Forbid: []string{"git push"}}
	p := withAPI(own, true, false)
	a, _ := turnArgs("claude", "", "", "", Writes{}, p, "hi")
	if got := strings.Join(a, " "); !strings.Contains(got, "Bash(go test:*),Bash(agentbus-api:*)") || !strings.Contains(got, "--disallowedTools Bash(git push:*)") {
		t.Errorf("args: %s", got)
	}
	if len(own.Allow) != 1 {
		t.Errorf("the person's allow list changed: %v", own.Allow)
	}
	if p := withAPI(own, true, true); len(p.Allow) != 1 {
		t.Errorf("read-only: %v", p.Allow)
	}
	if p := withAPI(own, false, false); len(p.Allow) != 1 {
		t.Errorf("no path: %v", p.Allow)
	}
}

// The API client's folder goes first on PATH, or is PATH when there was none.
func TestTurnEnvAPIDir(t *testing.T) {
	got := turnEnv([]string{"PATH=/usr/bin:/bin", "B=2"}, nil, "u", "t", "/home/x/bin")
	if strings.Join(got, " ") != "PATH=/home/x/bin:/usr/bin:/bin B=2 AGENTBUS_URL=u AGENTBUS_TOKEN=t" {
		t.Errorf("with PATH: %v", got)
	}
	got = turnEnv([]string{"B=2"}, nil, "u", "t", "/home/x/bin")
	if strings.Join(got, " ") != "B=2 PATH=/home/x/bin AGENTBUS_URL=u AGENTBUS_TOKEN=t" {
		t.Errorf("without PATH: %v", got)
	}
}
