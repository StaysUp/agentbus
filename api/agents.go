package api

// GET /api/v0/agents: which agents this service can start, their versions,
// and whether each is signed in. docs/CLIENT_API.md is the contract.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AgentNames are the agents runs can start, in the order they are listed.
var AgentNames = []string{"claude", "codex"}

// AgentProbeTimeout bounds each --version or status command.
const AgentProbeTimeout = 5 * time.Second

// AgentCacheFor is how long an answer is kept.
const AgentCacheFor = time.Minute

// statusArgs is each agent's own offline sign-in status command. Its exit
// code is the answer: 0 signed in, 1 not. Its output is discarded, since it
// can name the account. Both answer with network access denied (Claude Code
// 2.1.318 and codex-cli 0.164.0, 2026-11-05, run under a sandbox profile
// that denies the network).
var statusArgs = map[string][]string{
	"claude": {"auth", "status"},
	"codex":  {"login", "status"},
}

// Agent is one agent in GET /api/v0/agents.
type Agent struct {
	Name     string `json:"name"`
	Found    bool   `json:"found" doc:"The binary is on the service's PATH."`
	Version  string `json:"version,omitempty" doc:"From <bin> --version. Absent when not found or not shaped like a version."`
	SignedIn *bool  `json:"signed_in,omitempty" doc:"From the agent's own offline status command. Absent when it gave no clear answer."`
}

// AgentList is the answer to GET /api/v0/agents.
type AgentList struct {
	Agents []Agent `json:"agents"`
}

type agentCache struct {
	mu   sync.Mutex
	at   time.Time
	list []Agent
}

func (a *API) listAgents(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	// One probe at a time; a caller arriving meanwhile waits and reads the
	// fresh answer.
	a.agents.mu.Lock()
	defer a.agents.mu.Unlock()
	now := a.cfg.Now()
	if a.agents.list == nil || now.Sub(a.agents.at) >= AgentCacheFor {
		list := make([]Agent, 0, len(AgentNames))
		for _, name := range AgentNames {
			list = append(list, a.probe(r.Context(), name))
		}
		a.agents.list, a.agents.at = list, now
	}
	writeJSON(w, http.StatusOK, AgentList{a.agents.list})
}

// probe finds one agent's binary and asks it its version and sign-in status.
// It never returns the path.
func (a *API) probe(ctx context.Context, name string) Agent {
	ag := Agent{Name: name}
	if a.cfg.LookPath == nil {
		return ag
	}
	bin, err := a.cfg.LookPath(name)
	if err != nil {
		return ag
	}
	ag.Found = true
	out, code := a.runProbe(ctx, bin, "--version")
	if code == 0 {
		ag.Version = versionToken(out)
	}
	if args, ok := statusArgs[name]; ok {
		switch _, code := a.runProbe(ctx, bin, args...); code {
		case 0:
			t := true
			ag.SignedIn = &t
		case 1:
			f := false
			ag.SignedIn = &f
		}
	}
	return ag
}

// runProbe runs bin with args, an empty stdin and the service's environment,
// within AgentProbeTimeout. It returns at most 4 KiB of stdout and the exit
// code, or -1 when the command did not end on its own.
func (a *API) runProbe(ctx context.Context, bin string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, AgentProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if a.cfg.Environ != nil {
		cmd.Env = a.cfg.Environ()
	}
	out := &capped{max: 4 << 10}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", -1
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ee) && ee.Exited():
		return out.String(), ee.ExitCode()
	}
	return "", -1
}

var versionShape = regexp.MustCompile(`^v?([0-9]+\.[0-9]+(\.[0-9]+)?([-+][0-9A-Za-z.-]+)?)$`)

// versionToken is the first word of out shaped like a version: "2.1.318" from
// "2.1.318 (Claude Code)", "0.164.0" from "codex-cli 0.164.0".
func versionToken(out string) string {
	for _, f := range strings.Fields(out) {
		if m := versionShape.FindStringSubmatch(f); m != nil {
			return m[1]
		}
	}
	return ""
}

// capped keeps the first max bytes written to it.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
}
