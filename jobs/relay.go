package jobs

// Control's gateway for runners' model calls. A runner holds no secret: its
// agent's model calls carry the turn's token as their credential (a
// placeholder), the runner's own gateway records them and sends them here,
// and the relay swaps the token for the real credential on the way out: an
// API key from control's environment, or the person's own subscription
// connected on control (package credentials). The vendor's reply goes back
// as it came.
//
// Which credential, by one rule: that of the agent the token's job names; a
// job that names none continues on the agent its session's latest turn ran
// on, as control's record holds it (what the runner's engine reads too).
// Either way a saved agent's access and key variable come from control's
// saved agents, never from a runner: a runner can at most pick among the
// person's own agents, and never names a variable.
// The relay records nothing itself: the runner's record of the call reaches
// control with the turn (record.go).

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// RelayPrefix is the path the relay answers under: /relay/<name>/<the
// vendor's path>.
const RelayPrefix = "/relay/"

// Upstream is one vendor control relays runners' model calls to, and the
// access whose calls go there.
type Upstream struct {
	URL    *url.URL
	Access string // subscription or openrouter
}

// Relay answers RelayPrefix. It is nil without Config.Relay.
func (s *Server) Relay() http.Handler {
	if len(s.cfg.Relay) == 0 {
		return nil
	}
	return http.HandlerFunc(s.relay)
}

// discard is the proxy's logger: net/http's messages can quote a header.
var discard = log.New(io.Discard, "", 0)

func (s *Server) relay(w http.ResponseWriter, r *http.Request) {
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, RelayPrefix), "/")
	up, ok := s.cfg.Relay[name]
	if !ok {
		fail(w, http.StatusNotFound, "control relays no model calls on this path")
		return
	}
	tok, _ := bearer(r)
	j, _, ok := s.turnJob(tok)
	if !ok {
		fail(w, http.StatusUnauthorized, "the call carries no turn token of a turn running now; control adds credentials only to a running turn's calls")
		return
	}
	sp, why := s.agentOf(j)
	if why != "" {
		fail(w, http.StatusForbidden, why)
		return
	}
	access := sp.Access
	if access == "" {
		access = credentials.AccessSubscription
	}
	if access != up.Access {
		fail(w, http.StatusForbidden, "this turn's agent reaches its model by "+access+", so control adds no credential to a call on this path")
		return
	}
	if access == credentials.AccessSubscription {
		p := settings.DefaultPolicy()
		if s.cfg.Settings != nil {
			var err error
			if p, _, err = s.cfg.Settings.Policy(); err != nil {
				fail(w, http.StatusServiceUnavailable, "control's permissions cannot be used, so it adds no subscription; fix them")
				return
			}
		}
		if p.SubscriptionsOnRunners != settings.SubscriptionsOwn {
			fail(w, http.StatusForbidden, "subscriptions on runners are off: set subscriptions_on_runners = \"own\" in control's permissions.toml, "+
				"for runners on your own machines, or use an agent with an API key")
			return
		}
	}
	var err error
	if s.cfg.Credentials == nil {
		fail(w, http.StatusForbidden, "control keeps no credentials for runners")
		return
	}
	cred, err := s.cfg.Credentials.Credential(credentials.Need{Access: access, Harness: sp.Harness, KeyEnv: sp.KeyEnv})
	var missing *credentials.Missing
	switch {
	case errors.As(err, &missing):
		fail(w, http.StatusForbidden, missing.Why)
		return
	case err != nil:
		s.internal(w)
		return
	}
	u := up.URL
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = u.Scheme, u.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = strings.TrimSuffix(u.Path, "/")+"/"+rest, ""
			pr.Out.Host = u.Host
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Set("Authorization", "Bearer "+cred)
		},
		FlushInterval: -1, // a streamed reply goes on as it comes
		ErrorLog:      discard,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			fail(w, http.StatusBadGateway, "control could not reach the model's vendor")
		},
	}
	p.ServeHTTP(w, r)
}

// agentOf is the agent whose credential j's calls get (the rule above), or
// why there is none.
func (s *Server) agentOf(j queue.Job) (queue.Spec, string) {
	sp := j.Spec
	if sp.Harness == "" {
		if s.cfg.Record == nil {
			return sp, "control keeps no record, so it cannot tell this session's agent"
		}
		c, err := s.cfg.Record.Continuation(assignedSession(j))
		if err != nil || !c.Known || c.Harness == "" {
			return sp, "control's record does not hold this session's agent yet"
		}
		sp = queue.Spec{Harness: c.Harness, AgentID: c.AgentID}
		if sp.AgentID != "" {
			if s.cfg.Agents == nil {
				return sp, "control keeps no saved agents"
			}
			ag, err := s.cfg.Agents.Get(sp.AgentID)
			if err != nil {
				return sp, "this session's agent is not one of control's saved agents"
			}
			sp.Harness, sp.Access, sp.KeyEnv = ag.Harness, ag.Access, ag.KeyEnv
		}
	}
	return sp, ""
}
