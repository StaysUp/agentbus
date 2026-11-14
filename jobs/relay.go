package jobs

// Control's gateway for runners' model calls. A runner holds no secret: its
// agent's model calls carry the turn's token as their credential (a
// placeholder), the runner's own gateway sends them here, and the relay swaps
// the token for the real credential on the way out: an API key from
// control's environment, or the person's own subscription connected on
// control (package credentials). The vendor's reply goes back as it came.
//
// Which credential, by one rule: that of the agent the token's job names,
// as control read it from its saved agents when the turn was asked for. The
// runner's engine starts the turn with the same agent, so the two never
// disagree. A saved agent's access and key variable come from control's
// saved agents, never from a runner or the record a runner writes.
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
// names no session. A credential goes only on a call the vendor's adapter
// records (gateway/recorded.go): any other is refused and recorded as
// call.refused.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

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
	Access  string // subscription, openrouter or api
	Harness string // for a subscription or api: the harness whose vendor this is
	// Under, when set, is how every vendor path relayed here starts, such
	// as "v1internal:" for agy's Code Assist methods. Any other path gets
	// no credential.
	Under string
	// Gateways record the calls relayed here, each forwarding to URL: the
	// first whose adapter records a call takes it, and the first takes any
	// other, which it refuses with no credential (gateway/recorded.go). One
	// gateway per API format of the host, as OpenRouter's two. At least
	// one.
	Gateways []*gateway.Gateway
}

// gatewayFor is the gateway of gs that takes a call of method on path: the
// first whose adapter records it, or the first, which refuses it.
func gatewayFor(gs []*gateway.Gateway, method, path string) *gateway.Gateway {
	for _, g := range gs {
		if g.Records(method, path) {
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
		for _, g := range up.Gateways {
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
		fail(w, http.StatusNotFound, noRelay)
		return
	}
	j, _, ok := s.turnJob(callToken(r))
	if !ok {
		fail(w, http.StatusUnauthorized, noTurn)
		return
	}
	session := assignedSession(j)
	rl := gateway.Relayed{Session: session, Turn: j.Turn, Path: "/" + rest}
	g := gatewayFor(up.Gateways, r.Method, rl.Path)
	// Read once the call holds its turn open: the record and a renewal can
	// take a while. A call that goes with no credential reads none.
	rl.Prepare = func(rl *gateway.Relayed) {
		rl.Exchange = s.exchangeOf(r, session, j.Turn)
		if g.Credentialed(r.Method, rl.Path) {
			rl.Header, rl.Refuse = s.turnCredential(j, up)
		}
	}
	g.ServeRelayed(w, r, rl)
}

// callToken is the turn token a model call carries as its credential: as
// "Authorization: Bearer", or, for agy on the Gemini API, which sends its
// key only there, in X-Goog-Api-Key (harness/agy.go WireAPI).
func callToken(r *http.Request) string {
	if tok, ok := bearer(r); ok {
		return tok
	}
	return r.Header.Get("X-Goog-Api-Key")
}

// Why the relay, or a gateway of agentbus up, adds no credential to a call.
const (
	noRelay = "control relays no model calls on this path"
	noTurn  = "the call carries no turn token of a turn running now; control adds credentials only to a running turn's calls"
	// otherTurn: a gateway of agentbus up took a call on another turn's
	// path than its token's.
	otherTurn = "the call's token is another turn's than its path names; control adds a credential only to a turn's own calls"
)

// turnCredential is the credential for one call of j's turn on up: what
// sets it on the request to the vendor, or the refusal answered in the
// vendor's place. The one answer, whether the call came over the relay
// from a runner or from a gateway of agentbus up in this process (Local).
func (s *Server) turnCredential(j queue.Job, up Upstream) (func(http.Header), *gateway.Refusal) {
	cred, status, why := s.credential(j, up)
	if why != "" {
		return nil, refusal(status, why)
	}
	return func(h http.Header) {
		// The agent's placeholder is in one of these: it goes, whichever
		// header the real credential takes.
		h.Del("X-Api-Key")
		h.Del("X-Goog-Api-Key")
		h.Del("Authorization")
		if cred.Header == "" {
			h.Set("Authorization", "Bearer "+cred.Token)
		} else {
			h.Set(cred.Header, cred.Token)
		}
		for k, v := range cred.Headers {
			h.Set(k, v)
		}
	}, nil
}

func refusal(status int, why string) *gateway.Refusal {
	b, _ := json.Marshal(JobError{Error: why})
	return &gateway.Refusal{Status: status, ContentType: "application/json", Body: b}
}

// Local is the credential for the calls of the relay's upstream name made
// through a gateway in this process: agentbus up's own runner's agents.
// It is the relay's own answer (turnCredential) with no hop over HTTP, so
// the gateway records each call once, as it does every call it carries. A
// call's session and turn are its token's job's (Turn), never its path's.
func (s *Server) Local(name string) gateway.Credential {
	up, ok := s.cfg.Relay[name]
	return gateway.Credential{
		// A call's session and turn are its token's job's: a path that names
		// another, or no running turn's token, is refused before the
		// gateway records anything.
		Turn: func(r *http.Request, session, turn string) *gateway.Refusal {
			j, _, found := s.turnJob(callToken(r))
			switch {
			case !found:
				return refusal(http.StatusUnauthorized, noTurn)
			case session != assignedSession(j) || turn != j.Turn:
				return refusal(http.StatusForbidden, otherTurn)
			}
			return nil
		},
		Read: func(r *http.Request) (func(http.Header), *gateway.Refusal) {
			if !ok || !strings.HasPrefix(strings.TrimPrefix(r.URL.Path, "/"), up.Under) {
				return nil, refusal(http.StatusNotFound, noRelay)
			}
			j, _, found := s.turnJob(callToken(r))
			if !found {
				// It ended since Turn let the call through.
				return nil, refusal(http.StatusUnauthorized, noTurn)
			}
			return s.turnCredential(j, up)
		},
	}
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
	// A subscription, or a vendor's API key, goes to its own vendor only.
	if access == credentials.AccessSubscription || access == credentials.AccessAPI {
		if sp.Harness != up.Harness {
			what := "subscription"
			if access == credentials.AccessAPI {
				what = "API key"
			}
			return none, http.StatusForbidden, "this turn's agent runs " + sp.Harness + ", so control adds its " + what + " to no call on this path"
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

// agentOf is the agent whose credential j's calls get: the agent j names,
// as control read it when the turn was asked for (api, queue.Deliver), the
// one the runner's engine starts the turn with too. Every turn names its
// agent; one stored before that rule names none and gets no credential.
func (s *Server) agentOf(j queue.Job) (queue.Spec, string) {
	if j.Spec.Harness == "" {
		return j.Spec, "this turn names no agent, so control adds no credential to its calls; send it again naming its agent"
	}
	return j.Spec, ""
}
