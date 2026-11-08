package main

// How a run's child reaches the service: the session's base URL goes into the
// child's environment or arguments only, never the service's own.

import (
	"net/http"
	"strings"

	engine "github.com/staysup/agentbus/run"
)

// harnessWires maps each harness to how its base URL reaches the child, on its own subscription.
var harnessWires = map[string]func(baseURL string) (env []string, args []string){
	// Claude Code reads ANTHROPIC_BASE_URL (2.1.318, 2026-11-05).
	"claude": func(b string) ([]string, []string) {
		return []string{"ANTHROPIC_BASE_URL=" + b}, nil
	},
	"codex": codexWire,
}

// codexWire is how codex gets its base URL: it reads it from config, not the
// environment. -c parses the value as TOML, so the quotes make it a string.
// Measured on codex-cli 0.164.0, 2026-11-05. The service routes
// /backend-api/codex to chatgpt.com.
//
// TODO(trade, 2026-11-05): with this wiring codex first tries a WebSocket,
// the service answers 426, and codex falls back to a POST but prints one
// ERROR line per run. No switch turns that attempt off for the built-in
// openai provider (--disable responses_websockets, prefer_websockets and
// supports_websockets change nothing; the provider cannot be overridden). A
// custom provider with supports_websockets=false removes the line, but codex
// files its sessions under that provider: `codex resume` then hides them and
// refuses to resume one ("model provider agentbus ... is no longer
// configured"). Keeping the person's sessions usable is worth more than the
// line. Revisit if codex gains a per-run switch.
func codexWire(b string) ([]string, []string) {
	return nil, []string{"-c", `openai_base_url="` + b + `/backend-api/codex"`}
}

// ownEndpoint names, per agent, the variable that says the person already
// sends this agent somewhere else (their own LLM gateway). The service
// forwards to the vendor, so replacing it would send that gateway's
// credentials to the vendor instead. A run refuses rather than guess.
//
// Known limit: an openai_base_url in codex's own config.toml is not visible
// here. The run's -c replaces it, so that endpoint is bypassed.
var ownEndpoint = map[string]string{
	"claude": "ANTHROPIC_BASE_URL",
	"codex":  "OPENAI_BASE_URL",
}

// runWire is the service's engine.Wire, read from the service's environment. The
// child gets the service's environment with the base URL set; the token is
// never in it, since the service reads the token from its file.
func runWire(getenv func(string) string, environ func() []string, lookPath func(string) (string, error)) engine.Wire {
	return func(agent, baseURL string, acc engine.Access) (engine.Launch, error) {
		wire, ok := harnessWires[agent]
		if !ok {
			return engine.Launch{}, engine.ErrNotFound
		}
		if v := ownEndpoint[agent]; v != "" && getenv(v) != "" {
			return engine.Launch{}, engine.ErrOwnEndpoint
		}
		var env, pre []string
		switch acc.Kind {
		case "", "subscription":
			env, pre = wire(baseURL)
		case "openrouter":
			if acc.KeyEnv == "" || getenv(acc.KeyEnv) == "" {
				return engine.Launch{}, engine.Refused(http.StatusBadRequest, "this agent reads its OpenRouter key from "+
					acc.KeyEnv+", which is not set where the turn runs; set it and restart agentbus")
			}
			env, pre = openRouterWire(agent, baseURL, acc, getenv)
		default:
			return engine.Launch{}, engine.Refused(http.StatusBadRequest, "access must be subscription or openrouter")
		}
		bin, err := lookPath(agent)
		if err != nil {
			return engine.Launch{}, engine.ErrNotFound
		}
		return engine.Launch{Path: bin, Env: childEnv(environ(), env), Args: pre}, nil
	}
}

// openRouterPrefix is the path under a turn's base URL that the service
// forwards to OpenRouter, https://openrouter.ai/api: /openrouter/v1/messages
// for Claude Code, /openrouter/v1/responses for codex. The key travels in the
// agent's own Authorization header, as a sign-in does, and is never stored.
const openRouterPrefix = "/openrouter"

// openRouterProvider is the id codex files the turn's provider under. The ids
// openai, ollama and lmstudio are reserved (codex config reference, read
// 2026-11-08).
const openRouterProvider = "agentbus_openrouter"

// openRouterWire is how an OpenRouter agent reaches its model through the
// service. No format is translated: OpenRouter takes Anthropic Messages and
// the Responses API (research note 2026-11-08, harnesses on other models).
//
// Claude Code: ANTHROPIC_BASE_URL at the service, the key in
// ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY empty (OpenRouter's Claude Code
// guide says it must be), every model alias set to the turn's model so a
// subagent or a small fast call uses it too, and no experimental betas, which
// a non-Anthropic upstream refuses.
//
// codex: a provider of its own, set by -c flags for this run only, reading
// the key from the named variable itself. supports_websockets=false: codex
// then posts at once, with no WebSocket try the service would refuse.
func openRouterWire(agent, baseURL string, acc engine.Access, getenv func(string) string) (env, args []string) {
	base := baseURL + openRouterPrefix
	if agent == "claude" {
		env = []string{
			"ANTHROPIC_BASE_URL=" + base,
			"ANTHROPIC_AUTH_TOKEN=" + getenv(acc.KeyEnv),
			"ANTHROPIC_API_KEY=",
			"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1",
		}
		if acc.Model != "" {
			for _, v := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
				"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"} {
				env = append(env, v+"="+acc.Model)
			}
		}
		return env, nil
	}
	p := "model_providers." + openRouterProvider + "."
	return nil, []string{
		"-c", "model_provider=" + openRouterProvider,
		"-c", p + `name="OpenRouter"`,
		"-c", p + `base_url="` + base + `/v1"`,
		"-c", p + `env_key="` + acc.KeyEnv + `"`,
		"-c", p + `wire_api="responses"`,
		"-c", p + "supports_websockets=false",
	}
}

// childEnv is the parent's environment with each of set replacing any entry
// of the same name. The parent's own environment is not changed.
func childEnv(parent, set []string) []string {
	out := make([]string, 0, len(parent)+len(set))
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		replaced := false
		for _, s := range set {
			if sk, _, _ := strings.Cut(s, "="); sk == k {
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}
