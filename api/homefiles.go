package api

// "Use my config": GET /api/v0/folders/{id}/home-files says which of the
// usual config files (settings.UsualHomeFiles) the home of the folder's
// machine holds, so a cockpit can fill Home files with them. Control never
// sees a home: the machine's runner answers a home job (queue.KindHome),
// with names, kinds and sizes only, and the job is removed once read. A
// vendor sign-in file is never in the answer.

import (
	"errors"
	"net/http"
	"time"

	"github.com/staysup/agentbus/queue"
)

// HomeFileList is the answer to GET /api/v0/folders/{id}/home-files.
type HomeFileList struct {
	Folder  string           `json:"folder" doc:"The folder's reference, as asked."`
	Machine string           `json:"machine" doc:"The machine whose home was looked in: the folder's."`
	Files   []queue.HomeFile `json:"files" doc:"Each usual config path that machine's home holds, in a fixed order, as home_files takes it. A path it does not hold is left out. Never a vendor sign-in file."`
}

// Answers of the home-files route, in fixed wording.
const (
	homeNoFolder = "no runner holds that folder; GET /api/v0/folders lists them"
	homeTwo      = "two machines hold a folder of that reference; name one as machine, from GET /api/v0/folders"
	homeOffline  = "the folder's machine is offline; start agentbus runner there, and ask again"
	homeSlow     = "the folder's machine did not answer within 15 seconds; ask again"
	homeFailed   = "the folder's machine could not list its home"
	homeRefused  = "a turn token reads a machine's home only from a session that works in that machine's folders as they are"
)

// GET /api/v0/folders/<ref>/home-files.
func (a *API) homeFiles(w http.ResponseWriter, r *http.Request, ref string) {
	if !noBody(w, r) {
		return
	}
	if !queue.FolderRefShape.MatchString(ref) {
		writeError(w, http.StatusNotFound, homeNoFolder)
		return
	}
	if a.cfg.Queue == nil {
		writeError(w, http.StatusNotFound, homeNoFolder)
		return
	}
	want := r.URL.Query().Get("machine")
	if want == "" {
		want = r.URL.Query().Get("connector") // compat 0.6: drop connector
	}
	if want != "" && !queue.ConnectorShape.MatchString(want) {
		writeError(w, http.StatusBadRequest, "machine must be con_ and 16 hex digits: a machine's id, from GET /api/v0/folders")
		return
	}
	all, err := a.cfg.Queue.Folders(queue.DefaultTeam)
	if err != nil {
		a.internal(w)
		return
	}
	machine := ""
	// agentbus up's own folders are known at once; the runners report
	// theirs within a minute.
	if own := a.cfg.Folders; own != nil && (want == "" || want == own.Machine()) {
		if _, ok := own.Path(ref); ok {
			machine = own.Machine()
		}
	}
	for _, f := range all {
		if f.Ref != ref || (want != "" && f.Machine != want) || f.Machine == machine {
			continue
		}
		if machine != "" {
			writeError(w, http.StatusBadRequest, homeTwo)
			return
		}
		machine = f.Machine
	}
	if machine == "" {
		writeError(w, http.StatusNotFound, homeNoFolder)
		return
	}
	// A machine's home is outside every copy: a turn token reads it only from
	// a Direct session on that machine (O2.3, limit 1).
	if reach, err := callerOf(r).limits.machines(a); err != nil {
		a.internal(w)
		return
	} else if !reach.home(machine) {
		writeError(w, http.StatusForbidden, homeRefused)
		return
	}
	seen, err := a.cfg.Queue.MachineSeen(queue.DefaultTeam, machine)
	if err != nil {
		a.internal(w)
		return
	}
	if !seen {
		writeError(w, http.StatusServiceUnavailable, homeOffline)
		return
	}
	job, err := a.cfg.Queue.AskHome(queue.DefaultTeam, machine)
	switch {
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many jobs are waiting; wait for some to end and retry")
		return
	case err != nil:
		a.internal(w)
		return
	}
	// The job goes once read, or once it ends unread.
	defer func() {
		_, _ = a.cfg.Queue.Cancel(queue.DefaultTeam, job.ID)
		_ = a.cfg.Queue.DropHome(queue.DefaultTeam, job.ID)
	}()
	j, err := a.awaitHome(r, job)
	if err != nil || j.Ended == nil {
		switch {
		case r.Context().Err() != nil:
		case err != nil:
			a.internal(w)
		default:
			writeError(w, http.StatusGatewayTimeout, homeSlow)
		}
		return
	}
	res, _ := queue.ReadResult(j.Result)
	if j.State != queue.Finished {
		msg := res.Refused
		if msg == "" {
			msg = homeFailed
		}
		writeError(w, http.StatusConflict, msg)
		return
	}
	files := res.Home
	if files == nil {
		files = []queue.HomeFile{}
	}
	writeJSON(w, http.StatusOK, HomeFileList{Folder: ref, Machine: machine, Files: files})
}

// awaitHome is the home job once it ended, or as it is after
// queue.FolderWait, or when the caller went away.
func (a *API) awaitHome(r *http.Request, job queue.Job) (queue.Job, error) {
	deadline := time.NewTimer(queue.FolderWait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := a.cfg.Queue.Get(queue.DefaultTeam, job.ID)
		if err != nil || j.Ended != nil {
			return j, err
		}
		select {
		case <-r.Context().Done():
			return j, r.Context().Err()
		case <-deadline.C:
			return j, nil
		case <-tick.C:
		}
	}
}
