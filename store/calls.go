package store

// Model calls' request bodies, split into pieces. Each piece is kept once in
// pieces, under the SHA-256 of its bytes; a call keeps its other top-level
// fields and the ordered list of its pieces' hashes. A session sends the same
// system prompt and tools on every call, and each call repeats the
// conversation so far, so most of a call's pieces are already stored.
//
// Only bodies are kept, never headers. A reply is kept as the message the
// adapter rebuilt from it (body.Reply), as one piece, under the call's
// exchange in replies: a turn's last reply comes back in no later request of
// that turn, and replaying the turn needs it exactly.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/staysup/agentbus/body"
)

const callsSchema = `
CREATE TABLE IF NOT EXISTS pieces (
	hash    TEXT    PRIMARY KEY,
	bytes   BLOB    NOT NULL,
	size    INTEGER NOT NULL,
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS calls (
	exchange TEXT    PRIMARY KEY,
	session  TEXT    NOT NULL,
	vendor   TEXT    NOT NULL,
	time     INTEGER NOT NULL,
	size     INTEGER NOT NULL,
	top      TEXT    NOT NULL,
	pieces   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS calls_session ON calls(session, time);
CREATE TABLE IF NOT EXISTS replies (
	exchange TEXT    PRIMARY KEY,
	hash     TEXT    NOT NULL,
	status   INTEGER NOT NULL,
	stop     TEXT    NOT NULL,
	time     INTEGER NOT NULL
);
`

// pieceRef is one entry of a call's piece list.
type pieceRef struct {
	Hash  string `json:"h"`
	Kind  string `json:"k"`
	Label string `json:"l,omitempty"`
}

// KeepCall stores a call's top-level fields and piece list, and each piece
// not stored yet. A second call with the same exchange is ignored.
func (s *Store) KeepCall(c body.Call) error {
	refs := make([]pieceRef, len(c.Pieces))
	for i, p := range c.Pieces {
		refs[i] = pieceRef{Hash: p.Hash(), Kind: p.Kind, Label: p.Label}
	}
	list, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("store: encode a call's pieces: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	tx, err := s.w.Begin()
	if err != nil {
		return fmt.Errorf("store: write a call: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ins, err := tx.Prepare(`INSERT INTO pieces(hash, bytes, size, created) VALUES(?, ?, ?, ?)
		ON CONFLICT(hash) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("store: write a call: %w", err)
	}
	defer ins.Close()
	now := c.Time.UnixNano()
	for i, p := range c.Pieces {
		if _, err := ins.Exec(refs[i].Hash, []byte(p.JSON), len(p.JSON), now); err != nil {
			return fmt.Errorf("store: write a piece: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO calls(exchange, session, vendor, time, size, top, pieces)
		VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(exchange) DO NOTHING`,
		c.Exchange, c.Session, c.Vendor, now, c.Size, string(c.Top), string(list)); err != nil {
		return fmt.Errorf("store: write a call: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: write a call: %w", err)
	}
	return nil
}

// KeepReply stores a call's rebuilt reply: the message as one piece, and the
// row naming it. A second reply for the same exchange is ignored.
func (s *Store) KeepReply(r body.Reply) error {
	p := body.Piece{Kind: body.Message, Label: "assistant", JSON: r.Message}
	h := p.Hash()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	tx, err := s.w.Begin()
	if err != nil {
		return fmt.Errorf("store: write a reply: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := r.Time.UnixNano()
	if _, err := tx.Exec(`INSERT INTO pieces(hash, bytes, size, created) VALUES(?, ?, ?, ?)
		ON CONFLICT(hash) DO NOTHING`, h, []byte(r.Message), len(r.Message), now); err != nil {
		return fmt.Errorf("store: write a piece: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO replies(exchange, hash, status, stop, time) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(exchange) DO NOTHING`, r.Exchange, h, r.Status, r.Stop, now); err != nil {
		return fmt.Errorf("store: write a reply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: write a reply: %w", err)
	}
	return nil
}

// Reply reads a call's kept reply. found is false when none is kept.
func (s *Store) Reply(exchange string) (r body.Reply, found bool, err error) {
	var h string
	var t int64
	err = s.r.QueryRow(`SELECT hash, status, stop, time FROM replies WHERE exchange = ?`, exchange).
		Scan(&h, &r.Status, &r.Stop, &t)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("store: read a reply: %w", err)
	}
	var b []byte
	if err := s.r.QueryRow(`SELECT bytes FROM pieces WHERE hash = ?`, h).Scan(&b); err != nil {
		return r, false, fmt.Errorf("store: read a piece: %w", err)
	}
	r.Exchange, r.Time, r.Message = exchange, time.Unix(0, t).UTC(), json.RawMessage(b)
	return r, true, nil
}

// CallContext is a stored call's request: its top-level fields and its pieces
// in the order the request carried them.
type CallContext struct {
	Exchange string    `json:"exchange" doc:"The call: the exchange field of its events."`
	Session  string    `json:"session"`
	Vendor   string    `json:"vendor" doc:"The API format: anthropic or openai."`
	Time     time.Time `json:"time" doc:"When the gateway received the request."`
	Size     int       `json:"size" doc:"The request body's length in bytes, decoded."`
	// StopReason and Reply: the reply the adapter rebuilt, when one is kept.
	StopReason string          `json:"stop_reason,omitempty" doc:"The vendor's stop reason of the kept reply."`
	Reply      *Piece          `json:"reply,omitempty" doc:"The model's reply as the harness sends it back next, rebuilt from the stream. Anthropic: the assistant message, every block in full, thinking signatures included. OpenAI (codex): the list of response.completed's output items, each as the vendor sent it, encrypted reasoning included. Absent when none is kept: a reply that failed, was cut off, or came from an adapter that does not rebuild replies."`
	Request    json.RawMessage `json:"request" doc:"Every other top-level field of the request, such as model and max_tokens, in the request's order, values unchanged."`
	Pieces     []Piece         `json:"pieces" doc:"The pieces in the request's order."`
}

// Piece is one piece of a call.
type Piece struct {
	Kind         string          `json:"kind" doc:"system, tool, message (Anthropic), instructions, tool, input_item (OpenAI Responses)."`
	Label        string          `json:"label,omitempty" doc:"A tool's name, a message's role, an input item's role or type, a system block's type."`
	Hash         string          `json:"hash" doc:"The SHA-256 of the piece's bytes, in hex. The same piece in two calls has one hash and is stored once."`
	Size         int             `json:"size" doc:"The piece's length in bytes, as the request carried it."`
	ApproxTokens int             `json:"approx_tokens" doc:"A rough token count: size / 2.66, measured on one Claude Code request; off by up to 15%. Not the vendor's tokenizer."`
	JSON         json.RawMessage `json:"json,omitempty" doc:"The piece, byte for byte as the request carried it: not re-encoded, not compacted, not escaped, so its SHA-256 is hash and its length is size. Read it as raw text to check that. Absent with full=false."`
}

// CallContext reads a stored call. found is false when no request body is
// stored for exchange. With full false, the pieces carry no JSON.
func (s *Store) CallContext(exchange string, full bool) (c CallContext, found bool, err error) {
	var t int64
	var top, list string
	err = s.r.QueryRow(`SELECT session, vendor, time, size, top, pieces FROM calls WHERE exchange = ?`, exchange).
		Scan(&c.Session, &c.Vendor, &t, &c.Size, &top, &list)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("store: read a call: %w", err)
	}
	var refs []pieceRef
	if err := json.Unmarshal([]byte(list), &refs); err != nil {
		return c, false, fmt.Errorf("store: read a call's pieces: %w", err)
	}
	c.Exchange, c.Time, c.Request = exchange, time.Unix(0, t).UTC(), json.RawMessage(top)
	q := `SELECT size, NULL FROM pieces WHERE hash = ?`
	if full {
		q = `SELECT size, bytes FROM pieces WHERE hash = ?`
	}
	type got struct {
		size int
		data []byte
	}
	seen := map[string]got{}
	c.Pieces = make([]Piece, len(refs))
	for i, r := range refs {
		g, ok := seen[r.Hash]
		if !ok {
			if err := s.r.QueryRow(q, r.Hash).Scan(&g.size, &g.data); err != nil {
				return c, false, fmt.Errorf("store: read a piece: %w", err)
			}
			seen[r.Hash] = g
		}
		c.Pieces[i] = Piece{Kind: r.Kind, Label: r.Label, Hash: r.Hash, Size: g.size,
			ApproxTokens: body.ApproxTokens(g.size), JSON: g.data}
	}
	rep, ok, err := s.Reply(exchange)
	if err != nil {
		return c, false, err
	}
	if ok {
		p := Piece{Kind: body.Message, Label: "assistant", Hash: body.Piece{JSON: rep.Message}.Hash(),
			Size: len(rep.Message), ApproxTokens: body.ApproxTokens(len(rep.Message))}
		if full {
			p.JSON = rep.Message
		}
		c.Reply, c.StopReason = &p, rep.Stop
	}
	return c, true, nil
}

// CallSession is the session a stored call belongs to. found is false when no
// request body is stored for exchange.
func (s *Store) CallSession(exchange string) (session string, found bool, err error) {
	err = s.r.QueryRow(`SELECT session FROM calls WHERE exchange = ?`, exchange).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read a call: %w", err)
	}
	return session, true, nil
}

// Why CallPiece found nothing.
var (
	ErrNoCall  = errors.New("store: no request body is stored for this call")
	ErrNoPiece = errors.New("store: the call has no such piece")
	ErrNoReply = errors.New("store: no reply is kept for this call")
)

// CallPiece reads one piece of a stored call, with its bytes: piece n of the
// request, or with reply the kept reply (n is then ignored). It returns
// ErrNoCall, ErrNoPiece or ErrNoReply when there is none, and count, the
// request's number of pieces, whenever the call is found.
func (s *Store) CallPiece(exchange string, n int, reply bool) (p Piece, count int, err error) {
	var list string
	err = s.r.QueryRow(`SELECT pieces FROM calls WHERE exchange = ?`, exchange).Scan(&list)
	if errors.Is(err, sql.ErrNoRows) {
		return p, 0, ErrNoCall
	}
	if err != nil {
		return p, 0, fmt.Errorf("store: read a call: %w", err)
	}
	var refs []pieceRef
	if err := json.Unmarshal([]byte(list), &refs); err != nil {
		return p, 0, fmt.Errorf("store: read a call's pieces: %w", err)
	}
	count = len(refs)
	var ref pieceRef
	switch {
	case reply:
		var h string
		err := s.r.QueryRow(`SELECT hash FROM replies WHERE exchange = ?`, exchange).Scan(&h)
		if errors.Is(err, sql.ErrNoRows) {
			return p, count, ErrNoReply
		}
		if err != nil {
			return p, count, fmt.Errorf("store: read a reply: %w", err)
		}
		ref = pieceRef{Hash: h, Kind: body.Message, Label: "assistant"}
	case n < 0 || n >= len(refs):
		return p, count, ErrNoPiece
	default:
		ref = refs[n]
	}
	var b []byte
	if err := s.r.QueryRow(`SELECT bytes FROM pieces WHERE hash = ?`, ref.Hash).Scan(&b); err != nil {
		return p, count, fmt.Errorf("store: read a piece: %w", err)
	}
	if b == nil {
		b = []byte{}
	}
	return Piece{Kind: ref.Kind, Label: ref.Label, Hash: ref.Hash, Size: len(b),
		ApproxTokens: body.ApproxTokens(len(b)), JSON: b}, count, nil
}

// KeepSent stores a call another service kept, such as a runner's, as
// CallContext read it there: each piece that carries its bytes is stored
// once, after its hash is checked; a piece without them must be stored
// already. A call with pieces this store lacks is not stored, and their
// hashes are returned, for the sender to send again with their bytes. A
// second call with the same exchange is ignored.
func (s *Store) KeepSent(c CallContext) (missing []string, err error) {
	refs := make([]pieceRef, len(c.Pieces))
	for i, p := range c.Pieces {
		if p.JSON != nil && (body.Piece{JSON: p.JSON}).Hash() != p.Hash {
			return nil, errors.New("store: a piece's bytes do not match its hash")
		}
		refs[i] = pieceRef{Hash: p.Hash, Kind: p.Kind, Label: p.Label}
	}
	list, err := json.Marshal(refs)
	if err != nil {
		return nil, fmt.Errorf("store: encode a call's pieces: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	tx, err := s.w.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: write a call: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := c.Time.UnixNano()
	for _, p := range c.Pieces {
		if p.JSON != nil {
			if _, err := tx.Exec(`INSERT INTO pieces(hash, bytes, size, created) VALUES(?, ?, ?, ?) ON CONFLICT(hash) DO NOTHING`,
				p.Hash, []byte(p.JSON), len(p.JSON), now); err != nil {
				return nil, fmt.Errorf("store: write a piece: %w", err)
			}
			continue
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pieces WHERE hash = ?`, p.Hash).Scan(&n); err != nil {
			return nil, fmt.Errorf("store: read a piece: %w", err)
		}
		if n == 0 {
			missing = append(missing, p.Hash)
		}
	}
	if len(missing) > 0 {
		// The pieces that came are kept: the second send needs only the rest.
		return missing, tx.Commit()
	}
	if _, err := tx.Exec(`INSERT INTO calls(exchange, session, vendor, time, size, top, pieces)
		VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(exchange) DO NOTHING`,
		c.Exchange, c.Session, c.Vendor, now, c.Size, string(c.Request), string(list)); err != nil {
		return nil, fmt.Errorf("store: write a call: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: write a call: %w", err)
	}
	return nil, nil
}
