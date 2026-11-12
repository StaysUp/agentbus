package api

// /api/v0/agents: the saved agents. An agent is a harness, how it reaches its
// model, the model and a name; a turn names one by id.

import (
	"errors"
	"net/http"
	"time"

	"github.com/staysup/agentbus/agents"
)

// AgentList is the answer to GET /api/v0/agents.
type AgentList struct {
	Agents []SavedAgent `json:"agents"`
}

// SavedAgent is one saved agent, as the API sends it: agents.Agent and
// key_set.
type SavedAgent struct {
	ID      string    `json:"id" doc:"The agent's id. A turn names it in agent."`
	Name    string    `json:"name" doc:"What the person calls it, such as Claude Code · sonnet. Unique, ignoring case."`
	Harness string    `json:"harness" doc:"The program that runs the agent's loop: claude or codex."`
	Access  string    `json:"access" doc:"How the harness reaches its model: subscription (the person's, connected on control) or openrouter (a key from an environment variable)."`
	Model   string    `json:"model,omitempty" doc:"The model, passed as --model. Absent: the harness's own default."`
	KeyEnv  string    `json:"key_env,omitempty" doc:"For openrouter: the name of the environment variable on the runner's machine that holds the key. Never the key."`
	KeySet  *bool     `json:"key_set,omitempty" doc:"For openrouter: whether key_env is set in this service's environment. Its value is never sent."`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// AgentRequest is the body of POST /api/v0/agents and PUT /api/v0/agents/{id}.
type AgentRequest agents.Fields

const (
	noAgents   = "this service keeps no saved agents"
	noAgent    = "no such agent"
	badAgent   = "the body must be one JSON object with name, harness and access, and optionally model and key_env"
	tooManyAgs = "too many agents; remove some first"
)

func (a *API) saved(ag agents.Agent) SavedAgent {
	s := SavedAgent{ID: ag.ID, Name: ag.Name, Harness: ag.Harness, Access: ag.Access, Model: ag.Model,
		KeyEnv: ag.KeyEnv, Created: ag.Created, Updated: ag.Updated}
	if ag.Access == agents.OpenRouter {
		set := a.keySet(ag.KeyEnv)
		s.KeySet = &set
	}
	return s
}

func (a *API) listSaved(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	if a.cfg.Agents == nil {
		writeJSON(w, http.StatusOK, AgentList{[]SavedAgent{}})
		return
	}
	list := a.cfg.Agents.List()
	out := make([]SavedAgent, 0, len(list))
	for _, ag := range list {
		out = append(out, a.saved(ag))
	}
	writeJSON(w, http.StatusOK, AgentList{out})
}

func (a *API) getSaved(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	if a.cfg.Agents == nil {
		writeError(w, http.StatusNotFound, noAgents)
		return
	}
	ag, err := a.cfg.Agents.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, noAgent)
		return
	}
	writeJSON(w, http.StatusOK, a.saved(ag))
}

func (a *API) createSaved(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Agents == nil {
		writeError(w, http.StatusNotFound, noAgents)
		return
	}
	var f AgentRequest
	if !decodeObject(r, &f) {
		writeError(w, http.StatusBadRequest, badAgent)
		return
	}
	ag, err := a.cfg.Agents.Create(agents.Fields(f))
	if a.agentFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, a.saved(ag))
}

func (a *API) updateSaved(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Agents == nil {
		writeError(w, http.StatusNotFound, noAgents)
		return
	}
	var f AgentRequest
	if !decodeObject(r, &f) {
		writeError(w, http.StatusBadRequest, badAgent)
		return
	}
	ag, err := a.cfg.Agents.Update(id, agents.Fields(f))
	if a.agentFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, a.saved(ag))
}

func (a *API) removeSaved(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	if a.cfg.Agents == nil {
		writeError(w, http.StatusNotFound, noAgents)
		return
	}
	if a.agentFailed(w, a.cfg.Agents.Remove(id)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentFailed answers err, if there is one, and says whether it did.
func (a *API) agentFailed(w http.ResponseWriter, err error) bool {
	var bad *agents.Invalid
	switch {
	case err == nil:
		return false
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Why)
	case errors.Is(err, agents.ErrNotFound):
		writeError(w, http.StatusNotFound, noAgent)
	case errors.Is(err, agents.ErrFull):
		writeError(w, http.StatusConflict, tooManyAgs)
	default:
		a.internal(w)
	}
	return true
}
