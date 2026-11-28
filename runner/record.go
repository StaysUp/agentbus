package runner

// A runner's turn in control's record, while it runs (Config.Forward): the
// turn's events from the runner's own record, and its model calls with their
// pieces and replies, so a cockpit on control shows the turn live, and an
// agent's request tree is known to control when the agent asks another. The
// same is sent once more when the turn ends; what control holds already is
// not stored twice.
//
// A runner that holds no secret (Config.NoSecrets) sends none of its model
// calls: each went through control's relay, which recorded it from its own
// copy (jobs/relay.go). It sends what only it sees: the turn's own events,
// and of a call's events only those its gateway made itself (relayOnly). A
// call that never reached control reached no vendor: it stays in the
// runner's own record and log.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// relayOnly are the kinds of a call's events (those with an exchange) that
// the runner's gateway makes and control's relay cannot: own replay's refusal
// and its history steps (history.shortened, history.compacted), the guard's, and a call the runner's gateway refused on its own, before it
// reached control's relay (refuseCall, for an unrecorded path or a bad header:
// no credential is added and the call is sent nowhere, so the relay never sees
// it). Every other event of a relayed call control records itself.
var relayOnly = map[event.Kind]bool{event.ReplayFailed: true, event.HistoryShortened: true, event.HistoryCompacted: true, event.ToolRefused: true,
	event.ApprovalRequested: true, event.ApprovalDecided: true, event.CallRefused: true}

// relayedCall is true for a call's event that control's relay records itself, so
// a runner that holds no secret leaves it out of what it writes to control's
// record: it carries an exchange and is not one of the few only a gateway makes
// (relayOnly). The forwarding (forward, below) and agentbus up's built-in-runner
// sink (SuppressRelayed) share this one rule, so it cannot drift between them.
func relayedCall(kind event.Kind, hasExchange bool) bool {
	return hasExchange && !relayOnly[kind]
}

// SuppressRelayed wraps next so it drops the call events control's relay records
// (relayedCall), keeping only the few a gateway alone makes. agentbus up's
// built-in runner shares control's record with the relay; without this, both
// would record each call. A runner on another machine does the same by not
// forwarding those events.
func SuppressRelayed(next event.Sink) event.Sink { return relaySuppress{next} }

type relaySuppress struct{ next event.Sink }

func (r relaySuppress) Emit(e event.Event) {
	if relayedCall(e.Kind, e.Exchange != "") {
		return
	}
	r.next.Emit(e)
}

// LiveEvery is how often a running turn's new record goes to control, at
// most.
const LiveEvery = 300 * time.Millisecond

// sent is what of an attempt's record control holds.
type sent struct {
	after int64 // the runner's seq of the last event sent
}

// live sends the turn's record as it grows, until done closes.
func (at *attempt) live(ctx context.Context, done <-chan struct{}) {
	if !at.r.cfg.Forward || at.r.cfg.Store == nil {
		return
	}
	ch, cancel := at.r.cfg.Store.Notify()
	defer cancel()
	// What the turn's start wrote goes at once: it was written before this
	// listened.
	at.forward(ctx, at.session, false)
	for {
		select {
		case <-done:
			return
		case <-ch:
		}
		at.forward(ctx, at.session, false)
		select {
		case <-done:
			return
		case <-time.After(LiveEvery):
		}
	}
}

// forward sends what control lacks of the turn's events in session, and says
// whether control holds them all now (true when nothing is forwarded: the
// runner writes control's own record). It sends a batch at
// a time, from the runner's own record. A batch that cannot be sent waits for
// the next time; at the end (final) each is tried a few times, then given up:
// the job still finishes, and the turn stays in the runner's record. A runner
// holds no secret, so each of its model calls went through control's relay,
// which recorded the call from its own copy; the runner sends only the events
// the relay cannot (relayedCall), never the calls themselves.
func (at *attempt) forward(ctx context.Context, session string, final bool) bool {
	if !at.r.cfg.Forward || at.r.cfg.Store == nil || session == "" {
		return true
	}
	at.fwdMu.Lock()
	defer at.fwdMu.Unlock()
	s := &at.sent
	tries := 1
	if final {
		tries = 3
	}
	filter := store.Filter{Turn: at.job.Turn, TurnSession: session}
	for range 10000 {
		recs, more, err := at.r.cfg.Store.Page(s.after, filter, jobs.MaxBatch, MaxRecordBatch)
		if err != nil {
			return false
		}
		if len(recs) == 0 {
			break
		}
		b := jobs.RecordBatch{Number: at.a.Number}
		size, last, taken := 0, s.after, 0
		for _, rec := range recs {
			if at.r.cfg.NoSecrets && relayedCall(rec.Kind, hasExchange(rec.Data)) {
				// The relay recorded it.
				last, taken = rec.Seq, taken+1
				continue
			}
			if len(rec.Data) > MaxRecordBatch {
				// Bigger than a batch may be (a tool's whole output): left
				// out, so the rest of the turn still goes.
				at.r.cfg.Logf("job %s: an event of the turn is too big to send, left out", at.job.ID)
				last, taken = rec.Seq, taken+1
				continue
			}
			if size+len(rec.Data) > MaxRecordBatch {
				break
			}
			b.Events = append(b.Events, rec.Data)
			size += len(rec.Data)
			last, taken = rec.Seq, taken+1
		}
		if len(b.Events) > 0 && !at.send(ctx, "record", b, &jobs.RecordAck{}, tries) {
			at.r.cfg.Logf("job %s: the turn's record could not be sent", at.job.ID)
			return false
		}
		s.after = last
		if !more && taken == len(recs) {
			break
		}
	}
	return true
}

// hasExchange says whether an event's data names a model call's exchange.
func hasExchange(data []byte) bool {
	var e struct {
		Exchange string `json:"exchange"`
	}
	return json.Unmarshal(data, &e) == nil && e.Exchange != ""
}

// send posts body to the attempt's op, tries times at most while control
// answers 5xx or is out of reach.
func (at *attempt) send(ctx context.Context, op string, body, out any, tries int) bool {
	for i := range tries {
		status, err := call(ctx, at.r.short, at.path(op), at.r.token, body, out)
		if err == nil && status == http.StatusOK {
			return true
		}
		if err == nil && status < 500 {
			return false
		}
		if i < tries-1 {
			sleep(ctx, time.Duration(i+1)*time.Second)
		}
	}
	return false
}

// sessionOf is the turn's session at its end: the result's, or the one it
// started in.
func (at *attempt) sessionOf(f queue.Finish) string {
	if res, _ := queue.ReadResult(f.Result); res.Session != "" {
		return res.Session
	}
	return at.session
}
