package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/settings"
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
		s := &idScanner{harness: hn(tc.agent)}
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
// With the agentbus MCP tools, {tools} is one line that names them, and no
// agentbus-api command.
func TestInstructionsWithTools(t *testing.T) {
	got, err := (&Engine{}).instructions("abc")
	if err != nil {
		t.Fatal(err)
	}
	got = withTools(got, "abc", true, true)
	if !strings.Contains(got, settings.ToolsText(true)) || strings.Contains(got, "agentbus-api") || strings.Contains(got, "{tools}") ||
		!strings.Contains(got, "own copy of the person's folder") {
		t.Errorf("instructions with tools:\n%s", got)
	}
}

// With [agent_api] apply off (the default from defaults version 2), the
// instructions do not offer applying, which would answer 403: they say the
// person applies. On, they are the text of builds before 0.5.1. The same
// holds for the MCP tools' line and the tool server's own instructions.
func TestInstructionsFollowApply(t *testing.T) {
	got, err := (&Engine{}).instructions("abc")
	if err != nil {
		t.Fatal(err)
	}
	for _, tools := range []bool{false, true} {
		off, on := withTools(got, "abc", tools, false), withTools(got, "abc", tools, true)
		if strings.Contains(off, "/apply") || strings.Contains(off, "apply and stop") || !strings.Contains(off, "The person applies turns to their folder; you cannot.") {
			t.Errorf("tools %v, apply off:\n%s", tools, off)
		}
		if !strings.Contains(on, "apply") || strings.Contains(on, "you cannot") {
			t.Errorf("tools %v, apply on:\n%s", tools, on)
		}
	}
	if !strings.Contains(withTools(got, "abc", false, true), "agentbus-api POST /api/v0/turns/T/apply") {
		t.Error("apply on: no apply command")
	}
	if off := settings.ToolServerInstructions(false); strings.Contains(off, "apply_turn") || strings.Contains(off, "apply turns") {
		t.Errorf("tool server, apply off: %s", off)
	}
	if on := settings.ToolServerInstructions(true); !strings.Contains(on, "apply_turn") {
		t.Errorf("tool server, apply on: %s", on)
	}
}

func TestInstructionsDefault(t *testing.T) {
	e := &Engine{}
	got, err := e.instructions("abc")
	if err != nil {
		t.Fatal(err)
	}
	got = withTools(got, "abc", false, true)
	want := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(settings.DefaultInstructions, "{tools}", settings.APIText(true)), "{session}", "abc"))
	// What the baseline found agents miss: their copy is not the person's
	// folder, how to apply, and how to ask for a review.
	for _, line := range []string{"in session abc", "own copy of the person's folder", "agentbus-api POST /api/v0/turns/T/apply",
		"agentbus-api POST /api/v0/turns/T/review", `"/api/v0/sessions/abc/search?q=<text>"`, "not your harness's own agents"} {
		if !strings.Contains(got, line) {
			t.Errorf("instructions lack %q", line)
		}
	}
	if got != want {
		t.Fatalf("instructions\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "$") {
		t.Errorf("instructions name a variable:\n%s", got)
	}
	for _, resume := range []string{"", "r-1"} {
		a := turnArgs(hn("claude"), got, resume, "", Writes{}, settings.Policy{}, "hi")
		if i := slices.Index(a, "--append-system-prompt"); i < 0 || a[i+1] != want || a[len(a)-1] != "hi" {
			t.Errorf("claude resume %q: %q", resume, a)
		}
		a = turnArgs(hn("codex"), got, resume, "", Writes{}, settings.Policy{}, "hi")
		if i := slices.Index(a, "developer_instructions="+quoteTOML(want)); i < 1 || a[i-1] != "-c" || a[len(a)-1] != "hi" {
			t.Errorf("codex resume %q: %q", resume, a)
		}
	}
}

// instructions.md is the text: {session} filled in, trailing space cut; an
// empty file gives none; a missing one the default; one that cannot be used
// is an error that names no path.
func TestInstructionsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), settings.InstructionsFile)
	e := &Engine{cfg: Config{Settings: settings.NewFiles(filepath.Dir(path))}}
	if got, err := e.instructions("s1"); err != nil || !strings.Contains(withTools(got, "s1", false, false), "/api/v0/sessions/s1/search") {
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
		{strings.Repeat("a", settings.MaxInstructions+1), "over 64 KiB"},
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

func TestTurnEnv(t *testing.T) {
	got := turnEnv(nil, []string{"A=1", "AGENTBUS_TOKEN=main", "AGENTBUS_URL=x"}, "http://127.0.0.1:1", "abt_1", "", nil)
	if strings.Join(got, " ") != "A=1 AGENTBUS_URL=http://127.0.0.1:1 AGENTBUS_TOKEN=abt_1" {
		t.Errorf("turnEnv: %v", got)
	}
}

// Asked work gets the asking session and turn; an earlier value of either
// in the parent's environment never reaches a turn, asked or not.
func TestTurnEnvAsked(t *testing.T) {
	parent := []string{"AGENTBUS_ASKED_BY_SESSION=old", "AGENTBUS_ASKED_BY_TURN=old", "A=1"}
	got := turnEnv(nil, parent, "u", "t", "", &event.Origin{Kind: event.OriginAsked, Session: "s1", Turn: "t_1"})
	if strings.Join(got, " ") != "A=1 AGENTBUS_URL=u AGENTBUS_TOKEN=t AGENTBUS_ASKED_BY_SESSION=s1 AGENTBUS_ASKED_BY_TURN=t_1" {
		t.Errorf("asked: %v", got)
	}
	if got := turnEnv(nil, parent, "u", "t", "", nil); strings.Join(got, " ") != "A=1 AGENTBUS_URL=u AGENTBUS_TOKEN=t" {
		t.Errorf("not asked: %v", got)
	}
}

// The line names the asking turn's diff and its session's turns, with
// agentbus-api or with curl; nothing for a turn nobody asked for. A turn
// token may always read its own request tree, so there is no setting that
// leaves it out.
func TestAskedLine(t *testing.T) {
	o := &event.Origin{Kind: event.OriginAsked, Session: "s1", Turn: "t_1"}
	line := askedLine(askedBy(o), true, false)
	for _, want := range []string{"session s1, turn t_1", `agentbus-api GET "/api/v0/turns/t_1/diff?session=s1"`, `agentbus-api GET "/api/v0/turns?session=s1"`} {
		if !strings.Contains(line, want) {
			t.Errorf("line lacks %q: %s", want, line)
		}
	}
	if line := askedLine(askedBy(o), false, false); !strings.Contains(line, `"$AGENTBUS_URL/api/v0/turns/t_1/diff?session=s1"`) {
		t.Errorf("curl: %s", line)
	}
	for _, o := range []*event.Origin{nil, {Kind: event.OriginResult, Session: "s1", Turn: "t_1"}, {Kind: event.OriginAsked, Session: "s1"}} {
		if line := askedLine(askedBy(o), true, false); line != "" {
			t.Errorf("%v: %s", o, line)
		}
	}
}

// A turn that may write may run "<path> api" without asking; a read-only one
// gets nothing new, and the person's own lists are not changed.
func TestWithAPI(t *testing.T) {
	own := settings.Policy{Allow: []string{"go test"}, Forbid: []string{"git push"}}
	p := withAPI(own, true, false)
	a := turnArgs(hn("claude"), "", "", "", Writes{}, p, "hi")
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
	got := turnEnv([]string{"PATH=/usr/bin:/bin", "B=2"}, nil, "u", "t", "/home/x/bin", nil)
	if strings.Join(got, " ") != "PATH=/home/x/bin:/usr/bin:/bin B=2 AGENTBUS_URL=u AGENTBUS_TOKEN=t" {
		t.Errorf("with PATH: %v", got)
	}
	got = turnEnv([]string{"B=2"}, nil, "u", "t", "/home/x/bin", nil)
	if strings.Join(got, " ") != "B=2 PATH=/home/x/bin AGENTBUS_URL=u AGENTBUS_TOKEN=t" {
		t.Errorf("without PATH: %v", got)
	}
}

// hn is the registered harness name.
func hn(name string) harness.Harness {
	h, ok := harness.Get(name)
	if !ok {
		panic("no harness " + name)
	}
	return h
}

// quoteTOML is a TOML basic string, as codex's -c value is quoted.
func quoteTOML(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
