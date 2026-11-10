package runner

// The folder side's jobs on a connector, and a runner's copy built from a
// snapshot and its changes going out. A connector is a runner with
// Config.Role queue.RoleConnector: it takes only snapshot and apply jobs, and
// does them through Config.FolderSide (package connector). A runner never
// touches a folder of another machine: it asks for a snapshot through the
// control plane, builds the copy from the snapshot's blobs, and after each
// turn puts the copy's changeset and the files it names in the control
// plane's store, from where an apply reads them.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// FolderSide is what a connector does to its folders (connector.Jobs).
// Its errors are in fixed wording for the person: they name no path.
type FolderSide interface {
	// SnapshotJob takes a snapshot of the folder ref; its trees and files
	// are in the control plane's store once it answers.
	SnapshotJob(ctx context.Context, ref string) (queue.SnapshotTaken, error)
	// ApplyJob makes the folder ref as the changeset blob to holds it, from
	// as the changeset from holds it ("": the copy's base). conflicts, when
	// not empty, refused it: nothing was written. partial: not every file
	// could be put in place, and files are those that were.
	ApplyJob(ctx context.Context, ref, from, to string) (files []string, conflicts []queue.ApplyConflict, partial bool, err error)
}

// errPartial is a partial apply's refusal.
const errPartial = "the changes could not all be written to your folder; files lists those that were"

// maxApplyMessage bounds a refusal's message; past it, the message gives the
// count and conflicts lists the files.
const maxApplyMessage = 4 << 10

// applyFinish is an apply job's finish, its result sized so the finish fits
// the jobs API's body limit.
func applyFinish(status, reason string, res queue.Result) queue.Finish {
	if res.Apply != nil {
		res.Apply.Count = len(res.Apply.Files)
		res.Apply.Fit(jobs.MaxBody - finishMargin - len(res.Refused) - 1024)
	}
	return queue.Finish{Status: status, Reason: reason, Result: mustJSON(res)}
}

// folderWork does a snapshot or an apply job, on a connector. It stops
// reading the folder and the store once the job is cancelled, the attempt
// is no longer current, or the runner stops: an apply stopped so writes
// nothing, as no file is put in place before every one is staged.
func (r *Runner) folderWork(ctx context.Context, at *attempt) queue.Finish {
	ctx, stop := attemptContext(ctx, at)
	defer stop()
	sp := at.job.Spec
	refuse := func(msg string, written []string) queue.Finish {
		res := queue.Result{Refused: msg, Status: http.StatusConflict}
		if len(written) > 0 {
			res.Apply = &queue.ApplyResult{Files: written}
		}
		return applyFinish(queue.Failed, queue.ReasonSetup, res)
	}
	if r.cfg.Role != queue.RoleConnector || r.cfg.FolderSide == nil || r.cfg.Folders == nil ||
		sp.Connector != r.cfg.Folders.Connector() {
		return refuse("this runner is not the connector of the job's folder", nil)
	}
	if _, ok := r.cfg.Folders.Path(sp.FolderRef); !ok {
		return refuse(folderUnknown, nil)
	}
	if at.job.Kind == queue.KindSnapshot {
		t, err := r.cfg.FolderSide.SnapshotJob(ctx, sp.FolderRef)
		if err != nil {
			r.cfg.Logf("the snapshot of %s could not be taken: %s", sp.FolderRef, err)
			return queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
				Result: mustJSON(queue.Result{Refused: "the snapshot of the folder could not be taken: " + err.Error()})}
		}
		at.event("folder.snapshot", map[string]any{"snapshot": t.ID, "files": t.Files})
		return queue.Finish{Status: queue.Finished, Result: mustJSON(queue.Result{Snapshot: &t})}
	}
	if !blobs.Valid(sp.ApplyTo) || (sp.ApplyFrom != "" && !blobs.Valid(sp.ApplyFrom)) {
		return refuse("this apply was asked for before this build; ask for it again", nil)
	}
	files, conflicts, partial, err := r.cfg.FolderSide.ApplyJob(ctx, sp.FolderRef, sp.ApplyFrom, sp.ApplyTo)
	switch {
	case len(conflicts) > 0:
		msg := fmt.Sprintf("apply refused: %d file(s) in your folder changed since the checkpoint", len(conflicts))
		if full := msg + ": " + joinPaths(conflicts); len(full) <= maxApplyMessage {
			msg = full
		} else {
			msg += "; conflicts lists them"
		}
		res := queue.Result{Refused: msg, Status: http.StatusConflict, Apply: &queue.ApplyResult{Files: []string{}, Conflicts: conflicts}}
		at.event("workspace.apply_refused", map[string]any{"turn": at.job.Links.AppliesTurn, "conflicts": len(conflicts)})
		return applyFinish(queue.Failed, queue.ReasonConflict, res)
	case partial:
		return refuse(errPartial, files)
	case err != nil:
		r.cfg.Logf("the apply of turn %s could not be done: %s", at.job.Links.AppliesTurn, err)
		return refuse(err.Error()+"; nothing was written", nil)
	}
	at.event("workspace.applied", map[string]any{"turn": at.job.Links.AppliesTurn, "files": len(files)})
	return applyFinish(queue.Finished, "", queue.Result{Apply: &queue.ApplyResult{Files: files}})
}

func joinPaths(cs []queue.ApplyConflict) string {
	var b bytes.Buffer
	for i, c := range cs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.Path)
	}
	return b.String()
}

// snapshotWait bounds how long a runner waits for a folder's snapshot.
const snapshotWait = 30 * time.Minute

// fromSnapshot builds the session's copy from a snapshot of the folder its
// job names: the runner asks the control plane for one, which the folder's
// connector takes, and builds the copy from its blobs.
func (r *Runner) fromSnapshot(ctx context.Context, at *attempt) *queue.Result {
	if r.cfg.Blobs == nil {
		return &queue.Result{Refused: "this runner keeps no blob store, so it cannot copy a folder of another machine"}
	}
	deadline := time.Now().Add(snapshotWait)
	var st jobs.SnapshotState
	for {
		var e jobs.JobError
		status, err := callErr(ctx, r.short, at.path("snapshot"), r.token, jobs.AttemptCall{Number: at.a.Number}, &st, &e)
		switch {
		case ctx.Err() != nil:
			return &queue.Result{Refused: "the runner stopped while the folder's snapshot was taken"}
		case err == nil && status == http.StatusOK && st.Snapshot != nil:
		case err == nil && status == http.StatusOK && st.Error != "":
			return &queue.Result{Refused: st.Error}
		case err == nil && status != http.StatusOK && status < 500:
			return &queue.Result{Refused: "the folder's snapshot could not be asked for: " + e.Error}
		case time.Now().After(deadline):
			return &queue.Result{Refused: "the folder's snapshot was not taken in time"}
		default:
			select {
			case <-at.stale:
				return &queue.Result{Refused: "the attempt is no longer current"}
			case <-at.cancel:
				return &queue.Result{Refused: "the turn was stopped while the folder's snapshot was taken"}
			case <-ctx.Done():
				return &queue.Result{Refused: "the runner stopped while the folder's snapshot was taken"}
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		break
	}
	at.event("workspace.snapshot", map[string]any{"snapshot": st.Snapshot.ID, "files": st.Snapshot.Files, "job": st.Job})
	p, err := r.policy()
	if err != nil {
		return &queue.Result{Refused: "the permissions cannot be read, so the turn did not start; fix them and send the turn again"}
	}
	b, ok := isolation.For(p.Isolation, "")
	if !ok {
		b, _ = isolation.Get(isolation.Copy)
	}
	snap := &isolation.Snapshot{ID: st.Snapshot.ID, Blobs: r.cfg.Blobs, Times: st.Snapshot.Times, Git: st.Snapshot.Git}
	// The copy's blobs stop coming once the turn is stopped, the attempt is
	// no longer current, or the runner stops.
	bctx, stop := attemptContext(ctx, at)
	defer stop()
	return r.isolate(at, b, isolation.Request{Snapshot: snap, Ref: at.ref, Ctx: bctx})
}

// publish puts what the session's copy holds at the checkpoint name that
// differs from its base into the control plane's store, with every file it
// names, and answers the changeset's blob. "" when this runner has no store.
func (r *Runner) publish(ctx context.Context, s *isolation.Space, name string) (string, error) {
	if r.cfg.BlobSink == nil {
		return "", nil
	}
	cs, err := isolation.ChangesetOf(s, name)
	if err != nil {
		return "", err
	}
	// The file as the turn left it, and as the Base had it: an apply from a
	// later turn may put it back.
	type need struct {
		rel  string
		f    workspace.File
		base bool
	}
	needs := map[string]need{}
	var hashes []string
	for _, f := range cs.Files {
		if f.Now != nil {
			if _, ok := needs[f.Now.Hash]; !ok {
				needs[f.Now.Hash] = need{f.Path, *f.Now, false}
				hashes = append(hashes, f.Now.Hash)
			}
		}
		if f.Base != nil && f.Base.Hash != "" {
			if _, ok := needs[f.Base.Hash]; !ok {
				needs[f.Base.Hash] = need{f.Path, *f.Base, true}
				hashes = append(hashes, f.Base.Hash)
			}
		}
	}
	missing, err := blobs.MissingAll(ctx, r.cfg.BlobSink, hashes)
	if err != nil {
		return "", err
	}
	// A store on this disk takes a clone of each saved file. Over HTTP the
	// files are kept in the runner's own store first, a clone each where
	// it can, and go from there many to a request (blobs.Copy).
	type filer interface {
		PutFile(p, hash string, link func(src, dst string) error) (string, error)
		Put(ctx context.Context, h string, r io.Reader) error
	}
	local, _ := r.cfg.BlobSink.(filer)
	var own *blobs.Store
	if l, ok := r.cfg.Blobs.(blobs.Local); ok && local == nil && l.Store != nil {
		own, local = l.Store, l.Store
	}
	var staged []string
	for _, h := range missing {
		if p, ok := isolation.BlobPath(s, h); ok && local != nil {
			if _, err := local.PutFile(p, h, isolation.Clone); err != nil {
				return "", err
			}
			staged = append(staged, h)
			continue
		}
		n := needs[h]
		var rc io.ReadCloser
		if n.base {
			rc, err = isolation.BaseBlob(s, n.rel, n.f)
		} else {
			rc, err = isolation.Blob(s, h)
		}
		if err != nil {
			if n.base {
				// The Base's content is gone: an apply that would put it back
				// is refused then, and writes nothing.
				continue
			}
			return "", fmt.Errorf("the copy keeps no file %s", h)
		}
		if local != nil {
			err = local.Put(ctx, h, rc)
			staged = append(staged, h)
		} else {
			err = r.cfg.BlobSink.Put(ctx, h, rc)
		}
		rc.Close()
		if err != nil {
			return "", err
		}
	}
	if own != nil {
		if err := blobs.Copy(ctx, own, r.cfg.BlobSink, staged); err != nil {
			return "", err
		}
	}
	b, _ := json.Marshal(cs)
	h := sha256Hex(b)
	if m, err := r.cfg.BlobSink.Missing(ctx, []string{h}); err != nil {
		return "", err
	} else if len(m) > 0 {
		if err := r.cfg.BlobSink.Put(ctx, h, bytes.NewReader(b)); err != nil {
			return "", err
		}
	}
	return h, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range in {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// UseBlobs gives the runner its blob store, once it has enrolled: a store
// over HTTP needs the runner's own token.
func (r *Runner) UseBlobs(src isolation.BlobSource, sink blobs.Sink) {
	r.cfg.Blobs, r.cfg.BlobSink = src, sink
}

// attemptContext is ctx, ended also when the attempt's job is cancelled or
// the attempt is no longer current.
func attemptContext(ctx context.Context, at *attempt) (context.Context, context.CancelFunc) {
	c, stop := context.WithCancel(ctx)
	go func() {
		select {
		case <-at.cancel:
		case <-at.stale:
		case <-c.Done():
		}
		stop()
	}()
	return c, stop
}
