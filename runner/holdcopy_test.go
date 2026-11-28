package runner

import (
	"context"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
)

// The one rule of a copy's line (copyHolder.waitsFor), with a command control
// declared lost whose process is still stopping, so it still holds the copy:
// a turn passes it, as control lets a turn run beside a command; every other
// kind waits until it lets go. (Review t_e820610203a4480d P1: discard, update,
// close and put-away passed it and changed the copy under the command.)
func TestOnlyATurnPassesACommandInItsCopy(t *testing.T) {
	for _, c := range []struct {
		kind   string
		passes bool
	}{
		{queue.KindTurn, true},
		{queue.KindExec, false},
		{queue.KindDiscard, false},
		{queue.KindUpdate, false},
		{queue.KindClose, false},
		{queue.KindPutAway, false},
		{"a kind added later", false},
	} {
		t.Run(c.kind, func(t *testing.T) {
			r := &Runner{cfg: Config{Logf: t.Logf}}
			attempt := func(kind string) *attempt {
				return &attempt{r: r, job: queue.Job{ID: "job_" + kind, Kind: kind, Spec: queue.Spec{Workspace: "ws_1"}},
					stale: make(chan struct{})}
			}
			ctx := context.Background()
			stopCommand, ok := r.holdCopy(ctx, attempt(queue.KindExec)) // the lost command, still stopping
			if !ok {
				t.Fatal("the command did not get its copy")
			}
			got := make(chan func(), 1)
			go func() {
				release, ok := r.holdCopy(ctx, attempt(c.kind))
				if ok {
					got <- release
				}
			}()
			select {
			case release := <-got:
				if !c.passes {
					t.Fatalf("a %s entered the copy while the command before it still ran", c.kind)
				}
				release()
			case <-time.After(300 * time.Millisecond):
				if c.passes {
					t.Fatalf("a %s waited for a command; a turn runs beside one", c.kind)
				}
			}
			stopCommand()
			if !c.passes {
				select {
				case release := <-got:
					release()
				case <-time.After(5 * time.Second):
					t.Fatalf("a %s still waits after the command let go of the copy", c.kind)
				}
			}
		})
	}
}
