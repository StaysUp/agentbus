package runner

// The folder jobs of a runner's own machine, and a turn's changes going out.
// A runner holds its machine's folders (Config.Folders) and does their jobs
// through Config.FolderSide (package connector): it checks and registers a
// folder a person typed, and applies a session's changes into its folder.
// A runner never touches a folder of another machine, and a session's work
// comes only to the runner of its folder's machine (queue.Take). After each
// turn the runner puts the copy's changeset and the files it names in the
// control plane's store, from where an apply reads them, so the turn's
// changes are in the record's reach whichever runner applies them.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// FolderSide is what a runner does to its machine's folders (connector.Jobs).
// Its errors are in fixed wording for the person: they name no path.
type FolderSide interface {
	// ApplyJob makes the folder ref as the changeset blob to holds it, from
	// as the changeset from holds it ("": the copy's base). conflicts, when
	// not empty, refused it: nothing was written. partial: not every file
	// could be put in place, and files are those that were.
	ApplyJob(ctx context.Context, ref, from, to string) (files []string, conflicts []queue.ApplyConflict, partial bool, err error)
	// PreviewJob is ApplyJob's dry run, by the same code: what it would
	// write, each file's diff against the folder now, and the conflicts
	// that would refuse it. It writes nothing in the folder.
	PreviewJob(ctx context.Context, ref, from, to string) (queue.PreviewResult, error)
	// FolderJob checks the folder at path, a folder a person typed, and
	// registers it: it answers the folder's reference, or why it cannot
	// be used, in one sentence.
	FolderJob(ctx context.Context, path string) (ref string, err error)
}

// errPartial is a partial apply's refusal.
const errPartial = "the changes could not all be written to your folder; files lists those that were"

// maxApplyMessage bounds a refusal's message; past it, the message gives the
// count and conflicts lists the files.
const maxApplyMessage = 4 << 10

// applyFinish is an apply job's finish, its result sized so the finish fits
// the runner protocol's body limit.
func applyFinish(status, reason string, res queue.Result) queue.Finish {
	if res.Apply != nil {
		res.Apply.Count = len(res.Apply.Files)
		res.Apply.Fit(jobs.MaxBody - finishMargin - len(res.Refused) - 1024)
	}
	return queue.Finish{Status: status, Reason: reason, Result: mustJSON(res)}
}

// folderWork does an apply, its preview, or a folder job of this machine. It stops
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
	if r.cfg.FolderSide == nil || sp.Connector != r.cfg.Folders.Connector() {
		return refuse("this runner is not on the machine of the job's folder", nil)
	}
	if at.job.Kind == queue.KindFolder {
		if sp.Folder == "" {
			// Control keeps a typed path in memory only: it restarted since.
			return refuse("the folder's path is no longer known; send the turn again", nil)
		}
		ref, err := r.cfg.FolderSide.FolderJob(ctx, sp.Folder)
		if err != nil {
			return queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
				Result: mustJSON(queue.Result{Refused: err.Error(), Status: http.StatusBadRequest})}
		}
		// Reported before the job ends, so GET /api/v0/folders lists it
		// by the time the turn that typed it is answered.
		r.beat(ctx)
		return queue.Finish{Status: queue.Finished, Result: mustJSON(queue.Result{FolderRef: ref})}
	}
	if _, ok := r.cfg.Folders.Path(sp.FolderRef); !ok {
		return refuse(folderUnknown, nil)
	}
	if !blobs.Valid(sp.ApplyTo) || (sp.ApplyFrom != "" && !blobs.Valid(sp.ApplyFrom)) {
		return refuse("this apply was asked for before this build; ask for it again", nil)
	}
	if at.job.Kind == queue.KindPreview {
		p, err := r.cfg.FolderSide.PreviewJob(ctx, sp.FolderRef, sp.ApplyFrom, sp.ApplyTo)
		if err != nil {
			return refuse(err.Error()+"; nothing was written", nil)
		}
		p.Fit(jobs.MaxBody - finishMargin - 1024)
		return queue.Finish{Status: queue.Finished, Result: mustJSON(queue.Result{Preview: &p})}
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
