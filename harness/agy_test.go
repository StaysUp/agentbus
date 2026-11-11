package harness

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func agySettingsOf(t *testing.T, a []string) (allowNonWorkspace bool, allow, deny []string) {
	t.Helper()
	i := slices.Index(a, "--settings")
	if i < 0 || i+1 >= len(a) {
		t.Fatalf("no --settings in %q", a)
	}
	var s struct {
		AllowNonWorkspaceAccess *bool `json:"allowNonWorkspaceAccess"`
		Permissions             struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(a[i+1]), &s); err != nil || s.AllowNonWorkspaceAccess == nil || s.Permissions.Allow == nil {
		t.Fatalf("settings %s: %v", a[i+1], err)
	}
	return *s.AllowNonWorkspaceAccess, s.Permissions.Allow, s.Permissions.Deny
}

func TestAgyArgs(t *testing.T) {
	h := agy{}
	edit := Writes{Edit: []string{"/w/f"}, Protect: []string{"/w/f/policy.toml"}}
	for _, c := range []struct {
		name        string
		t           Turn
		allow, deny []string
	}{
		{"read-only", Turn{Writes: Writes{ReadOnly: true, Edit: []string{"/w/f"}}, Allow: []string{"go test"}, ReadFolders: []string{"/r"}},
			[]string{"read_file(/r)"}, []string{"write_file(*)", "edit_file(*)", "command(*)"}},
		{"edits", Turn{Writes: edit, Allow: []string{"go test", "agentbus-api"}},
			[]string{"write_file(/w/f)", "edit_file(/w/f)", "command(go test)", "command(agentbus-api)"},
			[]string{"write_file(/w/f/policy.toml)", "edit_file(/w/f/policy.toml)"}},
		{"no edit rule", Turn{Allow: []string{"make"}},
			[]string{"command(make)"}, []string{"write_file(*)", "edit_file(*)"}},
		// A rule cannot hold a parenthesis or a glob character: such an
		// entry is left out, so it allows nothing.
		{"unsafe names", Turn{Writes: edit, Allow: []string{"x) command(rm"}, ReadFolders: []string{"/a*"}},
			[]string{"write_file(/w/f)", "edit_file(/w/f)"},
			[]string{"write_file(/w/f/policy.toml)", "edit_file(/w/f/policy.toml)"}},
	} {
		c.t.Prompt = "--model x"
		a := h.Args(c.t)
		off, allow, deny := agySettingsOf(t, a)
		if off || !slices.Equal(allow, c.allow) || !slices.Equal(deny, c.deny) {
			t.Errorf("%s: allowNonWorkspace %t allow %q deny %q", c.name, off, allow, deny)
		}
		if a[len(a)-1] != "--print=--model x" || slices.Contains(a, "--model") {
			t.Errorf("%s: the prompt is not --print's own value: %q", c.name, a)
		}
	}
	a := h.Args(Turn{ResumeID: "4305987f-0c94-4f18-b670-820084213439", Model: "gemini-3.8-flash-medium", Prompt: "hi"})
	want := []string{"--conversation", "4305987f-0c94-4f18-b670-820084213439", "--model", "gemini-3.8-flash-medium", "--print=hi"}
	if !slices.Equal(a[len(a)-5:], want) || a[0] != "--output-format" || a[1] != "json" {
		t.Errorf("resume: %q", a)
	}
}

// The one object --output-format json prints (agy 1.5.5, 2026-11-11).
func TestAgyResumeID(t *testing.T) {
	h := agy{}
	line := `{"conversation_id":"4305987f-0c94-4f18-b670-820084213439","status":"SUCCESS","response":"pong\n","duration_seconds":3.07,"num_turns":1}`
	if got := h.ResumeID([]byte(line), ""); got != "4305987f-0c94-4f18-b670-820084213439" {
		t.Errorf("id %q", got)
	}
	failed := `{"conversation_id":"","status":"ERROR","response":"","error":"authentication failed or timed out"}`
	if got := h.ResumeID([]byte(failed), "keep"); got != "keep" {
		t.Errorf("a failed turn changed the id: %q", got)
	}
	for _, l := range []string{`{"conversation_id":"-x","status":"SUCCESS"}`, `{"conversation_id":"abc"}`, `pong`} {
		if got := h.ResumeID([]byte(l), ""); got != "" {
			t.Errorf("%s: id %q", l, got)
		}
	}
}

func TestAgyRefuse(t *testing.T) {
	h := agy{}
	if h.Refuse(Turn{}) != "" {
		t.Error("a plain turn refused")
	}
	if h.Refuse(Turn{NoSubagents: true}) == "" || h.Refuse(Turn{Forbid: []string{"git push"}}) == "" {
		t.Error("a setting agy cannot keep was not refused")
	}
	for _, o := range All() {
		if o.Info().Name != "agy" && o.Refuse(Turn{NoSubagents: true, Forbid: []string{"git push"}}) != "" {
			t.Errorf("%s refused", o.Info().Name)
		}
	}
}

// agy reads its folder's AGENTS.md and GEMINI.md, and is never given them
// again; a turn's hint has no place in it.
func TestAgyContext(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	h := agy{}
	if !h.Native(root, "AGENTS.md") || !h.Native(root, "GEMINI.md") || h.Native(root, "CLAUDE.md") {
		t.Error("Native")
	}
	if h.OwnInstructions(nil, "/w") == "" {
		t.Error("a hint would be given")
	}
	if env, args := h.Wire("http://127.0.0.1:8787/s/a/t/b"); len(args) != 0 || !slices.Equal(env, []string{"CLOUD_CODE_URL=http://127.0.0.1:8787/s/a/t/b"}) {
		t.Errorf("wire %q %q", env, args)
	}
	if Offers(h.Info(), OpenRouter) || Offers(h.Info(), API) || !Offers(h.Info(), "") {
		t.Error("access")
	}
}

// On a runner that holds no secret, agy is pointed at the service as on its
// own sign-in, and its home gets a placeholder sign-in: the placeholder as
// its access token, no refresh token, so it never renews it, and agy's own
// sign-in method.
func TestAgyRelay(t *testing.T) {
	h := agy{}
	env, args := h.WireRelay("http://127.0.0.1:9/s/x/t/t_1", "abr_placeholder")
	if !slices.Equal(env, []string{"CLOUD_CODE_URL=http://127.0.0.1:9/s/x/t/t_1"}) || len(args) != 0 {
		t.Fatalf("%q %q", env, args)
	}
	files := h.RelayHome("abr_placeholder")
	b, ok := files[".gemini/antigravity-cli/antigravity-oauth-token"]
	if len(files) != 1 || !ok {
		t.Fatalf("%v", files)
	}
	var f struct {
		Token  map[string]string `json:"token"`
		Method string            `json:"auth_method"`
	}
	if err := json.Unmarshal(b, &f); err != nil || f.Token["access_token"] != "abr_placeholder" || f.Token["token_type"] != "Bearer" ||
		f.Token["refresh_token"] != "" || f.Token["expiry"] != "2099-01-01T00:00:00Z" || f.Method != "consumer" {
		t.Fatalf("%s", b)
	}
	l := h.Info().Login
	if l == nil || l.Keep == "" || l.File != ".gemini/antigravity-cli/antigravity-oauth-token" || l.RefreshKey != "" ||
		!slices.Equal(l.RenewArgs, []string{"models"}) || l.RenewWithinMinutes >= 5 || !slices.Equal(l.Set, []string{"DBUS_SESSION_BUS_ADDRESS=disabled:"}) {
		t.Fatalf("%+v", l)
	}
}
