package harness

import (
	"encoding/json"
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
