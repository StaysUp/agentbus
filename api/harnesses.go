package api

// GET /api/v0/harnesses: the programs an agent can run on, which the runners
// find, their versions, whether each is signed in, and the access and models
// each offers. Each runner probes its own machine and reports it (package
// runner, queue.Capabilities); this service never runs an agent's program to
// answer. The "+ Agent" form reads it. docs/CLIENT_API.md is the contract.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/queue"
)

// HarnessReportWait bounds how long GET /api/v0/harnesses waits for a runner
// that has enrolled but not yet reported its first probe, as just after
// agentbus up starts.
const HarnessReportWait = 15 * time.Second

// Harness is one harness in GET /api/v0/harnesses.
type Harness struct {
	Name     string `json:"name" doc:"The harness, as an agent names it: claude, codex or opencode."`
	Label    string `json:"label" doc:"Its name for a screen, such as Claude Code."`
	Disabled bool   `json:"disabled,omitempty" doc:"True when no agent can be saved on it yet, as for opencode. It is not probed."`
	Found    bool   `json:"found" doc:"The binary is on the PATH of a runner seen in the last minute."`
	Version  string `json:"version,omitempty" doc:"From <bin> --version, on the first runner that found it (the newest seen). Absent when not found or not shaped like a version."`
	SignedIn *bool  `json:"signed_in,omitempty" doc:"From the harness's own offline status command, on the same runner. Absent when it gave no clear answer."`
	// Runners: what each runner reported of it.
	Runners []RunnerHarness `json:"runners" doc:"What each runner seen in the last minute reported of this harness, newest seen first. Empty when none did."`
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

// RunnerHarness is one runner's report of one harness.
type RunnerHarness struct {
	Runner   string `json:"runner" doc:"The runner's id."`
	Name     string `json:"name" doc:"The runner's name."`
	Found    bool   `json:"found" doc:"The binary is on that runner's PATH."`
	Version  string `json:"version,omitempty" doc:"From <bin> --version there."`
	SignedIn *bool  `json:"signed_in,omitempty" doc:"From the harness's own offline status command there."`
}

// HarnessList is the answer to GET /api/v0/harnesses.
type HarnessList struct {
	Harnesses []Harness `json:"harnesses"`
}

func (a *API) listHarnesses(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	runners, err := a.reportedRunners(r.Context())
	if err != nil {
		a.internal(w)
		return
	}
	list := make([]Harness, 0, len(harness.All())+len(harness.Planned()))
	for _, h := range harness.All() {
		info := h.Info()
		ag := Harness{Name: info.Name, Label: info.Label, Access: accessOptions(info), Runners: []RunnerHarness{}}
		for _, rn := range runners {
			for _, rep := range rn.Caps.Harnesses {
				if rep.Name != info.Name {
					continue
				}
				ag.Runners = append(ag.Runners, RunnerHarness{Runner: rn.ID, Name: rn.Name, Found: rep.Found,
					Version: rep.Version, SignedIn: rep.SignedIn})
				if rep.Found && !ag.Found {
					ag.Found, ag.Version, ag.SignedIn = true, rep.Version, rep.SignedIn
				}
			}
		}
		// key_set is read on every call: it is cheap, and an answer kept
		// would hide a variable the person just set. The key is the
		// control plane's: runners hold no secrets.
		for j := range ag.Access {
			if ag.Access[j].Kind == agents.OpenRouter {
				set := a.keySet(ag.Access[j].KeyEnv)
				ag.Access[j].KeySet = &set
			}
		}
		list = append(list, ag)
	}
	for _, p := range harness.Planned() {
		list = append(list, Harness{Name: p.Name, Label: p.Label, Disabled: true, Access: []AccessOption{}, Runners: []RunnerHarness{}})
	}
	writeJSON(w, http.StatusOK, HarnessList{list})
}

// reportedRunners are the runners seen within queue.RunnerFresh that have
// reported their harnesses, newest seen first. While a runner so seen has
// not reported yet, it waits for that report, up to HarnessReportWait.
func (a *API) reportedRunners(ctx context.Context) ([]queue.Runner, error) {
	if a.cfg.Queue == nil {
		return nil, nil
	}
	deadline := a.cfg.Now().Add(HarnessReportWait)
	for {
		all, err := a.cfg.Queue.Runners(queue.DefaultTeam)
		if err != nil {
			return nil, err
		}
		var out []queue.Runner
		waiting := false
		now := a.cfg.Now()
		for _, rn := range all {
			if now.Sub(rn.Seen) > queue.RunnerFresh {
				continue
			}
			if rn.Caps == nil || rn.Caps.Harnesses == nil {
				waiting = true
				continue
			}
			out = append(out, rn)
		}
		if !waiting || len(out) > 0 || !a.cfg.Now().Before(deadline) {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
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

// FolderList is the answer to GET /api/v0/folders.
type FolderList struct {
	Folders []queue.RunnerFolder `json:"folders" doc:"Each folder a runner reported, newest report first."`
}

func (a *API) listFolders(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	list := []queue.RunnerFolder{}
	if a.cfg.Queue != nil {
		var err error
		if list, err = a.cfg.Queue.Folders(queue.DefaultTeam); err != nil {
			a.internal(w)
			return
		}
	}
	writeJSON(w, http.StatusOK, FolderList{list})
}
