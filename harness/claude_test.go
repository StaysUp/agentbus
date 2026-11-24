package harness

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// settingsEnv parses the "env" block of a --settings argument. Empty if none.
func settingsEnv(t *testing.T, args []string) map[string]string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--settings" {
			continue
		}
		var v struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(args[i+1]), &v); err != nil {
			t.Fatalf("--settings is not JSON: %v (%q)", err, args[i+1])
		}
		return v.Env
	}
	return nil
}

// envVal is the value of name= in a process-env slice, and whether it was set.
func envVal(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			return v, true
		}
	}
	return "", false
}

// The gateway address must reach Claude Code through --settings, never a
// non-empty process env var: a user's own settings.json "env" block, or a
// cloned project's .claude/settings.json, overrides an env var and would
// redirect every model call off our gateway, past recording and review
// (VERIFY_copy_level.md check 2; agy review live on claude 2.1.330, 2026-11-22).
// This holds on EVERY access path — a bypass left on one is the whole bypass.
func TestClaudeWireAllPathsPinGatewayInSettings(t *testing.T) {
	const base = "http://127.0.0.1:8787/s/s1/t/t_1"
	for _, c := range []struct {
		name   string
		wire   func() (env, args []string)
		secret string // an env key that must stay in env, never argv
	}{
		{"Wire", func() ([]string, []string) { return claude{}.Wire(base) }, ""},
		{"WireRelay", func() ([]string, []string) { return claude{}.WireRelay(base, "abs_standin") }, ""},
		{"WireAPI", func() ([]string, []string) { return claude{}.WireAPI(APIWire{Base: base, Placeholder: "abs_standin"}) }, ""},
		{"WireOpenRouter", func() ([]string, []string) {
			return claude{}.WireOpenRouter(OpenRouterWire{Base: base, Key: "sk-real-key"})
		}, "ANTHROPIC_AUTH_TOKEN"},
	} {
		env, args := c.wire()
		if v, ok := envVal(env, "ANTHROPIC_BASE_URL"); ok && v != "" {
			t.Errorf("%s: gateway in a non-empty env var %q; settings.json would override it", c.name, v)
		}
		if got := settingsEnv(t, args)["ANTHROPIC_BASE_URL"]; got != base {
			t.Errorf("%s: --settings gateway %q, want %q", c.name, got, base)
		}
		// A real OpenRouter key must not reach argv (ps-readable); it stays in env.
		if c.secret != "" {
			if _, inSettings := settingsEnv(t, args)[c.secret]; inSettings {
				t.Errorf("%s: %s is in --settings argv, want it only in env (a real key is ps-readable)", c.name, c.secret)
			}
			if v, ok := envVal(env, c.secret); !ok || v == "" {
				t.Errorf("%s: %s not carried in env (%q, %v)", c.name, c.secret, v, ok)
			}
		}
	}
}

// Every relayed credential path (WireAPI, and WireOpenRouter when the key is a
// per-turn stand-in) pins its stand-in auth token in --settings and clears it in
// env, so a settings.json ANTHROPIC_AUTH_TOKEN cannot preempt it (which makes
// the relay reject the call with 401). Model aliases ride in --settings too.
func TestClaudeRelayedCredsPinnedInSettings(t *testing.T) {
	const base = "http://127.0.0.1:8787/s/s1/t/t_1"
	const standin = "abs_standin"

	apiEnv, apiArgs := claude{}.WireAPI(APIWire{Base: base, Placeholder: standin})
	if s := settingsEnv(t, apiArgs); s["ANTHROPIC_AUTH_TOKEN"] != standin || s["ANTHROPIC_API_KEY"] != "" {
		t.Errorf("WireAPI --settings auth=%q api_key=%q, want stand-in and empty api key", s["ANTHROPIC_AUTH_TOKEN"], s["ANTHROPIC_API_KEY"])
	}
	if v, ok := envVal(apiEnv, "ANTHROPIC_AUTH_TOKEN"); !ok || v != "" {
		t.Errorf("WireAPI env ANTHROPIC_AUTH_TOKEN=%q (set %v), want cleared", v, ok)
	}

	// Relayed OpenRouter: stand-in in --settings, env cleared, model aliases pinned.
	orEnv, orArgs := claude{}.WireOpenRouter(OpenRouterWire{Base: base, Key: standin, StandIn: true, Model: "m/x"})
	s := settingsEnv(t, orArgs)
	if s["ANTHROPIC_AUTH_TOKEN"] != standin {
		t.Errorf("relayed WireOpenRouter --settings auth=%q, want the stand-in (else settings.json preempts it, 401)", s["ANTHROPIC_AUTH_TOKEN"])
	}
	if s["ANTHROPIC_MODEL"] != "m/x" || s["CLAUDE_CODE_SUBAGENT_MODEL"] != "m/x" {
		t.Errorf("relayed WireOpenRouter model aliases not pinned in --settings: %q / %q", s["ANTHROPIC_MODEL"], s["CLAUDE_CODE_SUBAGENT_MODEL"])
	}
	if v, ok := envVal(orEnv, "ANTHROPIC_AUTH_TOKEN"); !ok || v != "" {
		t.Errorf("relayed WireOpenRouter env ANTHROPIC_AUTH_TOKEN=%q (set %v), want cleared", v, ok)
	}
}

// A Copy turn's two --settings (the gateway env from Wire, the sandbox from
// Args) are folded into one, keeping both top-level keys and taking the first's
// place. A turn with one --settings is unchanged.
func TestClaudeCombineSettings(t *testing.T) {
	// Two --settings: merged into one carrying both env and sandbox.
	in := []string{"--settings", `{"env":{"ANTHROPIC_BASE_URL":"gw"}}`, "-p", "--model", "haiku",
		"--settings", `{"sandbox":{"enabled":true}}`, "--", "hi"}
	got := claude{}.CombineSettings(in)
	if got[0] != "--settings" {
		t.Fatalf("merged --settings not first: %q", got)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(got[1]), &v); err != nil {
		t.Fatalf("merged value not JSON: %v", err)
	}
	if _, ok := v["env"]; !ok {
		t.Errorf("merged lost env: %q", got[1])
	}
	if _, ok := v["sandbox"]; !ok {
		t.Errorf("merged lost sandbox: %q", got[1])
	}
	if rest := strings.Join(got[2:], " "); rest != "-p --model haiku -- hi" {
		t.Errorf("rest reordered or dropped: %q", rest)
	}
	if n := strings.Count(strings.Join(got, "\x00"), "--settings"); n != 1 {
		t.Errorf("want exactly one --settings, got %d", n)
	}
	// One --settings: unchanged.
	one := []string{"--settings", `{"env":{"X":"1"}}`, "-p", "--", "hi"}
	if g := (claude{}).CombineSettings(one); !slices.Equal(g, one) {
		t.Errorf("single --settings changed: %q", g)
	}
	// A flag VALUE equal to "--settings" (a hint, a tool rule, the prompt) must
	// not be read as a flag: counting it would leave two real --settings and make
	// claude drop the gateway one (live, claude 2.1.330). Here the only real
	// --settings is the gateway's; the poison value is --append-system-prompt's.
	poison := []string{"--settings", `{"env":{"ANTHROPIC_BASE_URL":"gw"}}`, "--append-system-prompt", "--settings",
		"--allowedTools", "Bash", "--", "--settings"}
	if g := (claude{}).CombineSettings(poison); !slices.Equal(g, poison) {
		t.Errorf("a flag value of --settings poisoned the merge: %q", g)
	}
}

// --settings pins the settings a real home's (or a cloned repo's) settings.json
// could use to route a call off our gateway or override the sign-in: the
// alternate-backend switches, custom headers, and apiKeyHelper (all measured
// live on claude 2.1.330). This holds on every claude turn (personal runner).
func TestClaudeSettingsPinBypassVectors(t *testing.T) {
	// IPv6 loopback listener: its exact host:port must be exempted (a "::1"
	// token does not match "[::1]:8787" in claude's NO_PROXY).
	_, args := claude{}.Wire("http://[::1]:8787/s/s1/t/t_1")
	env := settingsEnv(t, args)
	// Every alternate-backend / header / socket switch is pinned empty.
	for _, k := range claudePinnedEnv {
		if v, ok := env[k]; !ok || v != "" {
			t.Errorf("--settings env %s = %q (set %v), want pinned empty (settings.json could route off the gateway)", k, v, ok)
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		v := env[k]
		for _, want := range []string{"127.0.0.1", "[::1]:8787"} {
			if !strings.Contains(","+v+",", ","+want+",") {
				t.Errorf("--settings env %s = %q, want it to exempt loopback %q", k, v, want)
			}
		}
	}
	// A non-loopback gateway (a LAN/Tailscale -listen IP) is also exempted: there
	// is no whole-process wall any more, so it is reached directly, not through a
	// settings.json proxy that could not reach it.
	_, lan := claude{}.Wire("http://192.168.1.2:8787/s/s1/t/t_1")
	if np := settingsEnv(t, lan)["NO_PROXY"]; !strings.Contains(","+np+",", ",192.168.1.2:8787,") {
		t.Errorf("NO_PROXY %q does not exempt the LAN gateway; a settings.json proxy would divert it", np)
	}
	var full map[string]any
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--settings" {
			if err := json.Unmarshal([]byte(args[i+1]), &full); err != nil {
				t.Fatalf("--settings not JSON: %v", err)
			}
		}
	}
	if full["apiKeyHelper"] != "" {
		t.Errorf("--settings apiKeyHelper = %v, want \"\" (a settings.json helper would preempt the stand-in)", full["apiKeyHelper"])
	}
}

// The Copy sandbox block fences writes to the copy plus the extra writable
// paths, keeps reads open except the CLI's login file, and gives a broad
// network allow-list (hosts plus the defaults) with localhost via
// allowLocalBinding. srt cannot be truly open (it refuses "*"), so this is the
// documented honest limit (DESIGN_contain_mode, measured 2026-11-23).
func TestClaudeSandboxBlock(t *testing.T) {
	b := claudeSandboxBlock("/copy", []string{"/tmp/t", "/caches/go"}, []string{"/home/.claude/.credentials.json"},
		[]string{"gw.local:8787"})
	if b["enabled"] != true {
		t.Errorf("enabled = %v, want true", b["enabled"])
	}
	// A cloned project's settings.json must not extend allowWrite or run
	// unsandboxed; a sandbox backend that cannot start must fail the turn, not
	// run it unconfined (both verified live, claude 2.1.330).
	if b["allowUnsandboxedCommands"] != false {
		t.Errorf("allowUnsandboxedCommands = %v, want false", b["allowUnsandboxedCommands"])
	}
	if b["failIfUnavailable"] != true {
		t.Errorf("failIfUnavailable = %v, want true", b["failIfUnavailable"])
	}
	// A nil denyRead must become [], not JSON null (claude 2.1.330 exits on null).
	if d := claudeSandboxBlock("/copy", nil, nil, nil)["filesystem"].(map[string]any)["denyRead"].([]string); d == nil {
		t.Error("denyRead is nil; marshals as null and claude rejects it")
	}
	fs := b["filesystem"].(map[string]any)
	if w := fs["allowWrite"].([]string); !slices.Equal(w, []string{"/copy", "/tmp/t", "/caches/go"}) {
		t.Errorf("allowWrite = %v, want copy + extras", w)
	}
	if d := fs["denyRead"].([]string); !slices.Equal(d, []string{"/home/.claude/.credentials.json"}) {
		t.Errorf("denyRead = %v, want the login file", d)
	}
	net := b["network"].(map[string]any)
	if net["allowLocalBinding"] != true {
		t.Errorf("allowLocalBinding = %v, want true (localhost)", net["allowLocalBinding"])
	}
	dom := net["allowedDomains"].([]string)
	if dom[0] != "gw.local:8787" {
		t.Errorf("allowedDomains[0] = %q, want the gateway first", dom[0])
	}
	for _, want := range []string{"registry.npmjs.org", "pypi.org", "proxy.golang.org", "crates.io", "github.com"} {
		if !slices.Contains(dom, want) {
			t.Errorf("allowedDomains missing a registry/GitHub host %q: %v", want, dom)
		}
	}
}

// The relay path pins the Connect stand-in and clears ANTHROPIC_API_KEY /
// ANTHROPIC_AUTH_TOKEN in --settings, so a settings.json key cannot preempt the
// stand-in; and it clears a host CLAUDE_CODE_OAUTH_TOKEN from the process env so
// none lingers in cmd.Env past the --settings one.
func TestClaudeWireRelayPinsStandInAndClears(t *testing.T) {
	const base = "http://127.0.0.1:8787/s/s1/t/t_1"
	const standin = "abs_test-standin"
	env, args := claude{}.WireRelay(base, standin)
	s := settingsEnv(t, args)
	if s["CLAUDE_CODE_OAUTH_TOKEN"] != standin {
		t.Errorf("--settings stand-in %q, want %q", s["CLAUDE_CODE_OAUTH_TOKEN"], standin)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if v, ok := s[k]; !ok || v != "" {
			t.Errorf("--settings %s = %q (set %v), want cleared to \"\" so a settings.json key cannot preempt the stand-in", k, v, ok)
		}
	}
	// The host token is scrubbed from cmd.Env (empty), and the real stand-in is
	// not exposed in the process env — it is authoritative only in --settings.
	if v, ok := envVal(env, "CLAUDE_CODE_OAUTH_TOKEN"); !ok || v != "" {
		t.Errorf("env CLAUDE_CODE_OAUTH_TOKEN = %q (set %v), want cleared to \"\" to scrub any host token", v, ok)
	}
}
