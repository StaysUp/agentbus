package gateway

import (
	"sync"
	"time"

	"github.com/staysup/agentbus/body"
)

// The bounds of the bodies waiting for a slow keeper. Past either, a body is
// not kept and is counted as dropped: its call then has no stored context, and
// its events are unaffected. Each entry counts bodyEntryOverhead bytes beyond
// its body, so empty bodies still fill the byte bound, and bodyQueueEntries
// caps the count on its own (empty bodies queued 100,000 entries with zero
// bytes counted).
const (
	bodyQueueLimit    = 64 << 20
	bodyQueueEntries  = 1024
	bodyEntryOverhead = 512
)

// KeepBodies makes the gateway hand every model request's body to k, split
// into pieces by the adapter, when the adapter is a body.Splitter. When k is
// also a body.ReplyKeeper and the adapter a body.ReplyBuilder, every reply
// the adapter can rebuild whole is handed to k too, except a probe's. Headers
// are never handed on. Call it before serving. Splitting, rebuilding and
// keeping happen off the request's path, so a slow keeper never holds up an
// agent.
func (g *Gateway) KeepBodies(k body.Keeper) {
	s, ok := g.adapter.(body.Splitter)
	if !ok {
		return
	}
	g.bodies = newBodyQueue(s, k, bodyQueueLimit, bodyQueueEntries)
	if rb, ok := g.adapter.(body.ReplyBuilder); ok {
		if rk, ok := k.(body.ReplyKeeper); ok {
			g.bodies.build, g.bodies.keepReply = rb, rk
		}
	}
}

// BodiesDropped is how many request bodies were not kept because the queue
// was full.
func (g *Gateway) BodiesDropped() int {
	if g.bodies == nil {
		return 0
	}
	g.bodies.mu.Lock()
	defer g.bodies.mu.Unlock()
	return g.bodies.dropped
}

func newBodyQueue(s body.Splitter, k body.Keeper, limit, entries int) *bodyQueue {
	q := &bodyQueue{split: s, keep: k, limit: limit, entries: entries, done: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	go q.run()
	return q
}

// keepBody queues a request body, already decoded, for splitting and keeping.
func (g *Gateway) keepBody(ex *exchange, plain []byte) {
	if g.bodies == nil {
		return
	}
	g.bodies.push(pendingBody{exchange: ex.id, session: ex.session, vendor: g.adapter.Vendor(),
		time: ex.start, data: plain})
}

// keepReply queues a reply body, already decoded, for rebuilding and keeping.
func (g *Gateway) keepReply(ex *exchange, status int, contentType string, plain []byte) {
	if g.bodies == nil || g.bodies.keepReply == nil || ex.probe {
		return
	}
	g.bodies.push(pendingBody{exchange: ex.id, session: ex.session, vendor: g.adapter.Vendor(),
		time: g.now(), data: plain, reply: true, status: status, contentType: contentType})
}

type pendingBody struct {
	exchange, session, vendor string
	time                      time.Time
	data                      []byte
	// barrier, when set, is called in order instead of keeping anything.
	barrier func()
	// A reply, not a request: its status and content type.
	reply       bool
	status      int
	contentType string
}

// bodyQueue splits and keeps bodies from one goroutine, in order.
type bodyQueue struct {
	split     body.Splitter
	keep      body.Keeper
	build     body.ReplyBuilder // nil: replies are not kept
	keepReply body.ReplyKeeper
	limit     int // bytes, each entry counted as bodyEntryOverhead plus its body
	entries   int // entries waiting

	mu      sync.Mutex
	cond    *sync.Cond
	pending []pendingBody
	bytes   int
	dropped int
	closed  bool
	done    chan struct{}
}

func (q *bodyQueue) push(b pendingBody) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	n := entrySize(b)
	if q.bytes+n > q.limit || len(q.pending) >= q.entries {
		q.dropped++
		return
	}
	q.pending = append(q.pending, b)
	q.bytes += n
	q.cond.Signal()
}

func (q *bodyQueue) run() {
	defer close(q.done)
	for {
		q.mu.Lock()
		for len(q.pending) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.pending) == 0 {
			q.mu.Unlock()
			return
		}
		b := q.pending[0]
		q.pending[0] = pendingBody{}
		q.pending = q.pending[1:]
		q.mu.Unlock()
		q.one(b)
		q.mu.Lock()
		q.bytes -= entrySize(b)
		q.mu.Unlock()
	}
}

// one splits and keeps one body. An adapter that fails or panics on it, or a
// keeper that fails, loses that call's context and nothing else.
func (q *bodyQueue) one(b pendingBody) {
	if b.barrier != nil {
		b.barrier()
		return
	}
	defer func() { _ = recover() }()
	if b.reply {
		msg, stop, err := q.build.Build(b.status, b.contentType, b.data)
		if err != nil {
			return
		}
		_ = q.keepReply.KeepReply(body.Reply{Exchange: b.exchange, Status: b.status, Stop: stop,
			Time: b.time, Message: msg})
		return
	}
	req, err := q.split.Split(b.data)
	if err != nil {
		return
	}
	_ = q.keep.KeepCall(body.Call{Exchange: b.exchange, Session: b.session, Vendor: b.vendor,
		Time: b.time, Size: len(b.data), Request: req})
}

// barrier calls f once every body queued before it has been kept, or
// dropped. It counts against the bounds like a body; past either it is not
// queued, f is never called, and barrier reports false.
func (q *bodyQueue) barrier(f func()) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		f()
		return true
	}
	// Bounded like a body: one that would pass either bound is not queued.
	if n := entrySize(pendingBody{}); q.bytes+n > q.limit || len(q.pending) >= q.entries {
		return false
	}
	q.pending = append(q.pending, pendingBody{barrier: f})
	q.bytes += entrySize(pendingBody{})
	q.cond.Signal()
	return true
}

// close keeps every body already queued, then stops.
func (q *bodyQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Signal()
	q.mu.Unlock()
	<-q.done
}

// entrySize is what one queued body counts against the byte bound.
func entrySize(b pendingBody) int { return bodyEntryOverhead + len(b.data) }
