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
// and its history steps (history.shortened, history.compacted), the notes it
// added (context.added), the guard's, and a call the runner's gateway refused on its own, before it
// reached control's relay (refuseCall, for an unrecorded path or a bad header:
// no credential is added and the call is sent nowhere, so the relay never sees
// it). Every other event of a relayed call control records itself.
var relayOnly = map[event.Kind]bool{event.ReplayFailed: true, event.HistoryShortened: true, event.HistoryCompacted: true, event.ToolRefused: true,
	event.ApprovalRequested: true, event.ApprovalDecided: true, event.CallRefused: true,
	// A note own replay added to the call (replay/notes.go): control's
	// relay sees the call with it, but only this gateway knows it was a note.
	event.ContextAdded: true}

// relayedCall is true for a call's event that control's relay records itself, so
// a runner that holds no secret leaves it out of what it writes to control's
// record: it carries an exchange and is not one of the few only a gateway makes
// (relayOnly). Every runner forwards by it (forward, below), agentbus up's
// built-in one too, a process of its own since 0.6.7.
func relayedCall(kind event.Kind, hasExchange bool) bool {
	return hasExchange && !relayOnly[kind]
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
// runner writes control's own record), and whether control answered every
// batch it was sent: taken, or refused, which control decided. It sends a batch at
// a time, from the runner's own record. A batch that cannot be sent waits for
// the next time; at the end (final) each is tried while the attempt is live,
// as the finish is (untilAnswered), so a turn that ended while control was
// away is whole in its record once it is back. A runner
// holds no secret, so each of its model calls went through control's relay,
// which recorded the call from its own copy; the runner sends only the events
// the relay cannot (relayedCall), never the calls themselves.
func (at *attempt) forward(ctx context.Context, session string, final bool) (took, answered bool) {
	if !at.r.cfg.Forward || at.r.cfg.Store == nil || session == "" {
		return true, true
	}
	at.fwdMu.Lock()
	defer at.fwdMu.Unlock()
	s := &at.sent
	filter := store.Filter{Turn: at.job.Turn, TurnSession: session}
	for range 10000 {
		recs, more, err := at.r.cfg.Store.Page(s.after, filter, jobs.MaxBatch, MaxRecordBatch)
		if err != nil {
			return false, true // the runner's own record: nothing to ask control
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
		if len(b.Events) > 0 {
			if took, answered := at.send(ctx, "record", b, &jobs.RecordAck{}, final); !took {
				at.r.cfg.Logf("job %s: the turn's record could not be sent", at.job.ID)
				return false, answered
			}
		}
		s.after = last
		if !more && taken == len(recs) {
			break
		}
	}
	return true, true
}

// hasExchange says whether an event's data names a model call's exchange.
func hasExchange(data []byte) bool {
	var e struct {
		Exchange string `json:"exchange"`
	}
	return json.Unmarshal(data, &e) == nil && e.Exchange != ""
}

// send posts body to the attempt's op: once, or when final until control
// answers while the attempt is control's (untilAnswered). It says whether
// control took it, and whether control answered at all: a refusal is an
// answer (control decided), no answer is not.
//
// A 5xx answer is tried recordTries times, then counted as an answer: the
// record is given up, the finish goes, and control closes the run whose end
// it did not get (jobs.closeOpenRuns). That is Buildkite's rule for a job's
// log chunks (agent/run_job.go: failed chunks are counted, the finish is not
// held for them). Only no answer at all (control away, as in a restart) is
// waited out like the finish.
// recordTries is how many 5xx answers a final record batch takes before it
// is given up (send).
const recordTries = 3

func (at *attempt) send(ctx context.Context, op string, body, out any, final bool) (took, answered bool) {
	fives := 0
	try := func() bool {
		status, err := at.post(ctx, op, body, out, nil)
		took = err == nil && status == http.StatusOK
		answered = err == nil && status < 500
		if err == nil && status >= 500 {
			if fives++; fives >= recordTries {
				answered = true
			}
		}
		return answered
	}
	if final {
		untilAnswered(ctx, at.r.cfg.Retire, at.gone, try)
	} else {
		try()
	}
	return took, answered
}

// sessionOf is the turn's session at its end: the result's, or the one it
// started in.
func (at *attempt) sessionOf(f queue.Finish) string {
	if res, _ := queue.ReadResult(f.Result); res.Session != "" {
		return res.Session
	}
	return at.session
}
