// Package agents keeps the saved agents: a harness, how it reaches its model,
// the model and a name. An agent says nothing about where it runs. They are
// kept in control's settings database (settings.DB): the default agents, one
// per harness on its own subscription, are in code, and the database holds
// only the agents the person added, changed or removed. agents.json, where
// 0.4 and before kept them, is read once by the upgrade (ImportRows).
//
// An OpenRouter agent names the environment variable that holds its key,
// never the key. The runner reads the variable when a turn starts.
package agents

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// FileName is the file in the data folder that holds the agents.
const FileName = settings.AgentsFile

// Access kinds (harness.Subscription and the rest).
const (
	Subscription = harness.Subscription // the harness's own sign-in
	OpenRouter   = harness.OpenRouter   // an OpenRouter key, from an environment variable
	API          = harness.API          // the vendor's API key: not available yet
)

// DefaultKeyEnv is the variable an OpenRouter agent reads when none is named.
const DefaultKeyEnv = "OPENROUTER_API_KEY"

// MaxName bounds an agent's name, in bytes.
const MaxName = 100

// MaxAgents bounds how many agents are kept.
const MaxAgents = 200

// Agent is one saved agent.
type Agent struct {
	ID      string    `json:"id" doc:"The agent's id. A turn names it in agent."`
	Name    string    `json:"name" doc:"What the person calls it, such as Claude Code · sonnet. Unique, ignoring case."`
	Harness string    `json:"harness" doc:"The program that runs the agent's loop: claude or codex."`
	Access  string    `json:"access" doc:"How the harness reaches its model: subscription (its own sign-in) or openrouter (a key from an environment variable)."`
	Model   string    `json:"model,omitempty" doc:"The model, passed as --model. Absent: the harness's own default. Required for openrouter, in OpenRouter's naming, such as deepseek/deepseek-v4.1-flash."`
	KeyEnv  string    `json:"key_env,omitempty" doc:"For openrouter: the name of the environment variable on the runner's machine that holds the key. Never the key."`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Fields are what a person sets on an agent: everything but its id and times.
type Fields struct {
	Name    string `json:"name" doc:"1 to 100 bytes, unique among the agents, ignoring case."`
	Harness string `json:"harness" doc:"claude or codex. opencode is not available yet."`
	Access  string `json:"access" doc:"subscription or openrouter. api is not available yet."`
	Model   string `json:"model,omitempty" doc:"Optional for subscription. Required for openrouter."`
	KeyEnv  string `json:"key_env,omitempty" doc:"For openrouter only: a variable name ending in _KEY, such as OPENROUTER_API_KEY, which is used when this is absent."`
}

// keyEnvShape is a variable name an OpenRouter agent may read. It must end in
// _KEY, so an agent cannot be pointed at any variable of the service.
var keyEnvShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,123}_KEY$`)

// KeyEnvShape is the shape of Fields.KeyEnv, for the API's description.
func KeyEnvShape() string { return keyEnvShape.String() }

// idShape is the shape of an agent id: the API's path id shape.
var idShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Errors.
var (
	ErrNotFound = errors.New("agents: no such agent")
	ErrFull     = errors.New("agents: too many agents")
)

// Invalid says why fields were refused, in words fit for the person.
type Invalid struct{ Why string }

func (e *Invalid) Error() string { return e.Why }

// Check says what is wrong with f, or "". It fills KeyEnv for openrouter and
// trims the name.
func Check(f *Fields) string {
	f.Name = strings.TrimSpace(f.Name)
	switch {
	case f.Name == "" || len(f.Name) > MaxName || !utf8.ValidString(f.Name) || strings.ContainsAny(f.Name, "\x00\r\n"):
		return "name must be 1 to 100 bytes of text on one line"
	case planned(f.Harness):
		return "harness " + f.Harness + " is not available yet; use " + harness.NameList()
	case !known(f.Harness):
		return "harness must be " + harness.NameList()
	case f.Access == API:
		return "access api is not available yet; use subscription or openrouter"
	case f.Access != Subscription && f.Access != OpenRouter:
		return "access must be subscription or openrouter"
	case !offers(f.Harness, f.Access):
		return "harness " + f.Harness + " cannot use access " + f.Access + " yet"
	case f.Model != "" && !run.ModelShape.MatchString(f.Model):
		return "model must match " + run.ModelShape.String()
	}
	if f.Access == OpenRouter {
		if f.Model == "" {
			return "an openrouter agent needs a model, such as deepseek/deepseek-v4.1-flash"
		}
		if f.KeyEnv == "" {
			f.KeyEnv = DefaultKeyEnv
		}
		if !keyEnvShape.MatchString(f.KeyEnv) {
			return "key_env must be a variable name ending in _KEY, such as " + DefaultKeyEnv
		}
	} else if f.KeyEnv != "" {
		return "key_env is for openrouter agents only"
	}
	return ""
}

// Backend keeps the agents the person added, changed or removed: the
// settings database (settings.DB). The default agents are not kept there
// unless the person changed or removed one.
type Backend interface {
	Agents() ([]settings.AgentRow, error)
	SetAgent(r settings.AgentRow, keys []string) error
	Created() time.Time
}

// Store keeps the saved agents. Its methods are safe to call at once from
// many goroutines of one process.
type Store struct {
	b    Backend
	now  func() time.Time
	mu   sync.Mutex
	list []Agent
}

type file struct {
	Agents []Agent `json:"agents"`
}

// Defaults are the agents a control has unless the person changed or removed
// them, or its data folder started without them (Starters): one per harness,
// on its own subscription. Their ids are the
// harness names, so a turn that names claude or codex finds them. A default
// agent grants nothing a turn naming its harness does not already get.
func Defaults(now time.Time) []Agent {
	var out []Agent
	for _, h := range harness.All() {
		i := h.Info()
		out = append(out, Agent{ID: i.Name, Name: i.Label, Harness: i.Name, Access: Subscription, Created: now, Updated: now})
	}
	return out
}

// Starters are the rows a new data folder starts with, so that its agents
// are the defaults whose harness is in found: a removed row for each other
// default. found nil starts with none (agentbus server, which has no
// machine yet). A turn that names a removed default's id still starts that
// harness on its own subscription (Resolve), as old clients expect.
func Starters(found []string) []settings.AgentRow {
	var out []settings.AgentRow
	for _, d := range Defaults(time.Time{}) {
		if !slices.Contains(found, d.Harness) {
			out = append(out, settings.AgentRow{ID: d.ID, Removed: true})
		}
	}
	return out
}

// known says an agent can be saved on name.
func known(name string) bool {
	_, ok := harness.Get(name)
	return ok
}

// offers says the harness name can run on access.
func offers(name, access string) bool {
	h, ok := harness.Get(name)
	return ok && harness.Offers(h.Info(), access)
}

// planned says name is a harness listed as coming.
func planned(name string) bool {
	for _, p := range harness.Planned() {
		if p.Name == name {
			return true
		}
	}
	return false
}

// Open reads the agents from b: the defaults the person kept, as changed,
// then the agents the person added. now nil means time.Now.
func Open(b Backend, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	rows, err := b.Agents()
	if err != nil {
		return nil, err
	}
	s := &Store{b: b, now: now}
	if s.list, err = merge(rows, b.Created().UTC()); err != nil {
		return nil, err
	}
	return s, nil
}

// merge is the defaults with rows on top: a row of a default's id replaces
// it in place, or removes it; the other rows follow in their order.
func merge(rows []settings.AgentRow, created time.Time) ([]Agent, error) {
	byID := map[string]settings.AgentRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	decode := func(r settings.AgentRow) (Agent, error) {
		var a Agent
		if err := json.Unmarshal(r.Data, &a); err != nil || a.ID != r.ID || !idShape.MatchString(a.ID) {
			return Agent{}, fmt.Errorf("agents: the stored agent %s cannot be read", strconv.Quote(r.ID))
		}
		return a, nil
	}
	var out []Agent
	isDefault := map[string]bool{}
	for _, d := range Defaults(created) {
		isDefault[d.ID] = true
		r, ok := byID[d.ID]
		switch {
		case !ok:
			out = append(out, d)
		case r.Removed:
		default:
			a, err := decode(r)
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		}
	}
	for _, r := range rows {
		if isDefault[r.ID] || r.Removed {
			continue
		}
		a, err := decode(r)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ImportRows reads an agents.json of 0.4 and before as the rows the
// settings database keeps: an agent that is a default as it was is left
// out, a default missing from the file is a removed row, and every other
// agent is kept whole.
func ImportRows(data []byte) ([]settings.AgentRow, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("agents: %s is not valid: %w", FileName, err)
	}
	seen := map[string]bool{}
	var out []settings.AgentRow
	for _, a := range f.Agents {
		if !idShape.MatchString(a.ID) {
			return nil, fmt.Errorf("agents: %s has an agent with the id %s, which is not a valid id", FileName, strconv.Quote(a.ID))
		}
		if seen[a.ID] {
			return nil, fmt.Errorf("agents: %s has the id %s twice", FileName, strconv.Quote(a.ID))
		}
		seen[a.ID] = true
		if isDefault(a) {
			continue
		}
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		out = append(out, settings.AgentRow{ID: a.ID, Data: b})
	}
	for _, d := range Defaults(time.Time{}) {
		if !seen[d.ID] {
			out = append(out, settings.AgentRow{ID: d.ID, Removed: true})
		}
	}
	return out, nil
}

// isDefault says a is a default agent with its default fields.
func isDefault(a Agent) bool {
	for _, d := range Defaults(time.Time{}) {
		if d.ID == a.ID {
			return a.Name == d.Name && a.Harness == d.Harness && a.Access == d.Access && a.Model == d.Model && a.KeyEnv == d.KeyEnv
		}
	}
	return false
}

// List is every agent, in the order they were made.
func (s *Store) List() []Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Agent(nil), s.list...)
}

// Get is one agent.
func (s *Store) Get(id string) (Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.list {
		if a.ID == id {
			return a, nil
		}
	}
	return Agent{}, ErrNotFound
}

// Create saves a new agent. Fields it refuses come back as *Invalid.
func (s *Store) Create(f Fields) (Agent, error) {
	if why := Check(&f); why != "" {
		return Agent{}, &Invalid{why}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) >= MaxAgents {
		return Agent{}, ErrFull
	}
	if why := s.nameTaken(f.Name, ""); why != "" {
		return Agent{}, &Invalid{why}
	}
	now := s.now().UTC()
	a := Agent{ID: "a_" + randomHex(8), Created: now, Updated: now}
	a.set(f)
	if err := s.put(a, []string{a.ID}); err != nil {
		return Agent{}, err
	}
	s.list = append(append([]Agent(nil), s.list...), a)
	return a, nil
}

// Update replaces an agent's fields. Its id and Created stay. A session that
// already started keeps what its turns recorded.
func (s *Store) Update(id string, f Fields) (Agent, error) {
	if why := Check(&f); why != "" {
		return Agent{}, &Invalid{why}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return Agent{}, ErrNotFound
	}
	if why := s.nameTaken(f.Name, id); why != "" {
		return Agent{}, &Invalid{why}
	}
	list := append([]Agent(nil), s.list...)
	list[i].set(f)
	list[i].Updated = s.now().UTC()
	if err := s.put(list[i], changedFields(s.list[i], list[i])); err != nil {
		return Agent{}, err
	}
	s.list = list
	return list[i], nil
}

// Remove removes an agent. Sessions it started keep their record and can
// still be continued: each turn recorded its harness and access.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return ErrNotFound
	}
	if err := s.b.SetAgent(settings.AgentRow{ID: id, Removed: true}, []string{id}); err != nil {
		return fmt.Errorf("agents: the agent could not be removed: %w", err)
	}
	s.list = append(append([]Agent(nil), s.list[:i]...), s.list[i+1:]...)
	return nil
}

// Resolve is the agent a turn names: a saved agent by id, or, for a harness
// name no saved agent has as its id (one the person removed), that harness
// on its own subscription with its default model. Programs written before
// saved agents send the harness name.
func (s *Store) Resolve(id string) (Agent, error) {
	a, err := s.Get(id)
	if err == nil {
		return a, nil
	}
	if known(id) {
		return Agent{Harness: id, Access: Subscription}, nil
	}
	return Agent{}, err
}

// Lookup is the agent a request names, found by resolve. When resolve finds
// none, a retry gets a stand-in, with id as both its id and its harness: the
// queue matches the idempotency key against the request as first asked and
// answers with the first job. retried says the request's key was used
// before; nil means the request is no retry. why is resolve's error when the
// request is no retry; err is retried's.
func Lookup(id string, resolve func(string) (Agent, error), retried func() (bool, error)) (ag Agent, why, err error) {
	ag, why = resolve(id)
	if why == nil {
		return ag, nil, nil
	}
	if retried == nil {
		return Agent{}, why, nil
	}
	known, err := retried()
	if err != nil {
		return Agent{}, nil, err
	}
	if !known {
		return Agent{}, why, nil
	}
	return Agent{ID: id, Harness: id}, nil, nil
}

func (a *Agent) set(f Fields) {
	a.Name, a.Harness, a.Access, a.Model, a.KeyEnv = f.Name, f.Harness, f.Access, f.Model, f.KeyEnv
}

func (s *Store) index(id string) int {
	for i, a := range s.list {
		if a.ID == id {
			return i
		}
	}
	return -1
}

func (s *Store) nameTaken(name, except string) string {
	for _, a := range s.list {
		if a.ID != except && strings.EqualFold(a.Name, name) {
			return "an agent named " + strconv.Quote(a.Name) + " already exists"
		}
	}
	return ""
}

// put stores a, naming keys as what changed.
func (s *Store) put(a Agent, keys []string) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err := s.b.SetAgent(settings.AgentRow{ID: a.ID, Data: b}, keys); err != nil {
		return fmt.Errorf("agents: the agent could not be saved: %w", err)
	}
	return nil
}

// changedFields names what an update changed, as id.field, such as
// a_1234.model. Never a value.
func changedFields(old, now Agent) []string {
	var out []string
	for _, f := range []struct {
		name string
		a, b string
	}{{"name", old.Name, now.Name}, {"harness", old.Harness, now.Harness}, {"access", old.Access, now.Access},
		{"model", old.Model, now.Model}, {"key_env", old.KeyEnv, now.KeyEnv}} {
		if f.a != f.b {
			out = append(out, now.ID+"."+f.name)
		}
	}
	if out == nil {
		out = []string{now.ID}
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
