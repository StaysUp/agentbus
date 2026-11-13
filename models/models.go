// Package models is the list of models an agent's access can use now, from
// each source's own data: OpenRouter's official list-models call made by
// control with the OpenRouter key, the output of an unmodified harness run where its sign-in is
// (agy models), what an unmodified harness left in a session's home and a
// runner reported (codex's models_cache.json slugs), or a fixed list
// (claude's aliases). No subscription's own endpoint is ever called from
// here: a subscription's list comes from its unmodified tool only.
//
// Each list is kept with the time it was fetched and fetched again at most
// once an hour, or when the person asks (Lister.List with refresh).
//
// A key is never logged, put in an error or returned. Errors are fixed
// wording: a vendor's reply body is never passed on.
//
// It imports nothing of the module.
package models

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Model is one model an access offers.
type Model struct {
	ID    string `json:"id" doc:"The model's name, as the agent's model names it."`
	Label string `json:"label,omitempty" doc:"Its name for a screen, when the source gives one."`
}

// List is the models of one harness on one access.
type List struct {
	Models []Model `json:"models" doc:"The models, in the source's order or sorted by id. Empty when the source has none yet or could not be read."`
	// Source says where the list came from, for a screen.
	Source    string     `json:"source" doc:"Where the list comes from: api (OpenRouter's list-models call, made by control with the OpenRouter key), command (the unmodified harness, where its sign-in is), runner (what the harness left in a session's home, reported by a runner), fixed (a list of the harness's own aliases)."`
	FetchedAt *time.Time `json:"fetched_at,omitempty" doc:"When the list was fetched or reported. Absent for a fixed list and for one never fetched."`
	Error     string     `json:"error,omitempty" doc:"Why the list is empty or old, in fixed words, such as connect your agy subscription in Settings. The models kept from before, if any, are still listed."`
}

// Source kinds.
const (
	FromAPI     = "api"
	FromCommand = "command"
	FromRunner  = "runner"
	FromFixed   = "fixed"
)

// MaxAge is how long a fetched list is used before it is fetched again.
const MaxAge = time.Hour

// MaxModels bounds one list.
const MaxModels = 2000

// idShape is a model name a list may hold: it becomes an agent's --model,
// so it is the shape a model may have (run.ModelShape is the check that
// counts; this keeps out anything that is plainly not a name).
var idShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$`)

// labelOK is a label fit to show: one short line of printable text.
func labelOK(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// keep is ms with every model whose id is not a name left out, labels that
// cannot be shown dropped, duplicates removed, at most MaxModels.
func keep(ms []Model) []Model {
	out := make([]Model, 0, len(ms))
	seen := map[string]bool{}
	for _, m := range ms {
		if !idShape.MatchString(m.ID) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if !labelOK(m.Label) || m.Label == m.ID {
			m.Label = ""
		}
		out = append(out, m)
		if len(out) == MaxModels {
			break
		}
	}
	return out
}

// Errors, in fixed words.
var (
	ErrRefused     = errors.New("the vendor refused the key; set it again in Settings")
	ErrUnreachable = errors.New("the vendor's list of models could not be reached just now")
	ErrUnreadable  = errors.New("the vendor's list of models could not be read")
)

// maxBody bounds one reply of a list-models call.
const maxBody = 16 << 20

// get is one GET of u with the headers of set, decoded into v.
func get(ctx context.Context, c *http.Client, u string, set func(http.Header), v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ErrUnreachable
	}
	req.Header.Set("Accept", "application/json")
	set(req.Header)
	resp, err := c.Do(req)
	if err != nil {
		return ErrUnreachable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrRefused
	case resp.StatusCode != http.StatusOK:
		return ErrUnreachable
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(b) > maxBody || json.Unmarshal(b, v) != nil {
		return ErrUnreadable
	}
	return nil
}

// OpenRouter lists OpenRouter's models, base such as
// https://openrouter.ai/api: GET /v1/models, with the key as the bearer when
// there is one (openrouter.ai/docs/api-reference/models/get-models, read
// 2026-11-13).
func OpenRouter(ctx context.Context, c *http.Client, base, key string) ([]Model, error) {
	var page struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	err := get(ctx, c, strings.TrimSuffix(base, "/")+"/v1/models", func(h http.Header) {
		if key != "" {
			h.Set("Authorization", "Bearer "+key)
		}
	}, &page)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(page.Data))
	for _, d := range page.Data {
		out = append(out, Model{ID: d.ID, Label: d.Name})
	}
	return keep(out), nil
}

// Lines reads a harness's own listing of its models, one per line as
// "id<TAB>label" (agy models). Any other line is left out: only lines of
// that shape are read, so nothing else the command prints (it may name the
// account) is kept.
func Lines(out []byte) []Model {
	var ms []Model
	for _, line := range strings.Split(string(out), "\n") {
		id, label, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok {
			continue
		}
		ms = append(ms, Model{ID: strings.TrimSpace(id), Label: strings.TrimSpace(label)})
	}
	return keep(ms)
}

// IDs is a list of model names with no labels, as a runner reports them.
func IDs(ids []string) []Model {
	ms := make([]Model, len(ids))
	for i, id := range ids {
		ms[i] = Model{ID: id}
	}
	return keep(ms)
}

// Cache keeps each list with the time it was fetched. Its methods are safe
// to call at once from many goroutines.
type Cache struct {
	Now func() time.Time // nil: time.Now
	mu  sync.Mutex
	m   map[string]*entry
}

type entry struct {
	mu   sync.Mutex // one fetch of this list at a time
	list []Model
	at   time.Time
	err  string
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cache) entry(key string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*entry{}
	}
	e, ok := c.m[key]
	if !ok {
		e = &entry{}
		c.m[key] = e
	}
	return e
}

// Get is the list kept under key: fetched again by fetch when none was
// fetched, when the one kept is MaxAge old, or when refresh is set. A fetch
// that fails keeps the models fetched before, with the error. A fetch for a
// key that is not set (an error, no models at all) leaves nothing kept.
func (c *Cache) Get(key, source string, refresh bool, fetch func() ([]Model, error)) List {
	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	if refresh || e.at.IsZero() || c.now().Sub(e.at) >= MaxAge {
		ms, err := fetch()
		if err != nil {
			e.err = err.Error()
			if e.at.IsZero() {
				// Not fetched yet: try again on the next call.
				return List{Models: []Model{}, Source: source, Error: e.err}
			}
		} else {
			e.list, e.at, e.err = ms, c.now().UTC(), ""
		}
	}
	at := e.at
	return List{Models: append([]Model{}, e.list...), Source: source, FetchedAt: &at, Error: e.err}
}

// Forget drops every list kept under a key that starts with prefix, such as
// a key's lists once the key is replaced.
func (c *Cache) Forget(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if strings.HasPrefix(k, prefix) {
			delete(c.m, k)
		}
	}
}

// Fixed is a list that is not fetched, such as claude's aliases.
func Fixed(ids []string) List {
	return List{Models: IDs(ids), Source: FromFixed}
}

// Reported is a list a runner reported at at, or one with err when none did.
func Reported(ids []string, at time.Time, err string) List {
	if at.IsZero() {
		return List{Models: []Model{}, Source: FromRunner, Error: err}
	}
	at = at.UTC()
	return List{Models: IDs(ids), Source: FromRunner, FetchedAt: &at}
}
