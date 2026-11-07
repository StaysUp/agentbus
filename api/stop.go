package api

// POST /api/v0/turns/{id}/stop: the person stops one running turn. Its whole
// process tree ends the way it does when the service stops (procTree): SIGTERM
// to the child's group and every process the tree counts, SIGKILL after
// RunGrace for what still runs. The turn's session.closed then carries
// stopped, and the session's next turn continues after it by own replay, with
// a note saying the person stopped it. Only the main token may stop a turn:
// the route is mainOnly, so a turn token gets 403. docs/CLIENT_API.md is the
// contract.

import (
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/staysup/agentbus/store"
)

// liveTurn is one running child: the turn it does, and its process tree.
type liveTurn struct {
	session, turn string
	tree          *procTree
	done          chan struct{} // closed once the child's own process exited

	mu      sync.Mutex
	exited  bool      // the child's process exited: a stop comes too late
	stopped bool      // a stop came while it ran
	killAt  time.Time // with stopped: when what still runs of the tree gets SIGKILL
}

// stop marks the turn stopped, with what still runs of its tree to get
// SIGKILL at killAt. running is false once the child exited; first is true for
// the one stop that marked it, so only that one signals.
func (lt *liveTurn) stop(killAt time.Time) (running, first bool) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if lt.exited {
		return false, false
	}
	first = !lt.stopped
	if first {
		lt.stopped, lt.killAt = true, killAt
	}
	return true, first
}

// exit marks the child exited and says whether a stop came before, and the
// stop's SIGKILL deadline.
func (lt *liveTurn) exit() (stopped bool, killAt time.Time) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if !lt.exited {
		lt.exited = true
		close(lt.done)
	}
	return lt.stopped, lt.killAt
}

// find is the running child of turn, or nil.
func (rs *runs) find(turn string) *liveTurn {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.byTurn[turn]
}

// Stopping is the answer to a turn's stop.
type Stopping struct {
	ID   string `json:"id" doc:"The turn's session."`
	Turn string `json:"turn" doc:"The turn being stopped. Its session.closed, with stopped true, comes once its process tree has ended."`
}

const (
	turnNotRunning = "this turn is not running: it has ended, or this service did not start it"
	noTurn         = "no such turn"
)

func (a *API) stopTurn(w http.ResponseWriter, _ *http.Request, id string) {
	if a.cfg.Wire == nil {
		writeError(w, http.StatusNotFound, "no such API path")
		return
	}
	if lt := a.runs.find(id); lt != nil {
		// One deadline for the whole tree: what the child leaves running
		// when it exits gets SIGKILL at the same moment the child would have
		// (startTurn's tree.end), not a second grace period after it.
		killAt := time.Now().Add(a.cfg.RunGrace) // real time, not the store's clock
		running, first := lt.stop(killAt)
		if running {
			if first {
				a.cfg.Logf("run.stop session=%s turn=%s", lt.session, lt.turn)
				lt.tree.signal(syscall.SIGTERM)
				go func() {
					select {
					case <-lt.done:
						// What is left of the tree ends after the child, by
						// the same deadline (startTurn's tree.end).
					case <-time.After(time.Until(killAt)):
						lt.tree.signal(syscall.SIGKILL)
					}
				}()
			}
			writeJSON(w, http.StatusAccepted, Stopping{lt.session, lt.turn})
			return
		}
	}
	recs, err := a.cfg.Store.After(0, store.Filter{Turn: id}, 1)
	if err != nil {
		a.internal(w)
		return
	}
	if len(recs) == 0 {
		writeError(w, http.StatusNotFound, noTurn)
		return
	}
	writeError(w, http.StatusConflict, turnNotRunning)
}
