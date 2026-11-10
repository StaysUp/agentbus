package jobs

// A runner's record of a turn, sent to the control plane at the turn's end
// so the control plane's record holds the turn as a turn on its own machine
// does: what the cockpit reads (GET /api/v0/sessions, /turns). Each event
// keeps its id, so a batch sent twice is stored once. No runner path reaches
// the control plane: an event's folder (the copy's, on the runner) is
// dropped; its folder_ref stays.

import (
	"encoding/json"
	"net/http"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
)

// RecordBatch is the body of POST /api/v0/attempts/{id}/record: events of
// the attempt's turn, as the runner's own record stored them.
type RecordBatch struct {
	Number int               `json:"number" doc:"The attempt's number."`
	Events []json.RawMessage `json:"events" doc:"The turn's events, at most 500, each with its id and the attempt's turn id as turn."`
}

// RecordAck answers a RecordBatch: how many events were new.
type RecordAck struct {
	Stored int `json:"stored"`
}

// Recorder is the control plane's record, for a runner's turn.
type Recorder interface {
	AppendOnce(e event.Event) (bool, error)
}

func (s *Server) record(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	if s.cfg.Record == nil {
		fail(w, http.StatusNotFound, "this service keeps no record")
		return
	}
	var b RecordBatch
	if !decode(r, &b) || len(b.Events) > MaxBatch {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with number and at most 500 events")
		return
	}
	turn := r.PathValue("id")
	j, err := s.cfg.Queue.Current(rn.ID, turn, b.Number)
	if err != nil {
		s.queueError(w, err)
		return
	}
	evs := make([]event.Event, 0, len(b.Events))
	for _, raw := range b.Events {
		var e event.Event
		if json.Unmarshal(raw, &e) != nil || e.ID == "" || e.Kind == "" || len(e.Kind) > 64 || e.Turn != turn || e.Session == "" {
			fail(w, http.StatusBadRequest, "every event needs an id, a kind, a session, and the attempt's turn as turn")
			return
		}
		// A drop marks other sessions' events as missing: it is the
		// store's own, never a turn's.
		if e.Kind == event.EventsDropped {
			fail(w, http.StatusBadRequest, "events.dropped is not a turn's event")
			return
		}
		e.Seq, e.Folder = 0, ""
		evs = append(evs, e)
	}
	// One rule: every event is of the session control assigned the job.
	// A turn job's is its session; a review's is the one control named
	// when it made the job (Links.ReviewSession), never one the runner
	// names. Both are fixed when the job is made, so the check needs no
	// lock with the append. Any other job has no record to send.
	assigned := ""
	switch j.Kind {
	case queue.KindTurn:
		assigned = j.Session
	case queue.KindReview:
		assigned = j.Links.ReviewSession
	}
	for _, e := range evs {
		if assigned == "" || e.Session != assigned {
			fail(w, http.StatusBadRequest, "every event must be of the session control assigned the attempt's job")
			return
		}
	}
	n := 0
	for _, e := range evs {
		stored, err := s.cfg.Record.AppendOnce(e)
		if err != nil {
			s.internal(w)
			return
		}
		if stored {
			n++
		}
	}
	writeJSON(w, http.StatusOK, RecordAck{Stored: n})
}
