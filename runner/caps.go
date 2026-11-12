package runner

// What this runner reports of itself (queue.Capabilities): its machine, its
// isolation backends, whether it can build a session's wall, its harnesses as its own probe finds them, and its
// machine's id with the folders it holds. Reported at enroll, and on a heartbeat
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
	c := &queue.Capabilities{OS: runtime.GOOS, Arch: runtime.GOARCH, Isolation: isolation.Available(), Harnesses: harnesses,
		Sandbox: r.sandbox}
	if f := r.cfg.Folders; f != nil {
		// This machine, and the folders the runner holds on it.
		c.Connector = f.Connector()
		for _, fo := range f.List(queue.MaxFolders) {
			c.Folders = append(c.Folders, queue.FolderInfo{Ref: fo.Ref, Name: fo.Name})
		}
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
		hs := harness.ProbeAll(ctx, r.cfg.LookPath, r.cfg.Environ)
		if ctx.Err() != nil {
			return
		}
		r.probeMu.Lock()
		r.probed = hs
		r.probeMu.Unlock()
		r.beat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(harness.ProbeEvery):
		}
	}
}

// beat sends the runner's capabilities now, with its last probe's harnesses:
// after each probe, and from a folder job that registered a folder, so GET
// /api/v0/folders lists it before the job ends, without waiting for a probe.
// One report at a time, each read while it is sent: the folders only grow,
// so whichever report control handles last holds every folder.
func (r *Runner) beat(ctx context.Context) {
	r.beatMu.Lock()
	defer r.beatMu.Unlock()
	r.probeMu.Lock()
	hs := r.probed
	r.probeMu.Unlock()
	status, err := call(ctx, r.short, r.cfg.Queue+"/api/v0/runner/heartbeat", r.token,
		jobs.RunnerBeat{Capacity: r.cfg.Capacity, Labels: r.cfg.Labels, Capabilities: r.caps(hs)}, nil)
	if err != nil || status != http.StatusOK {
		r.cfg.Logf("runner.report_failed status=%d", status)
	}
}

// folderPath is the path of the folder a job names: by reference, from this
// runner's folders; or, for a job stored before references, its path,
// which the runner registers. ok is false when the reference is not one of
// this machine's.
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

// folderUnknown refuses a job whose folder reference this runner does not hold.
const folderUnknown = "this runner does not hold the folder the job names; a session runs on its folder's machine, and GET /api/v0/folders lists each folder with its machine"
