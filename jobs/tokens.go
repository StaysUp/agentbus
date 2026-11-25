package jobs

// Turn tokens for runners' turns, minted by control. A runner gets one with
// each turn or review it starts (AttemptStarted.Token), and its agent uses it
// as AGENTBUS_TOKEN, to call control's API (read its request tree, ask other
// agents). The runner never names what the token is for: it names the
// attempt control assigned, and control reads the job's session, request
// tree and access from its own queue.
//
// A token is the runner, the attempt and its number, signed with a key
// derived from the main token, so control keeps none of them: it works while
// that attempt is current (taken, not ended, its lease not run out), and stops
// with it, across a restart of control too.
//
// The agent's model calls carry a stand-in instead (AttemptStarted.StandIn),
// never the turn token: the stand-in is what sits in the agent's sign-in, its
// key variable or its sign-in file, and the agent sends it on calls that never
// reach AgentBus too. Claude Code 2.1.321 sends its sign-in to api.anthropic.com
// (feature flags, bootstrap, event logging) and agy 1.5.9 to
// www.googleapis.com/oauth2/v2/userinfo and play.googleapis.com (measured
// 2026-11-15). A stand-in is good for one thing only: control's relay swaps it
// for the real credential on the turn's own model calls (relay.go). It is
// refused on control's API, and the turn token is refused on the relay.
//
// A stand-in is the same runner, attempt and number, sealed with another key
// derived from the main token, behind a random nonce: it reads as random
// bytes, names nothing to whoever gets it, and lives exactly as long as the
// turn token: while the attempt is current, across a restart of control.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
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
	return s.attemptJob(rest[:i])
}

// attemptJob is the job of the attempt msg names (runner.attempt.number)
// while it is current, and what its turn's token may do.
func (s *Server) attemptJob(msg string) (queue.Job, run.Grant, bool) {
	parts := strings.Split(msg, ".")
	if len(parts) != 3 {
		return queue.Job{}, run.Grant{}, false
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil {
		return queue.Job{}, run.Grant{}, false
	}
	j, a, err := s.cfg.Queue.CurrentAttempt(parts[0], parts[1], number)
	if err != nil {
		return queue.Job{}, run.Grant{}, false
	}
	session := assignedSession(j)
	if session == "" {
		return queue.Job{}, run.Grant{}, false
	}
	// Everything the grant says of the turn is read from its job and attempt,
	// the one copy: its place in the request tree, and the rules it took when
	// it started (turnRules).
	g := run.Grant{Session: session, Turn: j.Turn, Root: session, Harness: j.Spec.Harness, Rules: settings.Version(a.Rules)}
	if a.Started != nil {
		g.RulesAt = *a.Started
	}
	if c := j.Links.Chain; c != nil {
		g.Root, g.Depth = c.Root, c.Depth
	}
	return j, g, true
}

// StandInPrefix marks a turn's stand-in credential.
const StandInPrefix = "abs_"

// standInSealer seals stand-ins with a key derived from the main token, never
// the key that signs turn tokens.
func standInSealer(mainToken string) cipher.AEAD {
	key := sha256.Sum256([]byte("agentbus runner stand-ins\x00" + mainToken))
	b, err := aes.NewCipher(key[:])
	if err != nil {
		panic("jobs: AES with a 32-byte key: " + err.Error())
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		panic("jobs: GCM: " + err.Error())
	}
	return a
}

// standIn is the stand-in credential of attempt number of attemptID, taken
// by runnerID: a fresh one each call, every one good while the attempt is
// current. Empty when no random nonce could be read.
func (s *Server) standIn(runnerID, attemptID string, number int) string {
	nonce := make([]byte, s.sealer.NonceSize(), s.sealer.NonceSize()+64)
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	msg := runnerID + "." + attemptID + "." + strconv.Itoa(number)
	return StandInPrefix + hex.EncodeToString(s.sealer.Seal(nonce, nonce, []byte(msg), []byte(StandInPrefix)))
}

// standInJob is the job of a stand-in's attempt while it is current. A turn
// token, an ended attempt's stand-in, or one control did not seal is not one.
func (s *Server) standInJob(tok string) (queue.Job, bool) {
	rest, ok := strings.CutPrefix(tok, StandInPrefix)
	if !ok {
		return queue.Job{}, false
	}
	b, err := hex.DecodeString(rest)
	if err != nil || len(b) < s.sealer.NonceSize() {
		return queue.Job{}, false
	}
	n := s.sealer.NonceSize()
	msg, err := s.sealer.Open(nil, b[:n], b[n:], []byte(StandInPrefix))
	if err != nil {
		return queue.Job{}, false
	}
	j, _, ok := s.attemptJob(string(msg))
	return j, ok
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
// turn's own. Any other job (apply, folder) has none. B15: a review is a normal
// turn, so it has no separate review session.
func assignedSession(j queue.Job) string {
	if j.Kind == queue.KindTurn {
		return j.Session
	}
	return ""
}
