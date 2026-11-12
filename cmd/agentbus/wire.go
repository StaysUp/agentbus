package main

// How a run's child reaches the service: the session's base URL goes into the
// child's environment or arguments only, never the service's own.

import (
	"net/http"
	"strings"

	"github.com/staysup/agentbus/harness"
	engine "github.com/staysup/agentbus/run"
)

// runWire is the service's engine.Wire, read from the service's environment. The
// child gets the service's environment with the base URL set; the token is
// never in it, since the service reads the token from its file.
func runWire(getenv func(string) string, environ func() []string, lookPath func(string) (string, error)) engine.Wire {
	return func(agent, baseURL string, acc engine.Access) (engine.Launch, error) {
		h, ok := harness.Get(agent)
		if !ok {
			return engine.Launch{}, engine.ErrNotFound
		}
		if v := h.Info().OwnEndpointEnv; v != "" && getenv(v) != "" {
			return engine.Launch{}, engine.ErrOwnEndpoint
		}
		// A harness runs only on the access it offers: agy has no
		// OpenRouter way, and its Wire would send it to its own sign-in.
		if !harness.Offers(h.Info(), acc.Kind) {
			return engine.Launch{}, engine.Refused(http.StatusBadRequest, h.Info().Label+" cannot run on access "+acc.Kind)
		}
		var env, pre []string
		var files map[string][]byte
		switch {
		case acc.Placeholder != "" && (acc.Kind == "" || acc.Kind == harness.Subscription):
			// A runner that holds no secrets: the child's sign-in is the
			// placeholder, and control's gateway adds the person's own
			// subscription token, connected once on control.
			if h.Info().Login == nil {
				return engine.Launch{}, engine.Refused(http.StatusBadRequest, "a "+h.Info().Label+
					" subscription cannot be connected on control yet; use an OpenRouter agent")
			}
			env, pre = h.WireRelay(baseURL, acc.Placeholder)
			if f, ok := h.(harness.HomeFiles); ok {
				files = f.RelayHome(acc.Placeholder)
			}
		case acc.Placeholder != "" && acc.Kind == harness.OpenRouter:
			// The key's variable holds the placeholder; control's gateway
			// adds the key from its own environment. Nothing of this
			// machine's environment is read.
			if acc.KeyEnv == "" {
				return engine.Launch{}, engine.Refused(http.StatusBadRequest, "this agent names no variable for its OpenRouter key")
			}
			env, pre = h.WireOpenRouter(harness.OpenRouterWire{Base: baseURL + openRouterPrefix,
				KeyEnv: acc.KeyEnv, Key: acc.Placeholder, Model: acc.Model})
			env = append(env, acc.KeyEnv+"="+acc.Placeholder)
		case acc.Kind == "" || acc.Kind == harness.Subscription:
			env, pre = h.Wire(baseURL)
		case acc.Kind == harness.OpenRouter:
			if acc.KeyEnv == "" || getenv(acc.KeyEnv) == "" {
				return engine.Launch{}, engine.Refused(http.StatusBadRequest, "this agent reads its OpenRouter key from "+
					acc.KeyEnv+", which is not set where the turn runs; set it and restart agentbus")
			}
			env, pre = h.WireOpenRouter(harness.OpenRouterWire{Base: baseURL + openRouterPrefix,
				KeyEnv: acc.KeyEnv, Key: getenv(acc.KeyEnv), Model: acc.Model})
		default:
			return engine.Launch{}, engine.Refused(http.StatusBadRequest, "access must be subscription or openrouter")
		}
		bin, err := lookPath(agent)
		if err != nil {
			return engine.Launch{}, engine.ErrNotFound
		}
		return engine.Launch{Path: bin, Env: childEnv(environ(), env), Args: pre, HomeFiles: files}, nil
	}
}

// openRouterPrefix is the path under a turn's base URL that the service
// forwards to OpenRouter, https://openrouter.ai/api: /openrouter/v1/messages
// for Claude Code, /openrouter/v1/responses for codex. The key travels in the
// agent's own Authorization header, as a sign-in does, and is never stored.
// No format is translated: OpenRouter takes Anthropic Messages and the
// Responses API (research note 2026-11-08, harnesses on other models). Each
// harness says how its child is pointed there (harness.Harness.WireOpenRouter).
const openRouterPrefix = "/openrouter"

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
