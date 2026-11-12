package jobs

// A machine signs in (agentbus login), the person decides, and only then does
// the machine hold a working runner token. The flow is a device login:
//
//   - POST /api/v0/runner/login, with no token: the machine asks. The reply
//     holds a short code for the machine to show and the runner token it
//     will use, which works for nothing yet.
//   - The person types that code on the Runners page to approve the machine
//     (Server.ApproveLogin), or refuses it. The pending list never holds the
//     code, so approving needs the machine's own screen.
//   - GET /api/v0/runner/login, with that token: the machine asks how it
//     stands, until approved, refused or expired.
//
// Nothing of a waiting sign-in is a runner: it is not listed with the
// runners, takes no job, and reports no folder. The enrollment token
// (POST /api/v0/runner/register) still adds a runner at once, as before.

import (
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// LoginPoll is how often a machine asks how its sign-in stands.
const LoginPoll = 2 * time.Second

// LoginStarted is the answer to POST /api/v0/runner/login.
type LoginStarted struct {
	Login string `json:"login" doc:"The sign-in's id, as the Runners page lists it."`
	// Code is shown on the machine, as XXXX-XXXX; the person types it.
	Code string `json:"code" doc:"The code the machine shows, such as BCDF-GHJK. The person types it to approve the machine."`
	// Token is the runner token the machine uses once approved. Shown once.
	Token   string    `json:"token" doc:"The machine's runner token, this once. It works only for GET /api/v0/runner/login until the person approves the machine."`
	Expires time.Time `json:"expires" doc:"When the sign-in stops waiting for the person."`
	PollMS  int64     `json:"poll_ms" doc:"How often to ask GET /api/v0/runner/login."`
}

// LoginState is the answer to GET /api/v0/runner/login.
type LoginState struct {
	State string `json:"state" doc:"pending, approved, refused, expired, or removed (approved, and the runner removed since)."`
	// Runner, Heartbeat and Lease: once approved, as Enrolled has them.
	Runner    *queue.Runner `json:"runner,omitempty" doc:"The runner, once approved."`
	Heartbeat int64         `json:"heartbeat_ms,omitempty"`
	Lease     int64         `json:"lease_ms,omitempty"`
}

// codeLetters are the code's letters: consonants only, so no code spells a
// word, and none that reads like a digit.
const codeLetters = "BCDFGHJKLMNPQRSTVWXZ"

// newCode is 8 of codeLetters, each as likely, about 34 bits.
func newCode() string {
	out := make([]byte, 0, 8)
	b := make([]byte, 16)
	for len(out) < 8 {
		_, _ = rand.Read(b)
		for _, c := range b {
			// 240 is the largest multiple of 20 under 256.
			if c < 240 && len(out) < 8 {
				out = append(out, codeLetters[int(c)%len(codeLetters)])
			}
		}
	}
	return string(out)
}

// ShowCode is a code as a machine shows it: XXXX-XXXX.
func ShowCode(code string) string {
	if len(code) != 8 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// normalCode is a code as a person typed it: upper case, without a dash
// or spaces.
func normalCode(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
}

// checkName refuses a name with a control character: a sign-in's name comes
// from anyone who reaches control, and the Runners page shows it.
func checkName(name string) bool {
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (s *Server) loginStart(w http.ResponseWriter, r *http.Request) {
	// No token is read: a machine signing in has none yet.
	var b EnrollRequest
	if !decode(r, &b) {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with name, capacity, protocol and optionally labels")
		return
	}
	if msg := s.checkEnroll(b); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	if !checkName(b.Name) {
		fail(w, http.StatusBadRequest, "name must have no control characters")
		return
	}
	rt := newRunnerToken()
	code := newCode()
	l, err := s.cfg.Queue.StartLogin(queue.DefaultTeam, b.Name, b.Labels, b.Capacity, queue.HashToken(rt), code, b.Capabilities)
	if errors.Is(err, queue.ErrTooManyLogins) {
		fail(w, http.StatusTooManyRequests, "too many machines are waiting for approval: approve or refuse them on the Runners page, or wait 15 minutes")
		return
	}
	if err != nil {
		s.internal(w)
		return
	}
	if s.Logf != nil {
		s.Logf("runner.login id=%s name=%q", l.ID, l.Name)
	}
	writeJSON(w, http.StatusCreated, LoginStarted{Login: l.ID, Code: ShowCode(code), Token: rt, Expires: l.Expires, PollMS: LoginPoll.Milliseconds()})
}

func (s *Server) loginState(w http.ResponseWriter, r *http.Request) {
	tok, ok := bearer(r)
	if !ok || tok == "" {
		fail(w, http.StatusUnauthorized, "the token of a sign-in is required")
		return
	}
	l, err := s.cfg.Queue.LoginByToken(queue.HashToken(tok))
	if errors.Is(err, queue.ErrNotFound) {
		fail(w, http.StatusUnauthorized, "the token of a sign-in is required")
		return
	}
	if err != nil {
		s.internal(w)
		return
	}
	out := LoginState{State: l.State}
	if l.State == queue.LoginApproved {
		rn, err := s.cfg.Queue.RunnerByToken(queue.HashToken(tok))
		switch {
		case errors.Is(err, queue.ErrNotFound):
			out.State = "removed"
		case err != nil:
			s.internal(w)
			return
		default:
			out.Runner, out.Heartbeat, out.Lease = &rn, s.cfg.Heartbeat.Milliseconds(), s.cfg.Lease.Milliseconds()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ApproveLogin approves a waiting sign-in, when code is the one its machine
// shows (typed with or without its dash): its token becomes a runner's.
// Errors are the queue's: ErrNotFound, ErrBadState (decided or expired),
// ErrWrongCode.
func (s *Server) ApproveLogin(id, code string) (queue.Runner, error) {
	rn, err := s.cfg.Queue.ApproveLogin(queue.DefaultTeam, id, normalCode(code))
	if err != nil {
		return rn, err
	}
	s.enrolled(rn)
	return rn, nil
}

// RefuseLogin refuses a waiting sign-in.
func (s *Server) RefuseLogin(id string) error {
	err := s.cfg.Queue.RefuseLogin(queue.DefaultTeam, id)
	if err == nil && s.Logf != nil {
		s.Logf("runner.login_refused id=%s", id)
	}
	return err
}

// RemoveRunner removes an offline runner: its token stops working and it is
// no longer listed; its sessions, jobs and their events stay. Errors are the
// queue's: ErrNotFound, ErrRunnerOnline.
func (s *Server) RemoveRunner(id string) error {
	if err := s.cfg.Queue.RemoveRunner(queue.DefaultTeam, id); err != nil {
		return err
	}
	if s.Logf != nil {
		s.Logf("runner.removed id=%s", id)
	}
	if s.cfg.Record != nil {
		if _, err := s.cfg.Record.AppendOnce(event.Event{ID: queue.NewID("evt_"), Kind: event.RunnerRemoved, Time: time.Now().UTC(), Runner: id}); err != nil && s.Logf != nil {
			s.Logf("store.error what=%s", event.RunnerRemoved)
		}
	}
	return nil
}
