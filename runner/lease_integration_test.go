//go:build integration

package runner

import (
	"database/sql"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
	_ "modernc.org/sqlite"
)

// A heartbeat held up by a slow queue does not lose a healthy attempt. The
// queue has one connection, so a heartbeat waits for every write before it;
// here another connection holds the write lock for 2.5 s while a turn runs,
// as a slow commit did under the gate's parallel load. With the test world's
// old 2 s lease the job failed runner_lost and its finish was refused with
// 409 (TestFoldersTypedAtOnceAllListed, 2026-11-26).
func TestHeartbeatWaitsOutASlowQueue(t *testing.T) {
	w := newWorld(t, 1)
	j := w.create(t, queue.Spec{Harness: "claude", Prompt: "hold"})
	waitFor(t, "a turn", func() bool { return len(w.runs()) == 1 })
	db, err := sql.Open("sqlite", "file:"+w.jobsDB+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// A write takes the lock: every queue call waits behind it.
	if _, err := tx.Exec(`UPDATE jobs SET updated = updated WHERE id = ?`, j.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // a few heartbeats after the wait
	w.release()
	waitFor(t, "the job ended", func() bool { return w.job(t, j.ID).Ended != nil })
	if got := w.job(t, j.ID); got.State != queue.Finished {
		t.Fatalf("the job ended %s %s after a slow queue, want finished", got.State, got.Reason)
	}
}
