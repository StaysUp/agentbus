package api

import (
	"os/exec"
	"sort"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	// The command may hold spaces and ')': fields count after the last ')'.
	line := "4242 (we ird) (x)) S 4100 4242 4100 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615\n"
	ppid, sid, start, ok := parseStat(line)
	if !ok || ppid != 4100 || sid != 4100 || start != 987654 {
		t.Fatalf("parseStat = %d %d %d %v, want 4100 4100 987654 true", ppid, sid, start, ok)
	}
	if _, _, _, ok := parseStat("12 (short) S 1 2"); ok {
		t.Fatal("a short line was read")
	}
}

func members(tr *procTree) []int {
	var out []int
	for pid := range tr.members {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func same(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The tree's rules, on tables written by hand. The child is pid 100,
// started at time 50, in its own session 100.
func TestProcTreeMembers(t *testing.T) {
	tr := &procTree{root: 100, rootStart: 50, members: map[int]int64{}}
	tr.update([]proc{
		{pid: 1, ppid: 0, sid: 1, start: 0},
		{pid: 100, ppid: 90, sid: 100, start: 50},  // the child
		{pid: 101, ppid: 100, sid: 100, start: 51}, // its tool
		{pid: 102, ppid: 101, sid: 102, start: 52}, // a grandchild in a session of its own
		{pid: 103, ppid: 1, sid: 100, start: 53},   // orphaned, still in the child's session
		{pid: 104, ppid: 102, sid: 102, start: 54}, // under the grandchild
		{pid: 200, ppid: 90, sid: 90, start: 60},   // the child's sibling: not in the tree
		{pid: 201, ppid: 100, sid: 100, start: 40}, // older than the child: a reused pid
	})
	if got := members(tr); !same(got, []int{101, 102, 103, 104}) {
		t.Fatalf("members %v, want 101 102 103 104", got)
	}
	// The child is stopped and reaped: 101 exits, 102 moves to pid 1 and
	// leaves the session. Seen once, it stays a member, and so does 104.
	tr.rootGone = true
	tr.update([]proc{
		{pid: 1, ppid: 0, sid: 1, start: 0},
		{pid: 102, ppid: 1, sid: 102, start: 52},
		{pid: 103, ppid: 1, sid: 100, start: 53},
		{pid: 104, ppid: 102, sid: 102, start: 54},
		{pid: 105, ppid: 104, sid: 102, start: 70}, // started after: its parent is a member
		{pid: 200, ppid: 90, sid: 90, start: 60},
	})
	if got := members(tr); !same(got, []int{102, 103, 104, 105}) {
		t.Fatalf("members after the child exited %v, want 102 103 104 105", got)
	}
	// A member's pid reused by an unrelated process (another start time)
	// no longer counts, and the child's pid taken by a new session leader
	// stops the session from counting.
	tr.update([]proc{
		{pid: 1, ppid: 0, sid: 1, start: 0},
		{pid: 100, ppid: 1, sid: 100, start: 80}, // a new process with the child's pid
		{pid: 106, ppid: 100, sid: 100, start: 81},
		{pid: 102, ppid: 1, sid: 102, start: 90}, // 102 reused
		{pid: 103, ppid: 1, sid: 100, start: 53},
	})
	if got := members(tr); !same(got, []int{103}) {
		t.Fatalf("members after pid reuse %v, want 103", got)
	}
}

// A process table that cannot be read leaves the tree empty.
func TestProcTreeUnreadable(t *testing.T) {
	tr := &procTree{root: 100, rootStart: -1, members: map[int]int64{}}
	tr.update([]proc{{pid: 101, ppid: 100, sid: 100, start: 51}})
	if len(tr.members) != 0 {
		t.Fatalf("members %v, want none", members(tr))
	}
}

func TestStoppedBySignal(t *testing.T) {
	for code, want := range map[int]bool{0: false, 1: false, 128: false, 130: true, 137: true, 143: true, 160: false, 255: false} {
		if got := stoppedBySignal(code); got != want {
			t.Errorf("stoppedBySignal(%d) = %v, want %v", code, got, want)
		}
	}
}

// With the process table unreadable when a run started, its tree was marked
// as if the child had exited, so a stop sent nothing to the running child
// and StopRuns waited on it for good. Without the table, a stop still
// reaches the child's own group, and later failures to read the table
// change nothing.
func TestStopWithoutProcTableReachesTheChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start bool // the table fails from the start, not only later
	}{{"unreadable at start", true}, {"unreadable later", false}} {
		t.Run(tc.name, func(t *testing.T) {
			// The tree keeps the stand-in it began with; whether it fails is
			// an atomic, so its poll never races the test.
			var failing atomic.Bool
			failing.Store(tc.start)
			defer func(f func(int64) ([]proc, error)) { procTable = f }(procTable)
			procTable = func(min int64) ([]proc, error) {
				if failing.Load() {
					return nil, errNoProcTable
				}
				return readProcs(min)
			}
			cmd := exec.Command("sleep", "30")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			var rs runs
			code, msg, tree := rs.start(cmd)
			if code != 0 {
				t.Fatalf("start: %d %s", code, msg)
			}
			defer cmd.Process.Kill()
			defer tree.exited() // stops the poll
			failing.Store(true)
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			rs.signal(syscall.SIGTERM)
			select {
			case <-waited:
			case <-time.After(5 * time.Second):
				t.Fatal("the child got no signal with the process table unreadable")
			}
			if code := ExitCode(cmd.ProcessState, nil); code != 128+15 {
				t.Fatalf("exit %d, want 143", code)
			}
		})
	}
}
