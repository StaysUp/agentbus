//go:build integration

package queue

import (
	"testing"
	"time"

	"github.com/staysup/agentbus/ids"
)

// Codex's P1 on the first round (2026-11-24): the finisher writes a job's
// outcome into the record and then ends the job, and a planning read between
// the two saw the record's new applied mark with the job not yet finished, so
// an apply of a covered turn made a second job. Outcome makes the two one step
// for every planning read: a read waits until the job has ended.
func TestOutcomeIsOneStepForPlanning(t *testing.T) {
	q, _ := open(t)
	rn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Isolate: true})
	ws := ids.WorkspaceID(first.Session)
	run(t, q, rn, Result{Changeset: hexOf('1'), Checkpoint: "cp1"})

	inside, release, read := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = q.Outcome(func() error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	go func() {
		_, _, _, _ = q.AppliableCursor(DefaultTeam, ws)
		close(read)
	}()
	select {
	case <-read:
		t.Fatal("a planning read ran while an outcome was being written")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("the planning read did not run once the outcome was written")
	}
}
