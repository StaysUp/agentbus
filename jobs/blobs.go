package jobs

// The control plane's blob store, on the runner protocol: every file of a
// snapshot or of a turn's changes goes through it (package blobs). Each
// route takes a runner's token. A blob's body is streamed to disk and checked
// against its hash as it arrives: the protocol's MaxBody does not bound it.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/queue"
)

// BlobPath is the prefix of the blob routes.
const BlobPath = "/api/v0/runner/blobs/"

// PackPath is the route that takes a pack of blobs.
const PackPath = "/api/v0/runner/packs"

func (s *Server) blobsMissing(w http.ResponseWriter, r *http.Request, _ queue.Runner) {
	if s.cfg.Blobs == nil {
		fail(w, http.StatusNotFound, "this service keeps no blob store")
		return
	}
	var b blobs.MissingRequest
	if !decode(r, &b) || len(b.Hashes) > blobs.MaxAsk {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with hashes, at most 10000")
		return
	}
	missing, err := s.cfg.Blobs.Missing(r.Context(), b.Hashes)
	if err != nil {
		fail(w, http.StatusBadRequest, "each hash must be 64 lowercase hex digits")
		return
	}
	if missing == nil {
		missing = []string{}
	}
	writeJSON(w, http.StatusOK, blobs.MissingReply{Missing: missing})
}

func (s *Server) blobPut(w http.ResponseWriter, r *http.Request, _ queue.Runner) {
	if s.cfg.Blobs == nil {
		fail(w, http.StatusNotFound, "this service keeps no blob store")
		return
	}
	h := r.PathValue("id")
	if !blobs.Valid(h) {
		fail(w, http.StatusNotFound, "no such blob")
		return
	}
	err := s.cfg.Blobs.Put(r.Context(), h, r.Body)
	switch {
	case errors.Is(err, blobs.ErrBadHash):
		fail(w, http.StatusBadRequest, "the content does not have the hash it was sent with")
		return
	case errors.Is(err, blobs.ErrTooBig):
		fail(w, http.StatusRequestEntityTooLarge, "the blob is bigger than a blob may be")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the blob could not be kept")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) blobGet(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	if s.cfg.Blobs == nil {
		fail(w, http.StatusNotFound, "this service keeps no blob store")
		return
	}
	h := r.PathValue("id")
	if !blobs.Valid(h) {
		fail(w, http.StatusNotFound, "no such blob")
		return
	}
	if !s.readable(w, r, rn, []string{h}) {
		return
	}
	rc, err := s.cfg.Blobs.Open(r.Context(), h)
	if err != nil {
		fail(w, http.StatusNotFound, "no such blob")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if f, ok := rc.(*os.File); ok {
		if fi, err := f.Stat(); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// readable says rn may read every blob of hs (mayRead), and answers the
// request when it may not.
func (s *Server) readable(w http.ResponseWriter, r *http.Request, rn queue.Runner, hs []string) bool {
	ok, err := s.mayRead(r.Context(), rn, hs)
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "the runner's jobs could not be read to check this read")
		return false
	case !ok:
		fail(w, http.StatusForbidden, notYours)
		return false
	}
	return true
}

// blobPack keeps every blob of the pack the body holds (blobs.ReadPack).
func (s *Server) blobPack(w http.ResponseWriter, r *http.Request, _ queue.Runner) {
	if s.cfg.Blobs == nil {
		fail(w, http.StatusNotFound, "this service keeps no blob store")
		return
	}
	n, err := blobs.ReadPack(r.Body, nil, blobs.PackCount, func(h string, body io.Reader) error { return s.cfg.Blobs.Put(r.Context(), h, body) })
	switch {
	case errors.Is(err, blobs.ErrBadHash):
		fail(w, http.StatusBadRequest, "a blob of the pack does not have the hash it was sent with")
		return
	case errors.Is(err, blobs.ErrBadPack):
		fail(w, http.StatusBadRequest, "the pack must be frames of a hash, a size and that many bytes")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the pack could not be kept")
		return
	}
	writeJSON(w, http.StatusOK, blobs.PackReply{Kept: n})
}

// blobFetch answers the blobs asked for as one pack, in the order asked.
func (s *Server) blobFetch(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	if s.cfg.Blobs == nil {
		fail(w, http.StatusNotFound, "this service keeps no blob store")
		return
	}
	var b blobs.FetchRequest
	if !decode(r, &b) || len(b.Hashes) == 0 || len(b.Hashes) > blobs.PackCount {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with hashes, 1 to 1000")
		return
	}
	for _, h := range b.Hashes {
		if !blobs.Valid(h) {
			fail(w, http.StatusBadRequest, "each hash must be 64 lowercase hex digits")
			return
		}
	}
	if !s.readable(w, r, rn, b.Hashes) {
		return
	}
	missing, err := s.cfg.Blobs.Missing(r.Context(), b.Hashes)
	if err != nil {
		fail(w, http.StatusBadRequest, "each hash must be 64 lowercase hex digits")
		return
	}
	if len(missing) > 0 {
		writeJSON(w, http.StatusNotFound, blobs.MissingReply{Missing: missing})
		return
	}
	// Each blob is sized before the answer starts, and opened only when it
	// is written: a pack holds one file open at a time. One gone meanwhile
	// ends the pack short, which the client refuses.
	items := make([]blobs.Item, 0, len(b.Hashes))
	for _, h := range b.Hashes {
		size, err := s.cfg.Blobs.Size(h)
		if err != nil {
			writeJSON(w, http.StatusNotFound, blobs.MissingReply{Missing: []string{h}})
			return
		}
		items = append(items, blobs.Item{Hash: h, Size: size, Open: func(ctx context.Context) (io.ReadCloser, error) { return s.cfg.Blobs.Open(ctx, h) }})
	}
	// Every write has a deadline, up to the last bytes net/http writes
	// after this returns; so the connection then closes, and no later
	// request on it inherits the deadline.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	_ = blobs.WritePack(r.Context(), &deadlineWriter{w: w, rc: http.NewResponseController(w)}, items)
}

// PackWriteWait bounds how long one write of a fetched pack may wait on a
// client that stopped reading.
const PackWriteWait = time.Minute

// deadlineWriter sets a write deadline before each write.
type deadlineWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(PackWriteWait))
	return d.w.Write(p)
}

// machineOffline refuses a turn that needs a snapshot of a folder whose
// machine's runner is gone.
const machineOffline = "the folder's machine is offline: start agentbus runner there, then send the turn again"

// SnapshotState is the answer to an attempt's snapshot request: the
// snapshot job, and once it has finished, the snapshot.
type SnapshotState struct {
	Job      string               `json:"job" doc:"The snapshot job."`
	State    string               `json:"state" doc:"The job's state: queued, taken, running, finished, failed or cancelled."`
	Snapshot *queue.SnapshotTaken `json:"snapshot,omitempty" doc:"The snapshot, once the job has finished."`
	Error    string               `json:"error,omitempty" doc:"Why the snapshot could not be taken, when the job failed."`
}

// snapshot makes, or reads, the snapshot job for the attempt's folder.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	var b AttemptCall
	if !decode(r, &b) {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with number")
		return
	}
	j, err := s.cfg.Queue.Snapshot(rn.ID, r.PathValue("id"), b.Number)
	switch {
	case errors.Is(err, queue.ErrMachineOffline):
		writeJSON(w, http.StatusOK, SnapshotState{Job: j.ID, State: j.State, Error: machineOffline})
		return
	case errors.Is(err, queue.ErrNotApplicable):
		fail(w, http.StatusBadRequest, "only a session's first turn in an isolated copy of a folder takes a snapshot")
		return
	case err != nil:
		s.queueError(w, err)
		return
	}
	st := SnapshotState{Job: j.ID, State: j.State}
	res, _ := queue.ReadResult(j.Result)
	if j.State == queue.Finished {
		st.Snapshot = res.Snapshot
	} else if j.Ended != nil {
		st.Error = res.Refused
		if st.Error == "" && j.Reason == queue.ReasonRunnerLost {
			st.Error = "the folder's runner stopped while it took the snapshot; send the turn again"
		} else if st.Error == "" {
			st.Error = "the snapshot job ended " + j.State
		}
	}
	writeJSON(w, http.StatusOK, st)
}
