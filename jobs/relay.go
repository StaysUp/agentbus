package jobs

// Control's gateway for runners' model calls. A runner holds no secret: its
// agent's model calls carry the turn's token as their credential (a
// placeholder), the runner's own gateway sends them here, and the relay swaps
// the token for the real credential on the way out: an API key from
// control's environment, or the person's own subscription connected on
// control (package credentials). The vendor's reply goes back as it came.
//
// Which credential, by one rule: that of the agent the token's job names; a
// job that names none continues on the agent its session's latest turn ran
// on, as control's record holds it (what the runner's engine reads too).
// Either way a saved agent's access and key variable come from control's
// saved agents, never from a runner: a runner can at most pick among the
// person's own agents, and never names a variable.
//
// The relay records every call it carries, in control's record, with the
// session and turn of the job the token names: the request, then the reply
// as it streams (model.requested, model.responded, tool events, usage and
// cost), through a gateway of control's own for the vendor (Upstream.Gateways)
// that does what a gateway on the agent's machine does. One rule: control
// records every call it carries, and a runner that holds no secret sends
// none of its calls (runner/record.go). A call the relay refuses, or whose
// vendor it cannot reach, is recorded too, with why. A call that never
// reached control reached no vendor: it stays in the runner's own record. A
// call without a running turn's token is refused and recorded nowhere: it
// names no session.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/queue"
)

// RelayPrefix is the path the relay answers under: /relay/<name>/<the
// vendor's path>.
const RelayPrefix = "/relay/"

// Upstream is one vendor control relays runners' model calls to, and the
// access whose calls go there. A subscription goes only to its own vendor:
// Harness names the one harness whose subscription a call there gets.
type Upstream struct {
	URL     *url.URL
	Access  string // subscription or openrouter
	Harness string // for a subscription: the harness whose vendor this is
	// Under, when set, is how every vendor path relayed here starts, such
	// as "v1internal:" for agy's Code Assist methods. Any other path gets
	// no credential.
	Under string
	// Gateways record the calls relayed here, each forwarding to URL: the
	// first that reads a call's path takes it, and the first takes any
	// other (one gateway per API format of the host, as OpenRouter's two).
	// At least one.
	Gateways []*gateway.Gateway
	// Own, in agentbus up, are the same gateways recording nothing, one for
	// each of Gateways in its order: for the calls of up's own runner,
	// whose gateway records them in this same record (Server.SetOwnRunner).
	Own []*gateway.Gateway
}

// gatewayFor is the gateway of gs that takes a call of method on path.
func gatewayFor(gs []*gateway.Gateway, method, path string) *gateway.Gateway {
	for _, g := range gs {
		if g.Watches(method, path) {
			return g
		}
	}
	return gs[0]
}

// SettleWait bounds how long control waits, before it stores a turn's
// session.closed, for the relay to finish recording the turn's calls: their
// events are written off the reply's path, and can land after the runner
// sent the end. Under the runner's own wait for the answer.
const SettleWait = 5 * time.Second

// settle waits until every call of (session, turn) the relay carried is
// recorded, at most SettleWait; false when it could not confirm that.
func (s *Server) settle(session, turn string) bool {
	ok := true
	for _, up := range s.cfg.Relay {
		for _, g := range append(up.Gateways, up.Own...) {
			ok = g.Settle(session, turn, SettleWait) && ok
		}
	}
	return ok
}

// exchangeID is the shape of a gateway's exchange id.
var exchangeID = regexp.MustCompile(`^x-[0-9a-f]{8}-[0-9]{1,20}$`)

// exchangeOf is the runner's name for the call r carries
// (gateway.ExchangeHeader), when control may record the call under it: the
// shape of a gateway's id, none of control's own gateways', and no call of
// another session or of another turn of this one. Otherwise empty, and
// control's gateway names the call.
func (s *Server) exchangeOf(r *http.Request, session, turn string) string {
	x := r.Header.Get(gateway.ExchangeHeader)
	if !exchangeID.MatchString(x) {
		return ""
	}
	for _, up := range s.cfg.Relay {
		for _, g := range up.Gateways {
			if g.Names(x) {
				return ""
			}
		}
	}
	if s.cfg.Record != nil {
		if sess, found, err := s.cfg.Record.CallSession(x); err != nil || (found && sess != session) {
			return ""
		}
		if other, err := s.cfg.Record.OtherTurns(x, session, turn); err != nil || other {
			return ""
		}
	}
	return x
}

// Relay answers RelayPrefix. It is nil without Config.Relay.
func (s *Server) Relay() http.Handler {
	if len(s.cfg.Relay) == 0 {
		return nil
	}
	return http.HandlerFunc(s.relay)
}

func (s *Server) relay(w http.ResponseWriter, r *http.Request) {
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, RelayPrefix), "/")
	up, ok := s.cfg.Relay[name]
	if !ok || !strings.HasPrefix(rest, up.Under) {
		fail(w, http.StatusNotFound, "control relays no model calls on this path")
		return
	}
	tok, _ := bearer(r)
	j, _, ok := s.turnJob(tok)
	if !ok {
		fail(w, http.StatusUnauthorized, "the call carries no turn token of a turn running now; control adds credentials only to a running turn's calls")
		return
	}
	session := assignedSession(j)
	rl := gateway.Relayed{Session: session, Turn: j.Turn, Path: "/" + rest}
	gs := up.Gateways
	if len(up.Own) > 0 && s.own(tokenRunner(tok)) {
		gs = up.Own
	}
	g := gatewayFor(gs, r.Method, rl.Path)
	// Read once the call holds its turn open: the record and a renewal can
	// take a while.
	rl.Prepare = func(rl *gateway.Relayed) {
		rl.Exchange = s.exchangeOf(r, session, j.Turn)
		cred, status, why := s.credential(j, up)
		if why != "" {
			b, _ := json.Marshal(JobError{Error: why})
			rl.Refuse = &gateway.Refusal{Status: status, ContentType: "application/json", Body: b}
			return
		}
		rl.Header = func(h http.Header) {
			h.Del("X-Api-Key")
			h.Set("Authorization", "Bearer "+cred.Token)
			for k, v := range cred.Headers {
				h.Set(k, v)
			}
		}
	}
	g.ServeRelayed(w, r, rl)
}

// credential is what control adds to the calls of j's turn on up, or the
// status and words of the refusal.
func (s *Server) credential(j queue.Job, up Upstream) (credentials.Cred, int, string) {
	none := credentials.Cred{}
	sp, why := s.agentOf(j)
	if why != "" {
		return none, http.StatusForbidden, why
	}
	access := sp.Access
	if access == "" {
		access = credentials.AccessSubscription
	}
	if access != up.Access {
		return none, http.StatusForbidden, "this turn's agent reaches its model by " + access + ", so control adds no credential to a call on this path"
	}
	if access == credentials.AccessSubscription {
		if sp.Harness != up.Harness {
			return none, http.StatusForbidden, "this turn's agent runs " + sp.Harness + ", so control adds its subscription to no call on this path"
		}
	}
	if s.cfg.Credentials == nil {
		return none, http.StatusForbidden, "control keeps no credentials for runners"
	}
	cred, err := s.cfg.Credentials.Credential(credentials.Need{Access: access, Harness: sp.Harness, KeyEnv: sp.KeyEnv})
	var missing *credentials.Missing
	switch {
	case errors.As(err, &missing):
		return none, http.StatusForbidden, missing.Why
	case err != nil:
		if s.Logf != nil {
			s.Logf("relay.credential_failed session=%s turn=%s access=%s", assignedSession(j), j.Turn, access)
		}
		return none, http.StatusInternalServerError, "control could not read its credentials"
	}
	return cred, 0, ""
}

// savedAgent is control's saved agent id, or an error when control keeps
// none or the agent is gone.
func (s *Server) savedAgent(id string) (agents.Agent, error) {
	if s.cfg.Agents == nil {
		return agents.Agent{}, errors.New("jobs: no saved agents")
	}
	return s.cfg.Agents.Get(id)
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
			ag, err := s.savedAgent(sp.AgentID)
			switch {
			case err == nil:
				sp.Harness, sp.Access, sp.KeyEnv = ag.Harness, ag.Access, ag.KeyEnv
			case c.Access == "" || c.Access == credentials.AccessSubscription:
				// The agent was removed since: the session goes on, on its
				// harness's subscription, as the record holds it. A
				// subscription goes only to its own vendor (credential).
				sp.Access = credentials.AccessSubscription
			default:
				// A key's variable comes from control's saved agents only,
				// never from the record, which a runner writes.
				return sp, "this session's agent was removed, and only a saved agent names the variable of its key; save the agent again, or start a new conversation"
			}
		}
	}
	return sp, ""
}
