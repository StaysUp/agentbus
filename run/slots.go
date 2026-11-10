package run

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/netjail"
)

// runs tracks the children the engine started.
type runs struct {
	mu       sync.Mutex
	stopping bool
	max      int             // Config.MaxRuns
	n        int             // slots taken, starting or running
	busy     map[string]bool // sessions with a turn starting or running
	live     map[*exec.Cmd]*liveTurn
	byTurn   map[string]*liveTurn // the same runs, by turn id, for Stop
	wg       sync.WaitGroup
}

// take reserves a slot for session, or says why not.
func (rs *runs) take(session string) (int, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.stopping {
		return http.StatusServiceUnavailable, "the service is stopping"
	}
	if rs.busy[session] {
		return http.StatusConflict, TurnRunning
	}
	if rs.n >= rs.max {
		return http.StatusTooManyRequests, strconv.Itoa(rs.max) + " runs are already running; wait for one to end and retry"
	}
	if rs.busy == nil {
		rs.busy = map[string]bool{}
	}
	rs.busy[session] = true
	rs.n++
	return 0, ""
}

// start starts cmd, the child of turn in session, with startCmd, in a slot
// already taken, unless the engine began stopping, and starts tracking its
// process tree.
func (rs *runs) start(cmd *exec.Cmd, startCmd func() error, session, turn string) (int, string, *liveTurn) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.stopping {
		return http.StatusServiceUnavailable, "the service is stopping", nil
	}
	if err := startCmd(); err != nil {
		var ne *netjail.Error
		if errors.As(err, &ne) {
			return http.StatusInternalServerError, ne.Error(), nil
		}
		return http.StatusInternalServerError, "the agent could not be started", nil
	}
	if rs.live == nil {
		rs.live = map[*exec.Cmd]*liveTurn{}
		rs.byTurn = map[string]*liveTurn{}
	}
	lt := &liveTurn{session: session, turn: turn, tree: newProcTree(cmd.Process.Pid), done: make(chan struct{})}
	rs.live[cmd] = lt
	rs.byTurn[turn] = lt
	rs.wg.Add(1)
	return 0, "", lt
}

func (rs *runs) isStopping() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.stopping
}

// release frees session's slot; cmd is the child that held it, or nil if
// none started.
func (rs *runs) release(cmd *exec.Cmd, session string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.n--
	delete(rs.busy, session)
	if cmd != nil {
		if lt := rs.live[cmd]; lt != nil {
			delete(rs.byTurn, lt.turn)
		}
		delete(rs.live, cmd)
		rs.wg.Done()
	}
}

// signal sends sig to every running child's whole tree: its group and every
// process procTree counts in it.
func (rs *runs) signal(sig syscall.Signal) {
	rs.mu.Lock()
	trees := make([]*procTree, 0, len(rs.live))
	for _, lt := range rs.live {
		trees = append(trees, lt.tree)
	}
	rs.mu.Unlock()
	for _, t := range trees {
		t.signal(sig)
	}
}

func (rs *runs) find(turn string) *liveTurn {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.byTurn[turn]
}

// liveTurn is a running turn's child, for a stop.
type liveTurn struct {
	session, turn string
	tree          *procTree
	done          chan struct{} // closed once the child's own process exited

	mu      sync.Mutex
	exited  bool      // the child's process exited: a stop comes too late
	stopped bool      // a stop came while it ran
	killAt  time.Time // with stopped: when what still runs of the tree gets SIGKILL
}

// stop marks the turn stopped, unless its child has exited. first is true for
// the first stop, which sends the signals.
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

// exit marks the child's process exited, and says whether a stop came first.
func (lt *liveTurn) exit() (stopped bool, killAt time.Time) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if !lt.exited {
		lt.exited = true
		close(lt.done)
	}
	return lt.stopped, lt.killAt
}

// Grant is what a turn's own token is: its session and turn, the request tree
// it is in (Root, the session the person's turn ran in, and Depth, how many
// agents asked in a row before it), and whether the turn is read-only. The
// API decides from it what the token may do (api/tokens.go).
type Grant struct {
	Session, Turn string
	Root          string
	Depth         int
	ReadOnly      bool
}

// turnTokens are the live turns' own tokens. A turn's token reads only its
// own session, and works from the turn's start until its child exits. Kept as
// SHA-256, never as the token.
type turnTokens struct {
	mu sync.Mutex
	m  map[[32]byte]Grant
}

// turnTokenPrefix marks a turn's own token.
const turnTokenPrefix = "abt_"

func (t *turnTokens) mint(g Grant) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := turnTokenPrefix + hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[[32]byte]Grant{}
	}
	t.m[sha256.Sum256([]byte(tok))] = g
	return tok, nil
}

func (t *turnTokens) revoke(tok string) {
	if tok == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, sha256.Sum256([]byte(tok)))
}

func (t *turnTokens) lookup(tok string) (Grant, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	g, ok := t.m[sha256.Sum256([]byte(tok))]
	return g, ok
}

func (t *turnTokens) live() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

// TokenGrant says what a live turn's token may read. ok is false for any
// other token.
func (e *Engine) TokenGrant(tok string) (g Grant, ok bool) { return e.turnTokens.lookup(tok) }

// LiveTokens is how many turn tokens work now.
func (e *Engine) LiveTokens() int { return e.turnTokens.live() }

// turnEnv is the child's environment: env, or the parent's when env is nil,
// with the service's address and the turn's own token set, and any earlier
// value of either removed. apiDir, when set, goes first on PATH
// (Config.APIDir). asked, for work another turn asked for, is the asking
// session and turn (AGENTBUS_ASKED_BY_SESSION, AGENTBUS_ASKED_BY_TURN); an
// earlier value of either is removed whether it is set or not.
func turnEnv(env, parent []string, base, tok, apiDir string, asked *event.Origin) []string {
	if env == nil {
		env = parent
	}
	set := []string{"AGENTBUS_URL=" + base, "AGENTBUS_TOKEN=" + tok}
	if asked != nil {
		set = append(set, "AGENTBUS_ASKED_BY_SESSION="+asked.Session, "AGENTBUS_ASKED_BY_TURN="+asked.Turn)
	}
	out := make([]string, 0, len(env)+len(set)+1)
	path := false
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "AGENTBUS_URL" || k == "AGENTBUS_TOKEN" || k == "AGENTBUS_ASKED_BY_SESSION" || k == "AGENTBUS_ASKED_BY_TURN":
			continue
		case k == "PATH" && apiDir != "":
			path = true
			kv = "PATH=" + apiDir + string(os.PathListSeparator) + v
		}
		out = append(out, kv)
	}
	if apiDir != "" && !path {
		out = append(out, "PATH="+apiDir)
	}
	return append(out, set...)
}

type turnKey struct{ session, turn string }

// turnRules are the live turns' forbid entries from permissions.toml, for the
// gateway's guard.
type turnRules struct {
	mu sync.Mutex
	m  map[turnKey][]string
}

func (t *turnRules) set(session, turn string, forbid []string) {
	if len(forbid) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[turnKey][]string{}
	}
	t.m[turnKey{session, turn}] = append([]string(nil), forbid...)
}

func (t *turnRules) clear(session, turn string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, turnKey{session, turn})
}

// Forbid is the forbid list of a live turn, for the gateway's guard
// (gateway.Guard). A turn that is not live, or has none, gets nil.
func (e *Engine) Forbid(session, turn string) []string {
	e.turnRules.mu.Lock()
	defer e.turnRules.mu.Unlock()
	return e.turnRules.m[turnKey{session, turn}]
}
