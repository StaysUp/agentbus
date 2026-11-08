package main

// How a run's child reaches the service: the session's base URL goes into the
// child's environment or arguments only, never the service's own.

import (
	"strings"

	engine "github.com/staysup/agentbus/run"
)

// agents maps each supported agent to how its base URL reaches the child.
var agents = map[string]func(baseURL string) (env []string, args []string){
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
	return func(agent, baseURL string) (engine.Launch, error) {
		wire, ok := agents[agent]
		if !ok {
			return engine.Launch{}, engine.ErrNotFound
		}
		if v := ownEndpoint[agent]; v != "" && getenv(v) != "" {
			return engine.Launch{}, engine.ErrOwnEndpoint
		}
		bin, err := lookPath(agent)
		if err != nil {
			return engine.Launch{}, engine.ErrNotFound
		}
		env, pre := wire(baseURL)
		return engine.Launch{Path: bin, Env: childEnv(environ(), env), Args: pre}, nil
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
