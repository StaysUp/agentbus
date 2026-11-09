package api

// GET /api/v0/harnesses: the programs this service can run an agent on, which
// it finds, their versions, whether each is signed in, and the access and
// models each offers. The "+ Agent" form reads it. docs/CLIENT_API.md is the
// contract.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/harness"
)

// HarnessProbeTimeout bounds each --version or status command.
const HarnessProbeTimeout = 5 * time.Second

// HarnessCacheFor is how long an answer is kept.
const HarnessCacheFor = time.Minute

// Harness is one harness in GET /api/v0/harnesses.
type Harness struct {
	Name     string `json:"name" doc:"The harness, as an agent names it: claude, codex or opencode."`
	Label    string `json:"label" doc:"Its name for a screen, such as Claude Code."`
	Disabled bool   `json:"disabled,omitempty" doc:"True when no agent can be saved on it yet, as for opencode. It is not probed."`
	Found    bool   `json:"found" doc:"The binary is on the service's PATH."`
	Version  string `json:"version,omitempty" doc:"From <bin> --version. Absent when not found or not shaped like a version."`
	SignedIn *bool  `json:"signed_in,omitempty" doc:"From the harness's own offline status command. Absent when it gave no clear answer."`
	// Access lists every kind of access, in the order a form shows them.
	Access []AccessOption `json:"access" doc:"Each way the harness can reach a model, in the order a form shows them."`
}

// AccessOption is one way a harness reaches a model.
type AccessOption struct {
	Kind     string   `json:"kind" doc:"subscription, openrouter or api."`
	Label    string   `json:"label" doc:"Its name for a screen."`
	Disabled bool     `json:"disabled,omitempty" doc:"True when an agent cannot use it yet, as for api."`
	Models   []string `json:"models" doc:"Models to offer, first the one to start from. An agent may name another one when any_model is true."`
	AnyModel bool     `json:"any_model,omitempty" doc:"True when any model name is accepted, as an OpenRouter model id."`
	KeyEnv   string   `json:"key_env,omitempty" doc:"For openrouter: the variable a new agent reads its key from unless it names another."`
	KeySet   *bool    `json:"key_set,omitempty" doc:"For openrouter: whether key_env is set in this service's environment. Its value is never sent."`
}

// HarnessList is the answer to GET /api/v0/harnesses.
type HarnessList struct {
	Harnesses []Harness `json:"harnesses"`
}

type harnessCache struct {
	mu   sync.Mutex
	at   time.Time
	list []Harness
}

func (a *API) listHarnesses(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	// One probe at a time; a caller arriving meanwhile waits and reads the
	// fresh answer.
	a.harnesses.mu.Lock()
	defer a.harnesses.mu.Unlock()
	now := a.cfg.Now()
	if a.harnesses.list == nil || now.Sub(a.harnesses.at) >= HarnessCacheFor {
		list := make([]Harness, 0, len(harness.All())+len(harness.Planned()))
		for _, h := range harness.All() {
			list = append(list, a.probe(r.Context(), h.Info()))
		}
		for _, p := range harness.Planned() {
			list = append(list, Harness{Name: p.Name, Label: p.Label, Disabled: true, Access: []AccessOption{}})
		}
		a.harnesses.list, a.harnesses.at = list, now
	}
	// key_set is read on every call: it is cheap, and a cached answer would
	// hide a variable the person just set.
	out := make([]Harness, len(a.harnesses.list))
	for i, h := range a.harnesses.list {
		h.Access = slices.Clone(h.Access)
		for j := range h.Access {
			if h.Access[j].Kind == agents.OpenRouter {
				set := a.keySet(h.Access[j].KeyEnv)
				h.Access[j].KeySet = &set
			}
		}
		out[i] = h
	}
	writeJSON(w, http.StatusOK, HarnessList{out})
}

// accessOptions is what a harness offers (harness.Info.Access). An
// OpenRouter key is read from agents.DefaultKeyEnv unless an agent names
// another variable.
func accessOptions(info harness.Info) []AccessOption {
	out := make([]AccessOption, len(info.Access))
	for i, ac := range info.Access {
		out[i] = AccessOption{Kind: ac.Kind, Label: ac.Label, Disabled: ac.Disabled, Models: ac.Models, AnyModel: ac.AnyModel}
		if ac.Kind == harness.OpenRouter {
			out[i].KeyEnv = agents.DefaultKeyEnv
		}
	}
	return out
}

// keySet says whether name is set, and not empty, in the service's
// environment. Its value is never kept or sent.
func (a *API) keySet(name string) bool {
	if a.cfg.Environ == nil || name == "" {
		return false
	}
	for _, kv := range a.cfg.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v != ""
		}
	}
	return false
}

// probe finds one harness's binary and asks it its version and sign-in
// status: its own offline status command, whose exit code is the answer, 0
// signed in and 1 not. That command's output is discarded, since it can name
// the account. It never returns the path.
func (a *API) probe(ctx context.Context, info harness.Info) Harness {
	ag := Harness{Name: info.Name, Label: info.Label, Access: accessOptions(info)}
	if a.cfg.LookPath == nil {
		return ag
	}
	bin, err := a.cfg.LookPath(info.Name)
	if err != nil {
		return ag
	}
	ag.Found = true
	out, code := a.runProbe(ctx, bin, "--version")
	if code == 0 {
		ag.Version = versionToken(out)
	}
	if args := info.StatusArgs; len(args) > 0 {
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
// within HarnessProbeTimeout. It returns at most 4 KiB of stdout and the exit
// code, or -1 when the command did not end on its own.
func (a *API) runProbe(ctx context.Context, bin string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, HarnessProbeTimeout)
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
