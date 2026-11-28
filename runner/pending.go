package runner

// A turn's end kept on disk until control has it (Config.Finishes). A runner
// that leaves while control is away (agentbus up stopped, its built-in runner
// with no agent left to run) leaves the ends it could not send here, and the
// next runner of this machine sends them when it starts: control renews every
// lease when it starts (queue.RenewLeases), so the attempt is still current.
// An end is dropped only on control's answer: taken, or refused as no longer
// current (lease.go).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// kept is one attempt's end as kept on disk: what finish needs to send it.
type kept struct {
	Job     queue.Job            `json:"job"`
	Attempt queue.Attempt        `json:"attempt"`
	Finish  queue.Finish         `json:"finish"` // before the changes are added
	Events  []queue.AttemptEvent `json:"events,omitempty"`
	Next    int64                `json:"next"`
	// Space is the copy's id, when the job ran in one; Changes what it
	// changed there (localChanges).
	Space     string             `json:"space,omitempty"`
	Changes   *workspace.Changes `json:"changes,omitempty"`
	ChangesOK bool               `json:"changes_ok,omitempty"`
	Workspace *queue.Workspace   `json:"workspace,omitempty"`
	Session   string             `json:"session,omitempty"`
}

func (r *Runner) keptPath(attemptID string) string {
	return filepath.Join(r.cfg.Finishes, attemptID+".json")
}

// keep writes the attempt's end f to disk, whole or not at all.
func (r *Runner) keep(at *attempt, f queue.Finish) {
	if r.cfg.Finishes == "" {
		return
	}
	at.mu.Lock()
	k := kept{Job: at.job, Attempt: at.a, Finish: f, Events: append([]queue.AttemptEvent(nil), at.pending...), Next: at.next,
		Changes: at.endChanges, ChangesOK: at.endOK, Workspace: at.ws, Session: at.session}
	at.mu.Unlock()
	if at.space != nil {
		k.Space = at.space.ID
	}
	b, err := json.Marshal(k)
	if err == nil {
		err = os.MkdirAll(r.cfg.Finishes, 0o700)
	}
	if err == nil {
		tmp := r.keptPath(at.a.ID) + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, r.keptPath(at.a.ID))
		}
	}
	if err != nil {
		r.cfg.Logf("job %s: the turn's end could not be kept on disk", at.job.ID)
	}
}

// unkeep removes the attempt's kept end: control has it, or the attempt is
// gone.
func (r *Runner) unkeep(at *attempt) {
	if r.cfg.Finishes != "" {
		_ = os.Remove(r.keptPath(at.a.ID))
	}
}

// sendKept sends, each on its own, the ends a runner before this one kept,
// until ctx ends.
func (r *Runner) sendKept(ctx context.Context, wg *sync.WaitGroup) {
	if r.cfg.Finishes == "" {
		return
	}
	entries, _ := os.ReadDir(r.cfg.Finishes)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		b, err := os.ReadFile(r.keptPath(id))
		var k kept
		if err != nil || json.Unmarshal(b, &k) != nil || k.Attempt.ID != id {
			r.cfg.Logf("finishes.unreadable file=%s", e.Name())
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.busy.Add(1)
			defer r.busy.Add(-1)
			r.sendOne(ctx, k)
		}()
	}
}

// sendOne sends one kept end, as the runner that kept it would have.
func (r *Runner) sendOne(ctx context.Context, k kept) {
	at := &attempt{r: r, job: k.Job, a: k.Attempt, next: k.Next, pending: k.Events, ws: k.Workspace, session: k.Session,
		endChanges: k.Changes, endOK: k.ChangesOK,
		cancel: make(chan struct{}), stale: make(chan struct{})}
	if k.Space != "" {
		s, err := r.loadSpace(queue.Workspace{Space: k.Space})
		if err != nil {
			r.cfg.Logf("job %s: the copy of its kept end is gone; its end goes without its changes", k.Job.ID)
			at.spaceLost = k.Job.Kind == queue.KindTurn
		} else {
			at.space = s
		}
	}
	if at.space != nil && at.endChanges == nil {
		at.endChanges, at.endOK = &workspace.Changes{Kind: workspace.KindSnapshot, Files: []workspace.Change{}}, false
	}
	r.cfg.Logf("job %s attempt %d: sending the end a runner before kept", k.Job.ID, k.Attempt.Number)
	done := make(chan struct{})
	defer close(done)
	go at.heartbeats(ctx, done)
	at.finish(ctx, k.Finish)
}
