package harness

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// On a Gemini API key, the Gemini CLI gets the placeholder as GEMINI_API_KEY
// and the service as GOOGLE_GEMINI_BASE_URL, and its home's settings select
// the key as its sign-in, which the base URL alone would not.
func TestGeminiAPIKey(t *testing.T) {
	h, ok := Get("gemini")
	if !ok {
		t.Fatal("gemini is not registered")
	}
	env, args := h.WireAPI(APIWire{Base: "http://127.0.0.1:9/s/x/t/t_1/key/gemini", Placeholder: "abs_placeholder", Model: "m"})
	if len(args) != 0 || !slices.Equal(env, []string{"GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:9/s/x/t/t_1/key/gemini", "GEMINI_API_KEY=abs_placeholder"}) {
		t.Errorf("wire %q %q", env, args)
	}
	kh, ok := h.(KeyHome)
	if !ok {
		t.Fatal("gemini writes no settings into the home on an API key")
	}
	files := kh.APIHome()
	b, ok := files[".gemini/settings.json"]
	if len(files) != 1 || !ok {
		t.Fatalf("files %v", files)
	}
	var s struct {
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if err := json.Unmarshal(b, &s); err != nil || s.Security.Auth.SelectedType != "gemini-api-key" {
		t.Errorf("settings %s: %v", b, err)
	}
	// One key serves agy and the Gemini CLI.
	k, ak := h.Info().APIKey, agy{}.Info().APIKey
	if k == nil || *k != *ak {
		t.Errorf("APIKey %+v, agy's %+v", k, ak)
	}
	if h.Info().OwnEndpointEnv != "GOOGLE_GEMINI_BASE_URL" {
		t.Errorf("own endpoint %q", h.Info().OwnEndpointEnv)
	}
}

// Its own Google sign-in is listed but cannot be used yet, and OpenRouter
// cannot serve it. The wires stay as measured, for when control can connect
// that sign-in.
func TestGeminiSignIn(t *testing.T) {
	h, _ := Get("gemini")
	if harnessOffers(h, Subscription) || harnessOffers(h, "") || harnessOffers(h, OpenRouter) || !harnessOffers(h, API) {
		t.Errorf("access %+v", h.Info().Access)
	}
	if h.Info().Login != nil {
		t.Error("a Login, with no relay for its calls")
	}
	env, args := h.Wire("http://127.0.0.1:9/s/x/t/t_1")
	if len(args) != 0 || !slices.Equal(env, []string{"CODE_ASSIST_ENDPOINT=http://127.0.0.1:9/s/x/t/t_1", "GOOGLE_GENAI_USE_GCA=true"}) {
		t.Errorf("wire %q %q", env, args)
	}
	env, args = h.WireRelay("http://127.0.0.1:9/s/x/t/t_1", "abr_placeholder")
	if len(args) != 0 || !slices.Equal(env, []string{"CODE_ASSIST_ENDPOINT=http://127.0.0.1:9/s/x/t/t_1", "GOOGLE_GENAI_USE_GCA=true",
		"GOOGLE_CLOUD_ACCESS_TOKEN=abr_placeholder"}) {
		t.Errorf("relay %q %q", env, args)
	}
}

func TestGeminiArgs(t *testing.T) {
	h, _ := Get("gemini")
	head := []string{"--output-format", "stream-json", "--skip-trust", "--approval-mode"}
	for _, c := range []struct {
		name string
		t    Turn
		want []string
	}{
		// No edit rule: the default policy, which refuses edits and commands.
		{"plain", Turn{Prompt: "hi"}, []string{"default", "--prompt=hi"}},
		{"edits, allow list", Turn{Writes: Writes{Edit: []string{"/w/f"}}, Allow: []string{"tool:*", "api:x", "go test", "agentbus-api", "  "}, Prompt: "hi"},
			[]string{"auto_edit", "--allowed-tools", "run_shell_command(go test)", "--allowed-tools", "run_shell_command(agentbus-api)", "--prompt=hi"}},
		// A comma would split the flag's value, a parenthesis end the rule
		// and a glob widen it: such an entry allows nothing.
		{"unsafe names", Turn{Allow: []string{"a,b", "x) run_shell_command(rm", "rm *"}, Prompt: "hi"}, []string{"default", "--prompt=hi"}},
		{"resume and model", Turn{ResumeID: "4bd2e7a1-0c94-4f18-b670-820084213439", Model: "gemini-2.5-flash", Prompt: "next"},
			[]string{"default", "--resume", "4bd2e7a1-0c94-4f18-b670-820084213439", "--model", "gemini-2.5-flash", "--prompt=next"}},
		// The prompt is --prompt's own value, never a flag.
		{"dash prompt", Turn{Prompt: "--model x"}, []string{"default", "--prompt=--model x"}},
	} {
		if got, want := h.Args(c.t), append(slices.Clone(head), c.want...); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", c.name, got, want)
		}
	}
	for _, a := range h.Args(Turn{Writes: Writes{Edit: []string{"/w"}}, Allow: []string{"make"}}) {
		if a == "yolo" || a == "--yolo" || a == "-y" {
			t.Errorf("a turn auto-approves every tool: %q", a)
		}
	}
}

// The resume id is the session_id of the init line --output-format
// stream-json prints first (gemini 0.70.0).
func TestGeminiResumeID(t *testing.T) {
	h, _ := Get("gemini")
	id := "4bd2e7a1-0c94-4f18-b670-820084213439"
	for _, tc := range []struct {
		line, current, want string
	}{
		{`{"type":"init","timestamp":"2026-11-22T10:00:00.000Z","session_id":"` + id + `","model":"gemini-2.5-flash"}`, "", id},
		{`{"type":"init","session_id":"` + id + `"}`, "keep", id},
		{`{"type":"result","status":"success","stats":{}}`, id, id},
		// A number is a place in --resume's list, not an id.
		{`{"type":"init","session_id":"12"}`, "", ""},
		{`{"type":"init","session_id":"--help"}`, "", ""},
		{`{"type":"message","session_id":"` + id + `"}`, "", ""},
		{`  "session_id": "` + id + `",`, "", ""},
	} {
		if got := h.ResumeID([]byte(tc.line), tc.current); got != tc.want {
			t.Errorf("%s (current %q): %q, want %q", tc.line, tc.current, got, tc.want)
		}
	}
}

// forbid and ask on shell commands refuse the turn; subagents off runs and
// says it was not kept.
func TestGeminiRefuse(t *testing.T) {
	h, _ := Get("gemini")
	if h.Refuse(Turn{Forbid: []string{"tool:subagent"}, Ask: []string{"api:x"}, NoSubagents: true}) != "" {
		t.Error("a turn with tool and API rules only was refused")
	}
	if !strings.Contains(h.Refuse(Turn{Forbid: []string{"git push"}}), "forbid") {
		t.Error("forbid on a command was not refused")
	}
	if !strings.Contains(h.Refuse(Turn{Ask: []string{"curl"}}), "ask") {
		t.Error("ask on a command was not refused")
	}
	if got := Unkept(h, Turn{NoSubagents: true}); len(got) != 1 || got[0] != GeminiSubagentsUnkept {
		t.Errorf("unkept = %q", got)
	}
	if got := Unkept(h, Turn{}); len(got) != 0 {
		t.Errorf("a plain turn unkept = %q", got)
	}
}

// A protected file inside the folder runs the turn without edits.
func TestGeminiEdit(t *testing.T) {
	h, _ := Get("gemini")
	for _, tc := range []struct {
		folder, protect, why string
	}{
		{"/w/f", "/data/policy.toml", ""},
		{"/w/f", "/w/f2/policy.toml", ""},
		{"/w/f", "/w/f/policy.toml", "data_folder_inside"},
		{"/w/f/", "/w/f/sub/agents.json", "data_folder_inside"},
	} {
		edit, why := h.Edit([]string{tc.folder}, []string{tc.protect})
		if why != tc.why || (why == "") != (len(edit) == 1) {
			t.Errorf("%s with %s: edit %q why %q, want why %q", tc.folder, tc.protect, edit, why, tc.why)
		}
	}
}

// The Gemini CLI reads its folder's GEMINI.md by itself; a turn's hint has
// no place in it.
func TestGeminiContext(t *testing.T) {
	h, _ := Get("gemini")
	if !h.Native(nil, "GEMINI.md") || h.Native(nil, "AGENTS.md") || h.Native(nil, "CLAUDE.md") {
		t.Error("Native")
	}
	if h.OwnInstructions(nil, "/f") == "" {
		t.Error("a hint would be given")
	}
	if i := h.Info(); i.ToolServer || i.Replay != "" || i.ConfigDir != ".gemini" || i.ConfigEnv != "" {
		t.Errorf("info %+v", i)
	}
}
