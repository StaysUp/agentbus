// Package credentials is where control's gateway gets the credential it adds
// to a runner's model call. Runners hold no secrets: an agent on a runner
// gets a placeholder (its turn's token), and control swaps the real
// credential in on the way out (jobs/relay.go).
//
// Where a credential comes from is a plug (Source), one per kind of access:
// an API key read from control's environment by its variable's name, or the
// person's own subscription, connected once with the vendor's own login and
// kept in control's data folder (Subscriptions). A credential's value is
// never logged, printed, put in an event or in an error.
//
// It imports nothing of the module, so control and its command may use it.
package credentials

import (
	"errors"
	"strings"
)

// Access kinds, as harness names them. Kept as strings here so this package
// imports nothing.
const (
	AccessSubscription = "subscription"
	AccessOpenRouter   = "openrouter"
)

// Need is what one model call needs a credential for: the access its agent
// uses, its harness, and for an API key the name of the variable that holds
// it.
type Need struct {
	Access  string
	Harness string
	KeyEnv  string
}

// Cred is what control adds to one model call: the bearer token, and the
// other headers the sign-in needs, such as the ChatGPT account of codex's
// sign-in.
type Cred struct {
	Token   string
	Headers map[string]string
}

// Source gives the credential for a need. A credential that cannot be given
// is a *Missing, whose text says what to do and never quotes a value.
type Source interface {
	Credential(n Need) (Cred, error)
}

// Missing says why no credential could be given, in fixed wording.
type Missing struct{ Why string }

func (m *Missing) Error() string { return m.Why }

// Plug is the sources by access kind: the one place that says where each
// kind's credential comes from.
type Plug map[string]Source

// Credential asks the source of n's access.
func (p Plug) Credential(n Need) (Cred, error) {
	s, ok := p[n.Access]
	if !ok {
		return Cred{}, &Missing{Why: "control adds no credential for access " + n.Access}
	}
	return s.Credential(n)
}

// Readier is a Source that can say, without fetching or renewing anything,
// why a need's credential cannot be given now.
type Readier interface {
	Ready(n Need) string
}

// Ready says why n's credential cannot be given now, or "": for a turn
// that is about to start, so it is refused in plain words before its agent
// sends a call. A source that cannot tell says "".
func (p Plug) Ready(n Need) string {
	s, ok := p[n.Access]
	if !ok {
		return "control adds no credential for access " + n.Access
	}
	if r, ok := s.(Readier); ok {
		return r.Ready(n)
	}
	return ""
}

// Ready: as Credential, which reads only the environment.
func (e Env) Ready(n Need) string {
	if _, err := e.Credential(n); err != nil {
		return err.Error()
	}
	return ""
}

// Env reads an API key from control's environment, by the name the agent
// gives (its key_env).
type Env struct {
	Getenv func(string) string
}

// Credential reads the variable n.KeyEnv.
func (e Env) Credential(n Need) (Cred, error) {
	if !EnvNameOK(n.KeyEnv) {
		return Cred{}, &Missing{Why: "the agent names no usable variable for its key"}
	}
	v := e.Getenv(n.KeyEnv)
	if v == "" {
		return Cred{}, &Missing{Why: "the agent's key is read from " + n.KeyEnv + ", which is not set where control runs; set it and restart agentbus"}
	}
	return Cred{Token: v}, nil
}

// EnvNameOK says name is an environment variable's name: A-Z, 0-9 and _,
// not starting with a digit.
func EnvNameOK(name string) bool {
	if name == "" || len(name) > 128 || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	return strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

// ErrNotConnected is a subscription no one connected yet.
var ErrNotConnected = errors.New("credentials: not connected")
