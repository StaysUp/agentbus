package api

// A folder a person typed for a new session. Control never reads a folder,
// so a connector checks it: the one of agentbus up directly
// (queue.FolderIssuer), any other through a folder job that the connector
// takes (queue.KindFolder). Either way the folder is registered there, and
// the session goes on by its reference. GET /api/v0/folders lists it from
// then on.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/staysup/agentbus/queue"
)

// typedFolder is the reference of the folder at path, and the connector that
// registered it. Which connector: the one named; else this service's own;
// else the only one connected. status and msg say why not, when msg is not
// empty; status 0 means the caller went away.
func (a *API) typedFolder(ctx context.Context, path, connector string) (ref, conn string, status int, msg string) {
	if own := a.cfg.Folders; own != nil && (connector == "" || connector == own.Connector()) {
		ref, err := own.Add(path)
		if err != nil {
			return "", "", http.StatusBadRequest, err.Error()
		}
		return ref, own.Connector(), 0, ""
	}
	if connector != "" {
		if !queue.ConnectorShape.MatchString(connector) {
			return "", "", http.StatusBadRequest, "connector must be con_ and 16 hex digits, from GET /api/v0/folders"
		}
		seen, err := a.cfg.Queue.ConnectorSeen(queue.DefaultTeam, connector)
		if err != nil {
			return "", "", http.StatusInternalServerError, "internal"
		}
		if !seen {
			return "", "", http.StatusServiceUnavailable, "connector " + connector + " is not connected; start it, or name another"
		}
	} else {
		all, err := a.cfg.Queue.Connectors(queue.DefaultTeam)
		if err != nil {
			return "", "", http.StatusInternalServerError, "internal"
		}
		switch len(all) {
		case 0:
			return "", "", http.StatusServiceUnavailable, "no connector is connected, so the folder cannot be reached; start agentbus connector on the folder's machine"
		case 1:
			connector = all[0]
		default:
			return "", "", http.StatusBadRequest, strconv.Itoa(len(all)) +
				" connectors are connected; name the folder's connector, from GET /api/v0/folders"
		}
	}
	job, err := a.cfg.Queue.RegisterFolder(queue.DefaultTeam, connector, path)
	switch {
	case errors.Is(err, queue.ErrFull):
		return "", "", http.StatusTooManyRequests, "too many jobs are waiting; wait for some to end and retry"
	case err != nil:
		return "", "", http.StatusInternalServerError, "internal"
	}
	// The path goes from the job whatever happens, once it is read.
	defer a.cfg.Queue.ForgetFolder(queue.DefaultTeam, job.ID)
	deadline := time.NewTimer(queue.FolderWait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := a.cfg.Queue.Get(queue.DefaultTeam, job.ID)
		if err != nil {
			return "", "", http.StatusInternalServerError, "internal"
		}
		if j.Ended != nil {
			res, _ := queue.ReadResult(j.Result)
			switch {
			case j.State == queue.Finished && queue.FolderRefShape.MatchString(res.FolderRef):
				return res.FolderRef, connector, 0, ""
			case res.Refused != "":
				return "", "", http.StatusBadRequest, res.Refused
			}
			return "", "", http.StatusServiceUnavailable, "the connector could not check the folder; send the turn again"
		}
		select {
		case <-ctx.Done():
			return "", "", 0, "gone"
		case <-deadline.C:
			return "", "", http.StatusServiceUnavailable, "the connector did not check the folder within 15 seconds; check that it runs, and send the turn again"
		case <-tick.C:
		}
	}
}
