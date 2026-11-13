package api

// GET /api/v0/harnesses/{id}/models: the models a harness's access can use
// now, from each source's own data (package models). The agent form offers
// them; a typed name is still accepted, and no model is still the tool's own
// default.

import (
	"context"
	"net/http"
	"time"

	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/models"
)

// ModelList is the answer to GET /api/v0/harnesses/{id}/models.
type ModelList struct {
	Harness string `json:"harness" doc:"The harness."`
	Access  string `json:"access" doc:"The access: subscription, openrouter or api."`
	models.List
}

// ModelSources is how control reads each list (Config.Models). Every field
// may be nil: that source then has no list.
type ModelSources struct {
	Cache  *models.Cache
	Client *http.Client
	// OpenRouterBase is OpenRouter's API, such as https://openrouter.ai/api.
	OpenRouterBase string
	// OpenRouterKey is the OpenRouter key an agent of control would get: the
	// one set in Settings, or the default variable's. "" lists without one.
	OpenRouterKey func() string
	// APIBase is each vendor API of access "api", by the key's name in
	// Settings (harness.APIKey.Name), such as https://api.anthropic.com.
	APIBase map[string]string
	// APIKey is the key of that name set in Settings, or "" when none is.
	APIKey func(name string) string
	// Own runs the unmodified harness in its home on control so it prints
	// its models (credentials.Subscriptions.OwnModels).
	Own func(harness string) ([]byte, error)
}

var modelsRoute = route{method: "GET", path: "/api/v0/harnesses/{id}/models", op: "listModels",
	idDoc:   "A harness, such as claude. Another shape gets 404.",
	tag:     "Agents",
	summary: "List a harness's models",
	doc: "The models a harness can use now on an access, for a form that saves an agent, from each source's own data. " +
		"access openrouter: OpenRouter's list-models call, made by control with the OpenRouter key. " +
		"access subscription: claude's aliases opus, sonnet and haiku (always the newest of each); agy's own agy models, run unmodified where its sign-in is, on control; " +
		"codex's models_cache.json, which unmodified codex writes in a session's home, its model names reported by the runner after a turn. " +
		"No subscription's own endpoint is called by AgentBus. A list is kept with the time it was fetched and fetched again at most once an hour, or now with refresh=true. " +
		"An agent may still name a model the list lacks, and one that names none uses the tool's own default.",
	query: []param{{name: "access", doc: "subscription, openrouter or api. Required."},
		{name: "refresh", doc: "true fetches the list again now, rather than the one kept within the hour."}},
	replies: []reply{ok(200, ModelList{}, "The models, or none with why."), fail(400, "No access was named."), fail(404, "No such harness, or it offers no such access.")},
	turn:    anyRead,
	serve:   (*API).listModels}

func (a *API) listModels(w http.ResponseWriter, r *http.Request, id string) {
	h, ok := harness.Get(id)
	q := r.URL.Query()
	access := q.Get("access")
	if access == "" {
		writeError(w, http.StatusBadRequest, "access is required: subscription, openrouter or api")
		return
	}
	if !ok || !harness.Offers(h.Info(), access) {
		writeError(w, http.StatusNotFound, "no such harness, or it offers no such access")
		return
	}
	refresh := q.Get("refresh") == "true"
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, ModelList{Harness: id, Access: access, List: a.modelList(ctx, h.Info(), access, refresh)})
}

// modelList is info's list on access.
func (a *API) modelList(ctx context.Context, info harness.Info, access string, refresh bool) models.List {
	src := a.cfg.Models
	none := func(source, why string) models.List {
		return models.List{Models: []models.Model{}, Source: source, Error: why}
	}
	if src == nil || src.Cache == nil {
		return none("", "this service lists no models")
	}
	client := src.Client
	if client == nil {
		client = http.DefaultClient
	}
	switch access {
	case agents.OpenRouter:
		if src.OpenRouterBase == "" {
			return none(models.FromAPI, "this service lists no OpenRouter models")
		}
		key := ""
		if src.OpenRouterKey != nil {
			key = src.OpenRouterKey()
		}
		return src.Cache.Get("openrouter", models.FromAPI, refresh, func() ([]models.Model, error) {
			return models.OpenRouter(ctx, client, src.OpenRouterBase, key)
		})
	case agents.API:
		return a.apiKeyModels(ctx, client, info, refresh)
	}
	// A subscription: through the unmodified tool only.
	switch {
	case info.Login != nil && len(info.Login.ModelsArgs) > 0:
		if src.Own == nil {
			return none(models.FromCommand, "this service runs no harness to list its models")
		}
		if st, err := a.subscriptionStatus(info.Name); err != nil || !st.Connected {
			return none(models.FromCommand, credentials.NotConnected(info.Name))
		}
		return src.Cache.Get("own/"+info.Name, models.FromCommand, refresh, func() ([]models.Model, error) {
			out, err := src.Own(info.Name)
			if err != nil {
				return nil, err
			}
			return models.Lines(out), nil
		})
	case info.ModelsCache != "":
		ids, at := a.reportedModels(ctx, info.Name)
		return models.Reported(ids, at, "no runner has reported "+info.Label+"'s models yet: they come with its first turn")
	}
	for _, ac := range info.Access {
		if ac.Kind == harness.Subscription {
			return models.Fixed(ac.Models)
		}
	}
	return none(models.FromFixed, "")
}

// subscriptionStatus is harness's subscription on control.
func (a *API) subscriptionStatus(name string) (credentials.Status, error) {
	if a.cfg.Subscriptions == nil || a.cfg.Subscriptions.Store == nil {
		return credentials.Status{}, credentials.ErrNotConnected
	}
	return a.cfg.Subscriptions.Store.Status(name)
}

// reportedModels are the model names the newest report of harness's models
// cache holds, from the runners seen within queue.RunnerFresh, and when it
// was read.
func (a *API) reportedModels(ctx context.Context, name string) ([]string, time.Time) {
	runners, err := a.reportedRunners(ctx)
	if err != nil {
		return nil, time.Time{}
	}
	var ids []string
	var at time.Time
	for _, rn := range runners {
		for _, rep := range rn.Caps.Harnesses {
			if rep.Name == name && rep.ModelsAt != nil && rep.ModelsAt.After(at) && len(rep.Models) > 0 {
				ids, at = rep.Models, *rep.ModelsAt
			}
		}
	}
	return ids, at
}

// apiKeyModels is the list of info's access "api": its vendor's
// list-models call with the key set in Settings. With no key set there is
// no call and no list.
func (a *API) apiKeyModels(ctx context.Context, client *http.Client, info harness.Info, refresh bool) models.List {
	k := info.APIKey
	src := a.cfg.Models
	none := func(why string) models.List {
		return models.List{Models: []models.Model{}, Source: models.FromAPI, Error: why}
	}
	if k == nil || src.APIBase[k.Name] == "" || src.APIKey == nil {
		return none("this service lists no models for " + info.Label + " on an API key")
	}
	key := src.APIKey(k.Name)
	if key == "" {
		return none("set your " + k.Label + " in Settings")
	}
	list := models.OpenAI
	switch k.Format {
	case harness.FormatAnthropic:
		list = models.Anthropic
	case harness.FormatGemini:
		list = models.Gemini
	}
	base := src.APIBase[k.Name]
	return src.Cache.Get(keyModels(k.Name), models.FromAPI, refresh, func() ([]models.Model, error) {
		return list(ctx, client, base, key)
	})
}

// keyModels is the cache key of the list fetched with the key named name.
func keyModels(name string) string {
	if name == credentials.AccessOpenRouter {
		return "openrouter"
	}
	return "key/" + name
}

// forgetModels drops the list fetched with the key named name: it was
// replaced or removed.
func (a *API) forgetModels(name string) {
	if a.cfg.Models != nil && a.cfg.Models.Cache != nil {
		a.cfg.Models.Cache.Forget(keyModels(name))
	}
}
