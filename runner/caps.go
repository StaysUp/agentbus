package runner

// What this runner reports of itself (queue.Capabilities): its machine, its
// isolation backends, its harnesses as its own probe finds them, and the
// folders it owns as their connector. Reported at enroll, and on a heartbeat
// once its first probe ends and every harness.ProbeEvery after.

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
)

// caps is what the runner can report now. harnesses is nil before a probe.
func (r *Runner) caps(harnesses []harness.Report) *queue.Capabilities {
	if r.cfg.Role == queue.RoleConnector {
		// A connector runs no agent: it reports its folders only.
		c := &queue.Capabilities{OS: runtime.GOOS, Arch: runtime.GOARCH, Role: queue.RoleConnector, Connector: r.cfg.Folders.Connector()}
		for _, fo := range r.cfg.Folders.List(queue.MaxFolders) {
			c.Folders = append(c.Folders, queue.FolderInfo{Ref: fo.Ref, Name: fo.Name})
		}
		return c
	}
	c := &queue.Capabilities{OS: runtime.GOOS, Arch: runtime.GOARCH, Isolation: isolation.Available(), Harnesses: harnesses}
	if f := r.cfg.Folders; f != nil {
		// On the connector's machine: it can use those folders by the
		// local fast path. The connector reports them.
		c.Connector = f.Connector()
	}
	return c
}

// report probes the harnesses and sends what the runner can do, now and
// every harness.ProbeEvery, until ctx ends. Without LookPath it finds no
// harness; Take still hands it a job on a harness no runner has, whose
// engine then refuses it with how to install the program.
func (r *Runner) report(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		var hs []harness.Report
		if r.cfg.Role != queue.RoleConnector {
			hs = harness.ProbeAll(ctx, r.cfg.LookPath, r.cfg.Environ)
		}
		if ctx.Err() != nil {
			return
		}
		status, err := call(ctx, r.short, r.cfg.Queue+"/api/v0/runners/"+r.id+"/heartbeat", r.token,
			jobs.RunnerBeat{Capacity: r.cfg.Capacity, Labels: r.cfg.Labels, Capabilities: r.caps(hs)}, nil)
		if err != nil || status != http.StatusOK {
			r.cfg.Logf("runner.report_failed status=%d", status)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(harness.ProbeEvery):
		}
	}
}

// folderPath is the path of the folder a job names: by reference, from this
// runner's connector; or, for a job stored before references, its path,
// which the connector registers. ok is false when the reference is not one
// of this connector's.
func (r *Runner) folderPath(path, ref, connector string) (string, bool) {
	switch {
	case ref != "":
		if r.cfg.Folders == nil || connector != r.cfg.Folders.Connector() {
			return "", false
		}
		return r.cfg.Folders.Path(ref)
	case path != "" && r.cfg.Folders != nil:
		if _, err := r.cfg.Folders.Register(path); err != nil {
			return "", false
		}
	}
	return path, true
}

// folderUnknown refuses a job whose folder reference this runner does not own.
const folderUnknown = "this runner does not own the folder the job names; GET /api/v0/folders lists each folder with the runner that owns it"
