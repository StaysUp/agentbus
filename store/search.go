package store

// Search: text search over one session's stored history, for an agent that
// reads its own earlier turns, and for a cockpit.
//
// What is searched:
//   - the messages (Anthropic) and input items (OpenAI Responses) of the
//     session's stored requests, tool results among them. A message comes
//     back in every later request of its conversation, so each piece is
//     searched once, in the first call that carried it. System blocks,
//     instructions and tool definitions (Anthropic's tool pieces, and codex's
//     additional_tools and tool_search_output items) are not searched: every
//     call repeats them, and they are the harness's, not the work's;
//   - each call's kept reply (the model's message as the harness sends it
//     back next), once, the same way; a codex reply, a list of output items,
//     item by item, each under its own hash as a request item would be, so
//     an item a later request carries is searched once, and an item with
//     tool definitions (toolItems) is left out;
//   - the text of three kinds of event: a turn's turn.started (the prompt as
//     sent), a program's own x.* event, and a model.responded whose reply is
//     not searched as a reply: none is kept (one that failed or was cut
//     off), or it is kept but its call's request is not.
//
// Inside a piece only the string values are read, decoded from JSON, so an
// escaped quote or é matches as the character it is. Ids, types, roles,
// signatures, encrypted reasoning and base64 data are skipped. A match ignores
// case, rune by rune (Unicode simple folding).
//
// A call with no model.requested event stored for it (the event queue fell
// behind) has no seq and no turn, so it is not searched.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
)

// SearchQuery picks a page of hits.
type SearchQuery struct {
	Session string
	Q       string
	// Before is the NextBefore of the previous page; empty for the newest.
	Before string
	Limit  int
	// SkipTurn leaves out the hits of this turn: the turn that searches, so
	// its own search call is never a hit. Empty: none is left out.
	SkipTurn string
	// MaxBytes bounds the hits' JSON on one page. A page holds at least one
	// hit.
	MaxBytes int
	// MaxScan bounds the text read for one page, in bytes. A page reads at
	// least one piece or event.
	MaxScan int
}

// SearchPageBytes and SearchScanBytes are the bounds the API uses.
const (
	SearchPageBytes = 1 << 20
	SearchScanBytes = 64 << 20
)

// SnippetRunes is how many characters a snippet keeps on each side of the
// match.
const SnippetRunes = 80

// SearchResult is one page of hits.
type SearchResult struct {
	Store string      `json:"store" doc:"The store id."`
	Hits  []SearchHit `json:"hits" doc:"Newest first. One hit per piece or event: its first match."`
	// NextBefore is the cursor of the next, older page.
	NextBefore *string `json:"next_before" doc:"Send it as before for the next, older page; null when nothing older is left. A page stops at limit hits, at 1 MiB of hits, or after reading 64 MiB of text, whichever comes first, so a page can hold fewer than limit hits, even none, and still have a next page."`
}

// SearchHit is one match.
type SearchHit struct {
	Source  string `json:"source" doc:"request: a message or input item of a call's request. reply: a call's kept reply. event: an event's text."`
	Turn    string `json:"turn" doc:"The turn of the call or event. Empty for traffic no turn id names."`
	Call    string `json:"call,omitempty" doc:"The model call, the exchange of its events. Absent on an event hit without one."`
	Piece   *int   `json:"piece,omitempty" doc:"On a request hit: the piece's index in the call's request, as GET /api/v0/turns/{id}/calls/{call}/pieces/{n} takes it."`
	Seq     int64  `json:"seq" doc:"The event the hit is ordered by: the call's model.requested for a request hit, its model.responded for a reply hit (model.requested when none is stored), the event itself for an event hit."`
	Kind    string `json:"kind" doc:"On a request or reply hit: the piece's kind, message or input_item. On an event hit: the event's kind."`
	Role    string `json:"role,omitempty" doc:"On a request or reply hit: the piece's label, a message's role or an input item's role or type."`
	Block   string `json:"block,omitempty" doc:"The type of the content block the match is in, such as text, tool_use, tool_result, thinking or function_call_output. Absent when the text is in no typed block."`
	Snippet string `json:"snippet" doc:"Up to 80 characters on each side of the first match, cut at whole characters; … marks a cut. Valid UTF-8; a NUL byte in the text stays a NUL character."`
	Read    string `json:"read" doc:"The path that returns the whole of what matched: the piece, the reply, or a one-event page of the session's events."`
}

// ErrSearchCursor: a before that no page gave.
var ErrSearchCursor = errors.New("store: before is not a search cursor")

var searchCursor = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})\.(0|[1-9][0-9]{0,8})$`)

// key orders candidates: by seq, then by sub within one seq.
type key struct {
	seq int64
	sub int
}

func (k key) less(o key) bool { return k.seq < o.seq || k.seq == o.seq && k.sub < o.sub }

func (k key) String() string { return strconv.FormatInt(k.seq, 10) + "." + strconv.Itoa(k.sub) }

func parseCursor(s string) (key, error) {
	m := searchCursor.FindStringSubmatch(s)
	if m == nil {
		return key{}, ErrSearchCursor
	}
	seq, err1 := strconv.ParseInt(m[1], 10, 64)
	sub, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return key{}, ErrSearchCursor
	}
	return key{seq, sub}, nil
}

// candidate is one thing to search: a piece of a call, a reply, or an event.
type candidate struct {
	key
	source     string
	turn, call string
	piece      int // a request piece's index
	// item, on a reply candidate, is the index of one output item of a
	// codex reply (a JSON list); -1 for a reply searched whole.
	item        int
	hash        string
	kind, label string
}

// Search finds q in the session's stored history, newest first.
func (s *Store) Search(q SearchQuery) (SearchResult, error) {
	res := SearchResult{Store: s.id, Hits: []SearchHit{}}
	before := key{seq: 1<<63 - 1}
	if q.Before != "" {
		k, err := parseCursor(q.Before)
		if err != nil {
			return res, err
		}
		before = k
	}
	cands, err := s.searchCandidates(q.Session, s.Last())
	if err != nil {
		return res, err
	}
	sort.Slice(cands, func(i, j int) bool { return cands[j].less(cands[i].key) })
	i := sort.Search(len(cands), func(i int) bool { return cands[i].less(before) })
	cands = cands[i:]

	m := newMatcher(q.Q)
	size, scanned := 0, 0
	var last *key // the last candidate searched, whose hit (if any) is on this page
	stop := func() {
		if last != nil {
			c := last.String()
			res.NextBefore = &c
		}
	}
	for i := range cands {
		c := &cands[i]
		if len(res.Hits) == q.Limit || (last != nil && scanned >= q.MaxScan) {
			stop()
			return res, nil
		}
		if q.SkipTurn != "" && c.turn == q.SkipTurn {
			last = &c.key
			continue
		}
		text, n, err := s.candidateText(c)
		if err != nil {
			return res, err
		}
		scanned += n
		hit, ok := m.find(text, c)
		if ok {
			hit.Read = s.readPath(q.Session, c)
			b, err := json.Marshal(hit)
			if err != nil {
				return res, fmt.Errorf("store: search: %w", err)
			}
			if len(res.Hits) > 0 && size+len(b) > q.MaxBytes {
				stop() // last is the candidate before this one
				return res, nil
			}
			size += len(b)
			res.Hits = append(res.Hits, hit)
		}
		last = &c.key
	}
	return res, nil
}

// searchCandidates lists what Search reads for session, up to seq hi, in no
// order.
func (s *Store) searchCandidates(session string, hi int64) ([]candidate, error) {
	type model struct {
		requested, responded int64
		turn                 string
	}
	calls := map[string]*model{}
	rows, err := s.r.Query(`SELECT seq, kind, turn, COALESCE(json_extract(data, '$.exchange'), '') FROM events
		WHERE session = ? AND seq <= ? AND kind IN ('model.requested', 'model.responded') ORDER BY seq`, session, hi)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	for rows.Next() {
		var seq int64
		var kind, turn, exchange string
		if err := rows.Scan(&seq, &kind, &turn, &exchange); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: search: %w", err)
		}
		if exchange == "" {
			continue
		}
		c := calls[exchange]
		if c == nil {
			c = &model{}
			calls[exchange] = c
		}
		switch {
		case kind == string(event.ModelRequested) && c.requested == 0:
			c.requested, c.turn = seq, turn
		case kind == string(event.ModelResponded) && c.responded == 0:
			c.responded = seq
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: search: %w", err)
	}
	rows.Close()

	type stored struct {
		exchange string
		refs     []pieceRef
		reply    string // the kept reply's hash, or ""
		m        *model
	}
	var list []stored
	rows, err = s.r.Query(`SELECT c.exchange, c.pieces, COALESCE(r.hash, '') FROM calls c
		LEFT JOIN replies r ON r.exchange = c.exchange WHERE c.session = ?`, session)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	for rows.Next() {
		var st stored
		var refs string
		if err := rows.Scan(&st.exchange, &refs, &st.reply); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: search: %w", err)
		}
		if st.m = calls[st.exchange]; st.m == nil || st.m.requested == 0 {
			continue // no model.requested: no seq, no turn
		}
		if err := json.Unmarshal([]byte(refs), &st.refs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: search: %w", err)
		}
		list = append(list, st)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: search: %w", err)
	}
	rows.Close()
	// In the order the calls were made, so a piece is searched in the first
	// call that carried it, and a reply sent back in the next request is
	// searched as the reply.
	sort.Slice(list, func(i, j int) bool { return list[i].m.requested < list[j].m.requested })

	var out []candidate
	seen := map[string]bool{}
	// replied names the calls whose kept reply is searched, as the reply or
	// as the same bytes in a later request. Only their model.responded text
	// is left out below: a reply kept without its call (the body queue
	// dropped the request) is searched through its event's text instead.
	replied := map[string]bool{}
	for _, st := range list {
		for n, r := range st.refs {
			if (r.Kind != body.Message && r.Kind != body.InputItem) || toolItems[r.Label] || seen[r.Hash] {
				continue
			}
			seen[r.Hash] = true
			out = append(out, candidate{key: key{st.m.requested, n + 1}, source: "request", turn: st.m.turn,
				call: st.exchange, piece: n, hash: r.Hash, kind: r.Kind, label: r.Label})
		}
		if st.reply != "" {
			replied[st.exchange] = true
		}
		if st.reply == "" || seen[st.reply] {
			continue
		}
		seen[st.reply] = true
		k := key{st.m.responded, 0}
		if st.m.responded == 0 {
			k = key{st.m.requested, len(st.refs) + 1}
		}
		items, err := s.replyItems(st.reply)
		if err != nil {
			return nil, err
		}
		if items == nil {
			out = append(out, candidate{key: k, source: "reply", turn: st.m.turn, call: st.exchange,
				item: -1, hash: st.reply, kind: body.Message, label: "assistant"})
			continue
		}
		// A codex reply is a list of output items, and a later request
		// carries them one by one (own replay sends them back byte for
		// byte). Each item is a candidate of its own under the hash a
		// request piece of the same bytes has, so it is searched once, and
		// an item carrying tool definitions is left out as in a request.
		for i, it := range items {
			label := itemLabel(it)
			h := body.Piece{JSON: it}.Hash()
			if toolItems[label] || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, candidate{key: key{k.seq, k.sub + i}, source: "reply", turn: st.m.turn, call: st.exchange,
				item: i, hash: st.reply, kind: body.InputItem, label: label})
		}
	}

	// json_type, not a comparison of the text: SQLite's text functions stop
	// at a NUL, which a prompt can hold.
	rows, err = s.r.Query(`SELECT seq, kind, turn, COALESCE(json_extract(data, '$.exchange'), '') FROM events
		WHERE session = ? AND seq <= ? AND json_type(data, '$.text') = 'text' AND (
			kind IN ('turn.started', 'model.responded') OR kind GLOB 'x.*')`, session, hi)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.seq, &c.kind, &c.turn, &c.call); err != nil {
			return nil, fmt.Errorf("store: search: %w", err)
		}
		if c.kind == string(event.ModelResponded) && replied[c.call] {
			continue // searched as its reply
		}
		c.source = "event"
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	return out, nil
}

// candidateText reads what c holds: a piece's or reply's bytes, or an event's
// data. n is the bytes read.
func (s *Store) candidateText(c *candidate) (data []byte, n int, err error) {
	if c.source == "event" {
		var d string
		if err := s.r.QueryRow(`SELECT data FROM events WHERE seq = ?`, c.seq).Scan(&d); err != nil {
			return nil, 0, fmt.Errorf("store: search: %w", err)
		}
		return []byte(d), len(d), nil
	}
	if err := s.r.QueryRow(`SELECT bytes FROM pieces WHERE hash = ?`, c.hash).Scan(&data); err != nil {
		return nil, 0, fmt.Errorf("store: search: %w", err)
	}
	if c.source == "reply" && c.item >= 0 {
		items, _ := body.Elements(data)
		if c.item >= len(items) {
			return nil, 0, fmt.Errorf("store: search: a reply item is missing")
		}
		data = items[c.item]
	}
	return data, len(data), nil
}

// replyItems reads a kept reply and returns its items when it is a list (a
// codex reply's output items), else nil (an Anthropic message).
func (s *Store) replyItems(hash string) ([]json.RawMessage, error) {
	var data []byte
	if err := s.r.QueryRow(`SELECT bytes FROM pieces WHERE hash = ?`, hash).Scan(&data); err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	items, ok := body.Elements(data)
	if !ok {
		return nil, nil
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	return items, nil
}

// itemLabel names an output item as the OpenAI adapter names an input item:
// a message by its role, anything else by its type.
func itemLabel(raw json.RawMessage) string {
	if t := body.Label(raw, "type"); t != "" && t != "message" {
		return t
	}
	return body.Label(raw, "role")
}

// readPath is the API path that returns the whole of c.
func (s *Store) readPath(session string, c *candidate) string {
	switch {
	case c.turn == "":
	case c.source == "request":
		return "/api/v0/turns/" + c.turn + "/calls/" + c.call + "/pieces/" + strconv.Itoa(c.piece)
	case c.source == "reply":
		return "/api/v0/turns/" + c.turn + "/calls/" + c.call + "/pieces/reply"
	}
	return "/api/v0/sessions/" + session + "/events?store=" + s.id + "&after=" + strconv.FormatInt(c.seq-1, 10) + "&limit=1"
}

// toolItems are the OpenAI input items that carry tool definitions, by their
// type (the label the adapter gives an input item that is not a message):
// codex-cli 0.164.0 sends its tools as an additional_tools item, and a tool
// search's output loads more. They are left out like Anthropic's tool pieces,
// by kind, never by their text.
var toolItems = map[string]bool{"additional_tools": true, "tool_search_output": true}

// skipKeys are the JSON keys whose string values are not text of the work.
var skipKeys = map[string]bool{
	"type": true, "id": true, "tool_use_id": true, "call_id": true, "role": true, "status": true,
	"signature": true, "encrypted_content": true, "data": true, "media_type": true, "cache_control": true,
}

// matcher finds one query in text, ignoring case.
type matcher struct {
	q []rune
	// ascii marks the ASCII bytes the query's first rune folds to, so most
	// positions are refused without decoding a rune.
	ascii  [utf8.RuneSelf]bool
	first  []rune // every rune the first folds to, the first included
	nonASC bool   // first folds to a rune outside ASCII
}

func newMatcher(q string) *matcher {
	m := &matcher{q: []rune(q)}
	if len(m.q) == 0 {
		return m
	}
	r := m.q[0]
	for f := r; ; {
		m.first = append(m.first, f)
		if f < utf8.RuneSelf {
			m.ascii[f] = true
		} else {
			m.nonASC = true
		}
		if f = unicode.SimpleFold(f); f == r {
			break
		}
	}
	return m
}

// find searches a candidate's data and makes its hit.
func (m *matcher) find(data []byte, c *candidate) (SearchHit, bool) {
	hit := SearchHit{Source: c.source, Turn: c.turn, Call: c.call, Seq: c.seq, Kind: c.kind}
	if c.source == "event" {
		var e struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(data, &e) != nil {
			return hit, false
		}
		snip, ok := m.snippet(e.Text)
		hit.Snippet = snip
		return hit, ok
	}
	if c.source == "request" {
		p := c.piece
		hit.Piece = &p
	}
	hit.Role = c.label
	var v any
	if json.Unmarshal(data, &v) != nil {
		// Not JSON: read the bytes as text.
		snip, ok := m.snippet(string(data))
		hit.Snippet = snip
		return hit, ok
	}
	found := false
	walk(v, "", func(text, block string) bool {
		if snip, ok := m.snippet(text); ok {
			hit.Snippet, hit.Block, found = snip, block, true
			return true
		}
		return false
	})
	return hit, found
}

// walk calls f on every string value of v outside skipKeys, with the type of
// the object nearest around it, until f returns true.
func walk(v any, block string, f func(text, block string) bool) bool {
	switch x := v.(type) {
	case string:
		return f(x, block)
	case []any:
		for _, e := range x {
			if walk(e, block, f) {
				return true
			}
		}
	case map[string]any:
		if t, ok := x["type"].(string); ok {
			block = t
		}
		// In a fixed order, so the same piece always gives the same hit.
		keys := make([]string, 0, len(x))
		for k := range x {
			if !skipKeys[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			if walk(x[k], block, f) {
				return true
			}
		}
	}
	return false
}

// snippet finds the query in text and cuts the text around the first match.
func (m *matcher) snippet(text string) (string, bool) {
	i, j := m.index(text)
	if i < 0 {
		return "", false
	}
	start := i
	for n := 0; n < SnippetRunes && start > 0; n++ {
		_, size := utf8.DecodeLastRuneInString(text[:start])
		start -= size
	}
	end := j
	for n := 0; n < SnippetRunes && end < len(text); n++ {
		_, size := utf8.DecodeRuneInString(text[end:])
		end += size
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(text[start:end])
	if end < len(text) {
		b.WriteString("…")
	}
	return strings.ToValidUTF8(b.String(), "�"), true
}

// match reports whether s holds the query.
func (m *matcher) match(s string) bool { i, _ := m.index(s); return i >= 0 }

// index is the byte span of the first match of the query in s, or -1, -1.
func (m *matcher) index(s string) (int, int) {
	if len(m.q) == 0 {
		return -1, -1
	}
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if m.ascii[c] {
				if n, ok := m.prefix(s[i:]); ok {
					return i, i + n
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if m.nonASC && m.isFirst(r) {
			if n, ok := m.prefix(s[i:]); ok {
				return i, i + n
			}
		}
		i += size
	}
	return -1, -1
}

func (m *matcher) isFirst(r rune) bool {
	for _, f := range m.first {
		if f == r {
			return true
		}
	}
	return false
}

// prefix reports whether s starts with the query, ignoring case, and how many
// bytes of s the match takes.
func (m *matcher) prefix(s string) (int, bool) {
	n := 0
	for _, qr := range m.q {
		if n >= len(s) {
			return 0, false
		}
		r, size := utf8.DecodeRuneInString(s[n:])
		if r != qr && !foldsTo(r, qr) {
			return 0, false
		}
		n += size
	}
	return n, true
}

// foldsTo reports whether a and b are the same rune under simple folding.
func foldsTo(a, b rune) bool {
	for f := unicode.SimpleFold(a); f != a; f = unicode.SimpleFold(f) {
		if f == b {
			return true
		}
	}
	return false
}
