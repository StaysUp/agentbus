package run

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A run's child can start tools that leave its process group, and that
// outlive it: codex-cli 0.164.0 starts each tool in a process group of its
// own, so after SIGTERM to codex its `sleep 120` kept running with parent 1
// (acceptance soak, 2026-11-06). Signalling the child's group misses them.
//
// procTree tracks the child's whole tree instead. A process belongs to it
// when it started no earlier than the child and either
//   - its parent is the child or a process already in the tree, or
//   - it is in the child's session (the child is started with Setsid, so
//     its session id is its own pid; a descendant keeps that session unless
//     it calls setsid itself).
//
// The parent link breaks when a parent exits and its children move to pid 1,
// so the tree is read again every procPoll while the child runs, and a
// process once seen stays a member while its pid and start time match. A
// process that both leaves the session and loses its parent within one poll
// is missed; nothing on macOS records a descendant past that without
// kernel help. The start time also stops a reused pid from counting: a new
// process with an old member's pid has another start time.
//
// The process table comes from /proc/<pid>/stat on Linux and from the
// kern.proc.all sysctl plus getsid on macOS (proctable_*.go). Elsewhere it
// cannot be read, the tree stays empty, and only the child's group is
// signalled, as before.
type procTree struct {
	mu        sync.Mutex
	root      int
	rootStart int64 // in the table's own unit; only compared within it
	rootGone  bool  // the child was reaped: its pid may name another process
	members   map[int]int64
	read      func(minStart int64) ([]proc, error) // procTable, when the tree began
	stop      chan struct{}
	stopOnce  sync.Once
}

// proc is one process table row. readProcs(minStart) fills sid only for a
// process started at minStart or later (getsid is a call per process on
// macOS); an older one has sid -1, and is never a member anyway.
type proc struct {
	pid, ppid, sid int
	start          int64
}

// procPoll is how often a running child's tree is read again.
const procPoll = 200 * time.Millisecond

// errNoProcTable: this platform's process table cannot be read.
var errNoProcTable = errors.New("run: the process table cannot be read here")

// procTable reads the process table: readProcs, or a stand-in in tests.
var procTable = readProcs

// newProcTree starts tracking the tree of root, a child already started and
// not yet reaped (so it is in the table, as a zombie at worst).
func newProcTree(root int) *procTree {
	t := &procTree{root: root, rootStart: -1, members: map[int]int64{}, stop: make(chan struct{}), read: procTable}
	// No session ids yet: only the child's start time is wanted.
	if ps, err := t.read(math.MaxInt64); err == nil {
		for _, p := range ps {
			if p.pid == root {
				t.rootStart = p.start
			}
		}
	}
	if t.rootStart < 0 {
		// Not readable: the tree stays empty, and signal still reaches the
		// child's own group. The child is running, so it is not marked
		// gone: only exited says that (marked gone here, the child got no
		// signal at all and StopRuns waited on it for good).
		return t
	}
	go func() {
		tick := time.NewTicker(procPoll)
		defer tick.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-tick.C:
				t.refresh()
			}
		}
	}()
	return t
}

// refresh reads the table and updates the members.
func (t *procTree) refresh() {
	ps, err := t.read(t.rootStart)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.update(ps)
}

// update applies one reading of the table. Call with mu held.
func (t *procTree) update(ps []proc) {
	if t.rootStart < 0 {
		return
	}
	by := make(map[int]proc, len(ps))
	for _, p := range ps {
		by[p.pid] = p
	}
	for pid, start := range t.members {
		if p, ok := by[pid]; !ok || p.start != start {
			delete(t.members, pid)
		}
	}
	rootAlive := false
	if p, ok := by[t.root]; ok && !t.rootGone && p.start == t.rootStart {
		rootAlive = true
	}
	// The child's session counts while its pid cannot name another process:
	// before it is reaped, or right after when nothing has taken its pid.
	session := !t.rootGone || by[t.root].pid == 0
	self := os.Getpid()
	for changed := true; changed; {
		changed = false
		for _, p := range ps {
			if p.pid == t.root || p.pid == self || p.pid <= 1 || p.start < t.rootStart {
				continue
			}
			if _, ok := t.members[p.pid]; ok {
				continue
			}
			_, parentIn := t.members[p.ppid]
			if (rootAlive && p.ppid == t.root) || parentIn || (session && p.sid == t.root) {
				t.members[p.pid] = p.start
				changed = true
			}
		}
	}
}

// exited records that the child was reaped: one last reading, then the
// session no longer counts and polling stops.
func (t *procTree) exited() {
	t.stopOnce.Do(func() { close(t.stop) })
	t.mu.Lock()
	gone := t.rootGone
	t.rootGone = true
	t.mu.Unlock()
	if gone {
		return
	}
	if ps, err := t.read(t.rootStart); err == nil {
		t.mu.Lock()
		// rootGone is set, so the session is used only if nothing took the
		// child's pid in the moment since it was reaped.
		t.update(ps)
		t.mu.Unlock()
	}
}

// signal reads the tree again and sends sig to the child's own group, while
// the child lives, and to every member and the group each member leads.
func (t *procTree) signal(sig syscall.Signal) {
	t.refresh()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.rootGone {
		_ = syscall.Kill(-t.root, sig)
	}
	for pid := range t.members {
		_ = syscall.Kill(pid, sig)
		// A pid that names a group names the group its process leads; the
		// kernel never gives a live group's id to another process.
		_ = syscall.Kill(-pid, sig)
	}
}

// left is how many members are still running.
func (t *procTree) left() int {
	t.refresh()
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.members)
}

// end stops what is left of the tree after the child exited: SIGTERM, then
// SIGKILL for whatever still runs after grace.
func (t *procTree) end(grace time.Duration) {
	if t.left() == 0 {
		return
	}
	t.signal(syscall.SIGTERM)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if t.left() == 0 {
			return
		}
	}
	t.signal(syscall.SIGKILL)
}

// stoppedBySignal says a child's exit code is one a signal gives: 128 plus
// the signal, as ExitCode and a shell report it, and as codex exits on
// SIGTERM (143).
func stoppedBySignal(code int) bool { return code > 128 && code < 128+32 }

// parseStat reads one /proc/<pid>/stat line (proc(5)): the command sits in
// parentheses and may hold spaces or ')', so fields are counted after the
// last ')'. It returns ppid, session and starttime (clock ticks after boot).
func parseStat(line string) (ppid, sid int, start int64, ok bool) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return 0, 0, 0, false
	}
	f := strings.Fields(line[i+1:])
	// f[0] is state (field 3); ppid is field 4, session 6, starttime 22.
	if len(f) < 20 {
		return 0, 0, 0, false
	}
	var err1, err2, err3 error
	ppid, err1 = strconv.Atoi(f[1])
	sid, err2 = strconv.Atoi(f[3])
	start, err3 = strconv.ParseInt(f[19], 10, 64)
	return ppid, sid, start, err1 == nil && err2 == nil && err3 == nil
}
