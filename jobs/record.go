package jobs

// A runner's record of a turn, sent to the control plane while the turn
// runs and once more at its end, so the control plane's record holds the
// turn as a turn on its own machine does: what a cockpit reads (GET
// /api/v0/sessions, /turns, /calls), while the turn runs. Each event keeps
// its id, so a batch sent twice is stored once. No runner path reaches
// the control plane: an event's folder (the copy's, on the runner) is
// dropped; its folder_ref stays.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// RecordBatch is the body of POST /api/v0/runner/attempts/{id}/record: events of
// the attempt's turn, as the runner's own record stored them.
type RecordBatch struct {
	Number int               `json:"number" doc:"The attempt's number."`
	Events []json.RawMessage `json:"events" doc:"The turn's events, at most 500, each with its id and the job's turn id as turn."`
}

// RecordAck answers a RecordBatch: how many events were new.
type RecordAck struct {
	Stored int `json:"stored"`
}

// Recorder is the control plane's record, for a runner's turn.
type Recorder interface {
	AppendOnce(e event.Event) (bool, error)
	KeepSent(c store.CallContext) (missing []string, err error)
	KeepReply(r body.Reply) error
	CallSession(exchange string) (session string, found bool, err error)
	OtherTurns(exchange, session, turn string) (bool, error)
	Continuation(session string) (store.Continuation, error)
	Finder
}

// CallBatch is the body of POST /api/v0/runner/attempts/{id}/calls: model calls of
// the attempt's turn as the runner's own record kept them, and the replies
// kept for them. A piece may come without its bytes when the runner sent
// them before; control answers with the hashes it lacks. Every body is sent
// as base64, so its bytes arrive as they were kept: JSON's own encoding
// would compact them and escape <, > and &, and a piece's hash names its
// exact bytes.
type CallBatch struct {
	Number  int           `json:"number" doc:"The attempt's number."`
	Calls   []SentCall    `json:"calls" doc:"Calls of the turn, each with its pieces; a piece without bytes must be held by control already."`
	Replies []ReplyRecord `json:"replies" doc:"Replies kept for calls of the turn."`
}

// SentCall is one call as the runner's record kept it.
type SentCall struct {
	Exchange string      `json:"exchange"`
	Session  string      `json:"session"`
	Vendor   string      `json:"vendor"`
	Time     time.Time   `json:"time"`
	Size     int         `json:"size"`
	Top      []byte      `json:"top" doc:"The request's other top-level fields, as kept, base64."`
	Pieces   []SentPiece `json:"pieces"`
}

// SentPiece is one piece of a SentCall.
type SentPiece struct {
	Hash  string `json:"hash"`
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
	Bytes []byte `json:"bytes,omitempty" doc:"The piece's bytes, base64; absent when control holds them."`
}

// SentCallOf is c as it is sent: with every piece's bytes, or without those
// of the pieces have says control holds.
func SentCallOf(c store.CallContext, have func(hash string) bool) SentCall {
	out := SentCall{Exchange: c.Exchange, Session: c.Session, Vendor: c.Vendor, Time: c.Time, Size: c.Size, Top: []byte(c.Request)}
	for _, p := range c.Pieces {
		sp := SentPiece{Hash: p.Hash, Kind: p.Kind, Label: p.Label}
		if !have(p.Hash) {
			sp.Bytes = []byte(p.JSON)
		}
		out.Pieces = append(out.Pieces, sp)
	}
	return out
}

// context is c as the store takes it.
func (c SentCall) context() store.CallContext {
	out := store.CallContext{Exchange: c.Exchange, Session: c.Session, Vendor: c.Vendor, Time: c.Time, Size: c.Size,
		Request: json.RawMessage(c.Top), Pieces: make([]store.Piece, len(c.Pieces))}
	for i, p := range c.Pieces {
		out.Pieces[i] = store.Piece{Hash: p.Hash, Kind: p.Kind, Label: p.Label}
		if p.Bytes != nil {
			out.Pieces[i].JSON = json.RawMessage(p.Bytes)
		}
	}
	return out
}

// ReplyRecord is one kept reply (body.Reply), its message as base64.
type ReplyRecord struct {
	Exchange string    `json:"exchange"`
	Status   int       `json:"status"`
	Stop     string    `json:"stop"`
	Time     time.Time `json:"time"`
	Message  []byte    `json:"message"`
}

// CallAck answers a CallBatch: how many calls were stored, and the piece
// hashes control lacks, whose calls were not stored: send those calls again
// with these pieces' bytes.
type CallAck struct {
	Stored  int      `json:"stored"`
	Missing []string `json:"missing"`
}

func (s *Server) calls(w http.ResponseWriter, r *http.Request, rn queue.Runner) {
	if s.cfg.Record == nil {
		fail(w, http.StatusNotFound, "this service keeps no record")
		return
	}
	var b CallBatch
	if !decode(r, &b) || len(b.Calls)+len(b.Replies) > MaxBatch {
		fail(w, http.StatusBadRequest, "the body must be one JSON object with number and at most 500 calls and replies")
		return
	}
	j, err := s.cfg.Queue.Current(rn.ID, r.PathValue("id"), b.Number)
	if err != nil {
		s.queueError(w, err)
		return
	}
	// The same rule as the events: every call is of the session control
	// assigned the job.
	assigned := assignedSession(j)
	for _, c := range b.Calls {
		if c.Exchange == "" || assigned == "" || c.Session != assigned {
			fail(w, http.StatusBadRequest, "every call needs an exchange and must be of the session control assigned the attempt's job")
			return
		}
	}
	// And of the job's turn: a call or reply whose exchange an event of
	// another turn of the session carries is that turn's, and is not stored.
	other := func(exchange string) (bool, error) { return s.cfg.Record.OtherTurns(exchange, assigned, j.Turn) }
	ack := CallAck{Missing: []string{}}
	for _, c := range b.Calls {
		if o, err := other(c.Exchange); err != nil {
			s.internal(w)
			return
		} else if o {
			fail(w, http.StatusBadRequest, "every call must be of the attempt's own turn")
			return
		}
		missing, err := s.cfg.Record.KeepSent(c.context())
		if err != nil {
			fail(w, http.StatusBadRequest, "a call could not be stored: its pieces must match their hashes")
			return
		}
		if len(missing) > 0 {
			ack.Missing = append(ack.Missing, missing...)
			continue
		}
		ack.Stored++
	}
	for _, rp := range b.Replies {
		// A reply belongs to a call already held for this session.
		if sess, found, err := s.cfg.Record.CallSession(rp.Exchange); err != nil || !found || sess != assigned {
			continue
		}
		if o, err := other(rp.Exchange); err != nil || o {
			continue
		}
		if err := s.cfg.Record.KeepReply(body.Reply{Exchange: rp.Exchange, Status: rp.Status, Stop: rp.Stop, Time: rp.Time, Message: json.RawMessage(rp.Message)}); err != nil {
			s.internal(w)
			return
		}
	}
	writeJSON(w, http.StatusOK, ack)
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
	j, err := s.cfg.Queue.Current(rn.ID, r.PathValue("id"), b.Number)
	if err != nil {
		s.queueError(w, err)
		return
	}
	turn := j.Turn
	evs := make([]event.Event, 0, len(b.Events))
	for _, raw := range b.Events {
		var e event.Event
		if json.Unmarshal(raw, &e) != nil || e.ID == "" || e.Kind == "" || len(e.Kind) > 64 || turn == "" || e.Turn != turn || e.Session == "" {
			fail(w, http.StatusBadRequest, "every event needs an id, a kind, a session, and the job's turn as turn")
			return
		}
		// A drop marks other sessions' events as missing: it is the
		// store's own, never a turn's.
		if e.Kind == event.EventsDropped {
			fail(w, http.StatusBadRequest, "events.dropped is not a turn's event")
			return
		}
		// What a turn changed and what was applied are control's own
		// record of a job's end (outcome.go), never a runner's.
		if e.Kind == event.TurnChanged || e.Kind == event.TurnApplied {
			fail(w, http.StatusBadRequest, string(e.Kind)+" is control's own event, not a runner's")
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
	assigned := assignedSession(j)
	for _, e := range evs {
		if assigned == "" || e.Session != assigned {
			fail(w, http.StatusBadRequest, "every event must be of the session control assigned the attempt's job")
			return
		}
	}
	n := 0
	for _, e := range evs {
		// The turn's end is stored after the calls the relay carried for it
		// are: a reader that sees the end sees all of them. One the relay
		// could not confirm leaves the turn marked unsettled, as the runner
		// marks one its own gateway could not.
		if e.Kind == event.SessionClosed && e.Turn != "" && !s.settle(e.Session, e.Turn) {
			e.Unsettled = true
		}
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

// CloseLost stores session.closed, unsettled, for each hold the record has
// open whose turn's attempt lease ran out. Its runner stopped
// during the turn, so nothing else closes it: the cockpit would show the
// turn running, and the session would take no next turn. It is what
// store.CloseStaleHolds does at start for this process's own turns.
func CloseLost(q *queue.Queue, st *store.Store, now time.Time) (int, error) {
	holds, err := st.OpenHolds(st.Last())
	if err != nil || len(holds) == 0 {
		return 0, err
	}
	var turns []string
	for _, h := range holds {
		if h.Turn != "" {
			turns = append(turns, h.Turn)
		}
	}
	lost, err := q.Lost(turns)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range holds {
		if !lost[h.Turn] {
			continue
		}
		if _, _, err := st.Append(event.Event{ID: queue.NewID("evt_"), Kind: event.SessionClosed, Time: now,
			Session: h.Session, Turn: h.Turn, Hold: h.Hold, Unsettled: true}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
