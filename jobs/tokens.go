package jobs

// Turn tokens for runners' turns, minted by control. A runner gets one with
// each turn or review it starts (AttemptStarted.Token), and its agent uses it
// twice: as AGENTBUS_TOKEN, to call control's API (read its request tree, ask
// other agents), and as the placeholder credential of its model calls, which
// control's gateway swaps for the real one (relay.go). The runner never names
// what the token is for: it names the attempt control assigned, and control
// reads the job's session, request tree and access from its own queue.
//
// A token is the runner, the attempt and its number, signed with a key
// derived from the main token, so control keeps none of them: it works while
// that attempt is current (taken, not ended, its lease not run out), and stops
// with it, across a restart of control too.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// TokenPrefix marks a turn token control minted.
const TokenPrefix = "abr_"

func (s *Server) sign(msg string) string {
	m := hmac.New(sha256.New, s.tokenKey[:])
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// mint is the token of attempt number of attemptID, taken by runnerID.
func (s *Server) mint(runnerID, attemptID string, number int) string {
	msg := runnerID + "." + attemptID + "." + strconv.Itoa(number)
	return TokenPrefix + msg + "." + s.sign(msg)
}

// turnJob is the job of a token's attempt while it is current, and what the
// token may do as a turn token.
func (s *Server) turnJob(tok string) (queue.Job, run.Grant, bool) {
	rest, ok := strings.CutPrefix(tok, TokenPrefix)
	if !ok {
		return queue.Job{}, run.Grant{}, false
	}
	i := strings.LastIndexByte(rest, '.')
	if i < 0 || !hmac.Equal([]byte(rest[i+1:]), []byte(s.sign(rest[:i]))) {
		return queue.Job{}, run.Grant{}, false
	}
	parts := strings.Split(rest[:i], ".")
	if len(parts) != 3 {
		return queue.Job{}, run.Grant{}, false
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil {
		return queue.Job{}, run.Grant{}, false
	}
	j, err := s.cfg.Queue.Current(parts[0], parts[1], number)
	if err != nil {
		return queue.Job{}, run.Grant{}, false
	}
	session := assignedSession(j)
	if session == "" {
		return queue.Job{}, run.Grant{}, false
	}
	g := run.Grant{Session: session, Turn: j.Turn, Root: session, ReadOnly: j.Spec.ReadOnly || j.Kind == queue.KindReview,
		Harness: j.Spec.Harness}
	if c := j.Links.Chain; c != nil {
		g.Root, g.Depth = c.Root, c.Depth
	}
	return j, g, true
}

// TurnTokenEnded says tok is a turn token control minted whose attempt is
// no longer current: its turn ended, or another attempt replaced it.
func (s *Server) TurnTokenEnded(tok string) bool {
	rest, ok := strings.CutPrefix(tok, TokenPrefix)
	if !ok {
		return false
	}
	i := strings.LastIndexByte(rest, '.')
	if i < 0 || !hmac.Equal([]byte(rest[i+1:]), []byte(s.sign(rest[:i]))) || len(strings.Split(rest[:i], ".")) != 3 {
		return false
	}
	_, _, live := s.turnJob(tok)
	return !live
}

// TurnGrant says what a runner turn's token may do on the API, while its
// attempt is current. ok is false for any other token.
func (s *Server) TurnGrant(tok string) (run.Grant, bool) {
	_, g, ok := s.turnJob(tok)
	return g, ok
}

// assignedSession is the session control assigned a job when it made it: a
// turn's own, or the one it named for a review. Any other job has none.
func assignedSession(j queue.Job) string {
	switch j.Kind {
	case queue.KindTurn:
		return j.Session
	case queue.KindReview:
		return j.Links.ReviewSession
	}
	return ""
}
