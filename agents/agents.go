// Package agents keeps the saved agents: a harness, how it reaches its model,
// the model and a name. An agent says nothing about where it runs. They are
// kept in one JSON file in the data folder, agents.json, written whole by an
// atomic rename. The first open of a data folder without the file saves two
// agents, Claude Code and Codex, on their own subscriptions, so a turn named
// by harness keeps working.
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
	"os"
	"path/filepath"
	"regexp"
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

// Store keeps the agents of one data folder. Its methods are safe to call at
// once from many goroutines of one process.
type Store struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
	list []Agent
}

type file struct {
	Agents []Agent `json:"agents"`
}

// Defaults are the agents a new data folder starts with: one per harness, on
// its own subscription. Their ids are the harness names, so a turn that names
// claude or codex finds them.
func Defaults(now time.Time) []Agent {
	var out []Agent
	for _, h := range harness.All() {
		i := h.Info()
		out = append(out, Agent{ID: i.Name, Name: i.Label, Harness: i.Name, Access: Subscription, Created: now, Updated: now})
	}
	return out
}

// known says an agent can be saved on name.
func known(name string) bool {
	_, ok := harness.Get(name)
	return ok
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

// Open reads dir's agents file, or saves the defaults when there is none. now
// nil means time.Now.
func Open(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{path: filepath.Join(dir, FileName), now: now}
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.list = Defaults(now().UTC())
		if err := s.save(s.list); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("agents: %s could not be read: %w", s.path, err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("agents: %s is not valid: %w", s.path, err)
	}
	for _, a := range f.Agents {
		if !idShape.MatchString(a.ID) {
			return nil, fmt.Errorf("agents: %s has an agent with the id %s, which is not a valid id", s.path, strconv.Quote(a.ID))
		}
	}
	s.list = f.Agents
	return s, nil
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
	list := append(append([]Agent(nil), s.list...), a)
	if err := s.save(list); err != nil {
		return Agent{}, err
	}
	s.list = list
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
	if err := s.save(list); err != nil {
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
	list := append(append([]Agent(nil), s.list[:i]...), s.list[i+1:]...)
	if err := s.save(list); err != nil {
		return err
	}
	s.list = list
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

// save writes list to the file through a temporary file and a rename, so a
// reader never sees half of it.
func (s *Store) save(list []Agent) error {
	if list == nil {
		list = []Agent{}
	}
	data, err := json.MarshalIndent(file{list}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".agents-*.json")
	if err != nil {
		return fmt.Errorf("agents: the file could not be written: %w", err)
	}
	_, werr := tmp.Write(append(data, '\n'))
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("agents: the file could not be written: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("agents: the file could not be written: %w", err)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
