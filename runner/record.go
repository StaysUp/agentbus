package runner

// A runner's turn in control's record, while it runs (Config.Forward): the
// turn's events from the runner's own record, and its model calls with their
// pieces and replies, so a cockpit on control shows the turn live, and an
// agent's request tree is known to control when the agent asks another. The
// same is sent once more when the turn ends; what control holds already is
// not stored twice.

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

// callSize is about how many bytes sc takes in a calls batch, at least:
// its bodies as base64 (a third more than their bytes), and each piece's
// own fields and JSON around them.
func callSize(sc jobs.SentCall) int {
	n := len(sc.Top)*4/3 + len(sc.Exchange) + len(sc.Session) + len(sc.Vendor) + 128
	for _, p := range sc.Pieces {
		n += len(p.Bytes)*4/3 + len(p.Hash) + len(p.Kind) + len(p.Label) + 48
	}
	return n
}

// callBudget bounds one calls batch's callSize, under the protocol's body
// limit with room for what the estimate leaves out (JSON escapes).
const callBudget = jobs.MaxBody - 128<<10

// LiveEvery is how often a running turn's new record goes to control, at
// most.
const LiveEvery = 300 * time.Millisecond

// sent is what of an attempt's record control holds.
type sent struct {
	after     int64           // the runner's seq of the last event sent
	exchanges []string        // the turn's calls, in order, from its events
	known     map[string]bool // exchanges listed in exchanges
	calls     map[string]bool // calls control stored
	replies   map[string]bool // replies sent
	pieces    map[string]bool // piece hashes control holds
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

// forward sends what control lacks of the turn's record in session: its
// events, a batch at a time, then its calls and replies. A batch that cannot
// be sent waits for the next time; at the end (final) each is tried a few
// times, then given up: the job still finishes, and the turn stays in the
// runner's record.
func (at *attempt) forward(ctx context.Context, session string, final bool) {
	if !at.r.cfg.Forward || at.r.cfg.Store == nil || session == "" {
		return
	}
	at.fwdMu.Lock()
	defer at.fwdMu.Unlock()
	s := &at.sent
	if s.known == nil {
		s.known, s.calls, s.replies, s.pieces = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	}
	tries := 1
	if final {
		tries = 3
	}
	filter := store.Filter{Turn: at.a.ID, TurnSession: session}
	for range 10000 {
		recs, more, err := at.r.cfg.Store.Page(s.after, filter, jobs.MaxBatch, MaxRecordBatch)
		if err != nil || len(recs) == 0 {
			break
		}
		b := jobs.RecordBatch{Number: at.a.Number}
		size, last, taken := 0, s.after, 0
		var exch []string
		for _, rec := range recs {
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
			if rec.Kind == event.ModelRequested || rec.Kind == event.ModelResponded {
				var e struct {
					Exchange string `json:"exchange"`
				}
				if json.Unmarshal(rec.Data, &e) == nil && e.Exchange != "" {
					exch = append(exch, e.Exchange)
				}
			}
		}
		if len(b.Events) > 0 && !at.send(ctx, "record", b, &jobs.RecordAck{}, tries) {
			at.r.cfg.Logf("job %s: the turn's record could not be sent", at.job.ID)
			return
		}
		s.after = last
		for _, x := range exch {
			if !s.known[x] {
				s.known[x] = true
				s.exchanges = append(s.exchanges, x)
			}
		}
		if !more && taken == len(recs) {
			break
		}
	}
	// A call refused for pieces control lacked goes again with them.
	for range 2 {
		if !at.forwardCalls(ctx, session, tries) {
			return
		}
	}
}

// forwardCalls sends the turn's calls control lacks, each with the bytes of
// the pieces control may lack, then the replies of the calls control holds.
// again says a call was refused for pieces control lacked: sent again, it
// carries them.
func (at *attempt) forwardCalls(ctx context.Context, session string, tries int) (again bool) {
	s := &at.sent
	have := func(h string) bool { return s.pieces[h] }
	var batch []jobs.SentCall
	size := 0
	sendCalls := func() bool {
		if len(batch) == 0 {
			return true
		}
		var ack jobs.CallAck
		if !at.send(ctx, "calls", jobs.CallBatch{Number: at.a.Number, Calls: batch, Replies: []jobs.ReplyRecord{}}, &ack, tries) {
			return false
		}
		missing := map[string]bool{}
		for _, h := range ack.Missing {
			missing[h] = true
		}
		for _, c := range batch {
			stored := true
			for _, p := range c.Pieces {
				if missing[p.Hash] {
					stored = false
					delete(s.pieces, p.Hash) // its bytes go with the next send
				} else {
					s.pieces[p.Hash] = true
				}
			}
			s.calls[c.Exchange] = stored
			again = again || !stored
		}
		batch, size = nil, 0
		return true
	}
	for _, x := range s.exchanges {
		if s.calls[x] {
			continue
		}
		c, found, err := at.r.cfg.Store.CallContext(x, true)
		if err != nil || !found || c.Session != session {
			continue
		}
		sc := jobs.SentCallOf(c, have)
		n := callSize(sc)
		if n > callBudget {
			at.r.cfg.Logf("job %s: a model call of the turn is too big to send, left out", at.job.ID)
			s.calls[x] = true
			continue
		}
		if (size+n > callBudget || len(batch) >= jobs.MaxBatch) && !sendCalls() {
			return false
		}
		batch = append(batch, sc)
		size += n
	}
	if !sendCalls() {
		return again
	}
	// The replies of the calls control holds now.
	var replies []jobs.ReplyRecord
	size = 0
	sendReplies := func() bool {
		if len(replies) == 0 {
			return true
		}
		if !at.send(ctx, "calls", jobs.CallBatch{Number: at.a.Number, Calls: []jobs.SentCall{}, Replies: replies}, &jobs.CallAck{}, tries) {
			return false
		}
		for _, rp := range replies {
			s.replies[rp.Exchange] = true
		}
		replies, size = nil, 0
		return true
	}
	for _, x := range s.exchanges {
		if !s.calls[x] || s.replies[x] {
			continue
		}
		rp, found, err := at.r.cfg.Store.Reply(x)
		if err != nil || !found {
			continue
		}
		n := len(rp.Message) * 4 / 3
		if n > callBudget {
			s.replies[x] = true
			continue
		}
		if (size+n > callBudget || len(replies) >= jobs.MaxBatch) && !sendReplies() {
			return again
		}
		replies = append(replies, jobs.ReplyRecord{Exchange: rp.Exchange, Status: rp.Status, Stop: rp.Stop, Time: rp.Time, Message: []byte(rp.Message)})
		size += n
	}
	sendReplies()
	return again
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
