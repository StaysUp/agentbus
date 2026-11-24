package run

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/netjail"
)

// runs tracks the children the engine started.
type runs struct {
	mu       sync.Mutex
	stopping bool
	busy     map[string]bool // sessions with a turn starting or running
	live     map[*exec.Cmd]*liveTurn
	byTurn   map[string]*liveTurn // the same runs, by turn id, for Stop
	wg       sync.WaitGroup
}

// take marks session busy, or says why not: a session runs one turn at a
// time. How many sessions run at once is control's to count (queue/lanes.go).
func (rs *runs) take(session string) (int, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.stopping {
		return http.StatusServiceUnavailable, "the service is stopping"
	}
	if rs.busy[session] {
		return http.StatusConflict, TurnRunning
	}
	if rs.busy == nil {
		rs.busy = map[string]bool{}
	}
	rs.busy[session] = true
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

// release frees session; cmd is the child that held it, or nil if
// none started.
func (rs *runs) release(cmd *exec.Cmd, session string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
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

// turnTokens are the live turns' own tokens. A turn's token reads only its
// own session, and works from the turn's start until its child exits. Kept as
// SHA-256, never as the token.
type turnTokens struct {
	mu sync.Mutex
	m  map[[32]byte]Grant
}

// TurnTokenPrefix marks a turn's own token.
const TurnTokenPrefix = "abt_"

func (t *turnTokens) mint(g Grant) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := TurnTokenPrefix + hex.EncodeToString(b)
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
		case slices.Contains(turnVars, k):
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

// turnRules are the live turns' command rules from permissions.toml — allow,
// ask, forbid, the unlisted default and any allow-once — for the gateway's
// guard to decide every shell command.
type turnRules struct {
	mu sync.Mutex
	m  map[turnKey]turnRule
}

// turnRule is one live turn's command rules, for the gateway to decide every
// shell command: allow (run), ask (held), forbid (refused) and allowOnce
// (command lines allowed once for this turn, from an earlier card). live marks
// the entry as a running turn, so the gateway holds a command on no list for a
// person; a turn not in the map is not guarded.
type turnRule struct {
	allow     []string
	ask       []string
	forbid    []string
	allowOnce []string
	live      bool
	// copyWrite is the writable set of a turn that runs in a session's copy: the
	// copy directory first, then any extra writable paths, exactly what the
	// per-command sandbox allows (run.go). The gateway refuses a file-edit tool
	// call whose path is outside it (gateway/copyguard.go). Empty for a turn with
	// no copy (a Direct turn).
	copyWrite []string
}

// set records a live turn's rules. Every live turn gets an entry, even with
// empty lists, so the gateway knows the turn is guarded and holds a command on
// no list for a person.
func (t *turnRules) set(session, turn string, allow, ask, forbid, allowOnce []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[turnKey]turnRule{}
	}
	t.m[turnKey{session, turn}] = turnRule{
		allow:     append([]string(nil), allow...),
		ask:       append([]string(nil), ask...),
		forbid:    append([]string(nil), forbid...),
		allowOnce: append([]string(nil), allowOnce...),
		live:      true,
	}
}

// setCopyWrite records a live turn's copy writable set, the copy directory and
// any extra writable paths. It updates the entry set() already made for the
// turn, so it is called after set once the copy is known (run.go). A turn with
// no copy never calls it, so its copyWrite stays empty.
func (t *turnRules) setCopyWrite(session, turn string, write []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.m[turnKey{session, turn}]
	if !ok {
		return
	}
	r.copyWrite = append([]string(nil), write...)
	t.m[turnKey{session, turn}] = r
}

func (t *turnRules) clear(session, turn string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, turnKey{session, turn})
}

func (t *turnRules) get(session, turn string) turnRule {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[turnKey{session, turn}]
}

// Forbid is the forbid list of a live turn, for the gateway's guard
// (gateway.Guard). A turn that is not live, or has none, gets nil.
func (e *Engine) Forbid(session, turn string) []string {
	return e.turnRules.get(session, turn).forbid
}

// Ask is the ask list of a live turn, for the gateway's guard: commands it
// holds until a person answers. A turn that is not live, or has none, gets nil.
func (e *Engine) Ask(session, turn string) []string {
	return e.turnRules.get(session, turn).ask
}

// Allow is the allow list of a live turn: commands that run without asking.
func (e *Engine) Allow(session, turn string) []string {
	return e.turnRules.get(session, turn).allow
}

// AllowOnce are the command lines a person allowed once for a live turn, from
// an earlier card. They run this turn only and are written to no settings.
func (e *Engine) AllowOnce(session, turn string) []string {
	return e.turnRules.get(session, turn).allowOnce
}

// CopyWrite is the copy writable set of a live turn — the copy directory and
// any extra writable paths — for the gateway's guard to refuse a file-edit tool
// call whose path is outside it (gateway/copyguard.go). A turn with no copy, or
// one not live, gets nil, so no edit is refused on a path.
func (e *Engine) CopyWrite(session, turn string) []string {
	return e.turnRules.get(session, turn).copyWrite
}

// Unlisted is the gateway's decision for a command of a live turn on none of
// its lists: Ask, so it is held for a person. A turn that is not live is not
// guarded, so a command on no list runs (Allow).
func (e *Engine) Unlisted(session, turn string) gateway.Decision {
	r := e.turnRules.get(session, turn)
	if !r.live {
		return gateway.Allow
	}
	return gateway.Ask
}
