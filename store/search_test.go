package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
)

// history stores a two-call turn of session s1, a call of another session,
// and their events:
//
//	turn.started t_1 "find the needle"                       seq 1
//	x-1 requested: system "needle in the system", user msg   seq 2
//	x-1 responded, reply kept: tool_use with needle          seq 3
//	x-2 requested: user msg again, tool_result with Needle   seq 4
//	x-2 responded, reply not kept, text "no NEEDLE here"     seq 5
//	x-9 requested in session s2, user msg with needle        seq 6
func history(t *testing.T) *Store {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	now := time.Unix(1793858039, 0).UTC()
	user := `{"role":"user","content":"find the needle"}`
	sys := body.Piece{Kind: body.System, Label: "text", JSON: json.RawMessage(`{"type":"text","text":"needle in the system"}`)}
	msg := body.Piece{Kind: body.Message, Label: "user", JSON: json.RawMessage(user)}
	for _, c := range []body.Call{
		{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: now,
			Request: body.Request{Top: json.RawMessage(`{"model":"m"}`), Pieces: []body.Piece{sys, msg}}},
		{Exchange: "x-2", Session: "s1", Vendor: "anthropic", Time: now.Add(time.Second),
			Request: body.Request{Top: json.RawMessage(`{"model":"m"}`), Pieces: []body.Piece{sys, msg,
				{Kind: body.Message, Label: "assistant", JSON: json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_needle","name":"Read","input":{"file_path":"/a/needle.go"}}]}`)},
				{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_needle","content":"package a // the Needle is here"}]}`)}}}},
		{Exchange: "x-9", Session: "s2", Vendor: "anthropic", Time: now,
			Request: body.Request{Top: json.RawMessage(`{"model":"m"}`), Pieces: []body.Piece{
				{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role":"user","content":"another needle"}`)}}}},
	} {
		if err := s.KeepCall(c); err != nil {
			t.Fatal(err)
		}
	}
	// The reply as the harness sends it back: the same bytes as x-2's third
	// piece, so it is searched once, as the reply.
	if err := s.KeepReply(body.Reply{Exchange: "x-1", Status: 200, Stop: "tool_use", Time: now,
		Message: json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_needle","name":"Read","input":{"file_path":"/a/needle.go"}}]}`)}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []event.Event{
		{Kind: event.TurnStarted, Session: "s1", Turn: "t_1", Text: "find the needle"},
		{Kind: event.ModelRequested, Session: "s1", Turn: "t_1", Exchange: "x-1"},
		{Kind: event.ModelResponded, Session: "s1", Turn: "t_1", Exchange: "x-1", Text: "reading needle.go"},
		{Kind: event.ModelRequested, Session: "s1", Turn: "t_1", Exchange: "x-2"},
		{Kind: event.ModelResponded, Session: "s1", Turn: "t_1", Exchange: "x-2", Text: "no NEEDLE here"},
		{Kind: event.ModelRequested, Session: "s2", Turn: "t_9", Exchange: "x-9"},
	} {
		e.ID, e.Time = "evt_"+e.Exchange+string(e.Kind), now
		if _, _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func search(t *testing.T, s *Store, q SearchQuery) SearchResult {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.MaxBytes == 0 {
		q.MaxBytes = SearchPageBytes
	}
	if q.MaxScan == 0 {
		q.MaxScan = SearchScanBytes
	}
	if q.Session == "" {
		q.Session = "s1"
	}
	res, err := s.Search(q)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func where(h SearchHit) string {
	p := ""
	if h.Piece != nil {
		p = "/" + string(rune('0'+*h.Piece))
	}
	return h.Source + ":" + h.Call + p
}

func TestSearchFindsEachPlaceOnceNewestFirst(t *testing.T) {
	s := history(t)
	res := search(t, s, SearchQuery{Q: "NeEdLe"})
	var got []string
	for _, h := range res.Hits {
		got = append(got, where(h))
		if h.Turn != "t_1" {
			t.Errorf("%s: turn %q", where(h), h.Turn)
		}
	}
	// x-2 responded (no reply kept: its event), x-2's new tool_result, x-1's
	// reply, x-1's user message, the turn's prompt. Not the system block,
	// not the repeated pieces, not s2, not x-1's model.responded (its reply is
	// kept).
	want := []string{"event:x-2", "request:x-2/3", "reply:x-1", "request:x-1/1", "event:"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("hits %v, want %v", got, want)
	}
	if res.NextBefore != nil {
		t.Errorf("next_before %q on the last page", *res.NextBefore)
	}
	h := res.Hits[1]
	if h.Block != "tool_result" || h.Role != "user" || h.Kind != body.Message || h.Snippet != "package a // the Needle is here" {
		t.Errorf("tool_result hit %+v", h)
	}
	if h.Read != "/api/v0/calls/x-2/pieces/3" || res.Hits[2].Read != "/api/v0/calls/x-1/pieces/reply" {
		t.Errorf("read paths %q %q", h.Read, res.Hits[2].Read)
	}
	if r := res.Hits[2]; r.Block != "tool_use" || r.Snippet != "/a/needle.go" {
		t.Errorf("reply hit %+v: the match is in the tool's input, not its id", r)
	}
	if e := res.Hits[4]; e.Kind != "turn.started" || e.Seq != 1 || e.Read != "/api/v0/sessions/s1/events?store="+s.ID()+"&after=0&limit=1" {
		t.Errorf("prompt hit %+v", e)
	}
}

func TestSearchNoMatch(t *testing.T) {
	s := history(t)
	for _, q := range []string{"haystack", "toolu", "tool_result", "assistant"} {
		res := search(t, s, SearchQuery{Q: q})
		if len(res.Hits) != 0 || res.NextBefore != nil {
			t.Errorf("%q: %+v, want no hits: ids, types and roles are not searched", q, res)
		}
	}
	if res := search(t, s, SearchQuery{Q: "needle", Session: "nobody"}); len(res.Hits) != 0 || res.Hits == nil {
		t.Errorf("unknown session: %+v, want an empty list", res)
	}
}

func TestSearchPages(t *testing.T) {
	s := history(t)
	all := search(t, s, SearchQuery{Q: "needle"})
	var got []SearchHit
	before := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging does not end")
		}
		res := search(t, s, SearchQuery{Q: "needle", Limit: 2, Before: before})
		if len(res.Hits) > 2 {
			t.Fatalf("%d hits, limit 2", len(res.Hits))
		}
		got = append(got, res.Hits...)
		if res.NextBefore == nil {
			break
		}
		before = *res.NextBefore
	}
	a, _ := json.Marshal(all.Hits)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("pages %s\nwant %s", b, a)
	}
	if _, err := s.Search(SearchQuery{Session: "s1", Q: "needle", Limit: 1, Before: "12"}); !errors.Is(err, ErrSearchCursor) {
		t.Errorf("before 12: %v, want ErrSearchCursor", err)
	}
}

// The byte budget and the scan budget both end a page early, with a cursor
// that loses nothing.
func TestSearchBudgets(t *testing.T) {
	s := history(t)
	all := search(t, s, SearchQuery{Q: "needle"})
	for name, q := range map[string]SearchQuery{
		"bytes": {Q: "needle", MaxBytes: 1},
		"scan":  {Q: "needle", MaxScan: 1},
	} {
		var got []SearchHit
		for pages := 0; ; pages++ {
			if pages > 20 {
				t.Fatalf("%s: paging does not end", name)
			}
			res := search(t, s, q)
			if len(res.Hits) > 1 {
				t.Fatalf("%s: %d hits on a page with a 1-byte budget", name, len(res.Hits))
			}
			got = append(got, res.Hits...)
			if res.NextBefore == nil {
				break
			}
			q.Before = *res.NextBefore
		}
		if len(got) != len(all.Hits) {
			t.Errorf("%s: %d hits over all pages, want %d", name, len(got), len(all.Hits))
		}
	}
}

// A NUL byte in a piece or an event's text neither hides a match after it
// nor breaks the snippet. Snippets are cut at whole characters.
func TestSearchNULAndUnicode(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	long := strings.Repeat("é", 200)
	piece := `{"role":"user","content":[{"type":"text","text":"a\u0000b ÉCOLE ` + long + `"}]}`
	if err := s.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: time.Now(),
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{{Kind: body.Message, Label: "user", JSON: json.RawMessage(piece)}}}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []event.Event{
		{ID: "e1", Kind: event.TurnStarted, Session: "s1", Turn: "t_1", Text: "\x00 école \x00"},
		{ID: "e2", Kind: event.ModelRequested, Session: "s1", Turn: "t_1", Exchange: "x-1"},
	} {
		if _, _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	res := search(t, s, SearchQuery{Q: "école"})
	if len(res.Hits) != 2 {
		t.Fatalf("hits %+v, want the piece and the prompt", res.Hits)
	}
	p, e := res.Hits[0], res.Hits[1]
	if !utf8.ValidString(p.Snippet) || !strings.HasPrefix(p.Snippet, "a\x00b ÉCOLE ") || !strings.HasSuffix(p.Snippet, "…") {
		t.Errorf("piece snippet %q", p.Snippet)
	}
	if n := utf8.RuneCountInString(p.Snippet); n != 4+5+SnippetRunes+1 { // "a\x00b ", ÉCOLE, 80 after it, …
		t.Errorf("piece snippet is %d characters", n)
	}
	if e.Snippet != "\x00 école \x00" {
		t.Errorf("prompt snippet %q", e.Snippet)
	}
	b, err := json.Marshal(res)
	if err != nil || !json.Valid(b) {
		t.Fatalf("the result does not encode: %v", err)
	}
	// The Kelvin sign (U+212A) folds to k: a first rune outside ASCII.
	if !newMatcher("\u212Aelvin").match("KELVIN") || newMatcher("kelvin").match("kelvi") {
		t.Error("simple folding: the Kelvin sign is k")
	}
}

func TestCallPiece(t *testing.T) {
	s := history(t)
	p, n, err := s.CallPiece("x-2", 3, false)
	want := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_needle","content":"package a // the Needle is here"}]}`
	sum := sha256.Sum256([]byte(want))
	if err != nil || n != 4 || string(p.JSON) != want || p.Hash != hex.EncodeToString(sum[:]) || p.Size != len(want) || p.Label != "user" {
		t.Fatalf("piece 3: %+v %d %v", p, n, err)
	}
	if p, _, err := s.CallPiece("x-1", 0, true); err != nil || p.Kind != body.Message || p.Label != "assistant" || !strings.Contains(string(p.JSON), "needle.go") {
		t.Fatalf("reply: %+v %v", p, err)
	}
	for _, tc := range []struct {
		exchange string
		n        int
		reply    bool
		want     error
	}{{"x-2", 4, false, ErrNoPiece}, {"x-2", -1, false, ErrNoPiece}, {"x-2", 0, true, ErrNoReply}, {"x-404", 0, false, ErrNoCall}} {
		if _, _, err := s.CallPiece(tc.exchange, tc.n, tc.reply); !errors.Is(err, tc.want) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	if sess, ok, err := s.CallSession("x-9"); err != nil || !ok || sess != "s2" {
		t.Errorf("CallSession x-9: %q %v %v", sess, ok, err)
	}
	if _, ok, err := s.CallSession("x-404"); err != nil || ok {
		t.Errorf("CallSession x-404: %v %v", ok, err)
	}
}

// codex's tool definitions come as input items; they are not searched, by
// their kind, while a message about the same text is.
func TestSearchSkipsCodexToolItems(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	if err := s.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "openai", Time: time.Now(),
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{
			{Kind: body.InputItem, Label: "additional_tools", JSON: json.RawMessage(`{"type":"additional_tools","role":"developer","tools":[{"name":"shell","description":"Runs a zebracommand"}]}`)},
			{Kind: body.InputItem, Label: "tool_search_output", JSON: json.RawMessage(`{"type":"tool_search_output","tools":[{"name":"apply_patch","description":"zebrapatch"}]}`)},
			{Kind: body.InputItem, Label: "user", JSON: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"use the zebrapatch"}]}`)}}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(event.Event{ID: "e1", Kind: event.ModelRequested, Session: "s1", Turn: "t_1", Exchange: "x-1"}); err != nil {
		t.Fatal(err)
	}
	if res := search(t, s, SearchQuery{Q: "zebracommand"}); len(res.Hits) != 0 {
		t.Errorf("a tool description was searched: %+v", res.Hits)
	}
	res := search(t, s, SearchQuery{Q: "zebrapatch"})
	if len(res.Hits) != 1 || res.Hits[0].Role != "user" || *res.Hits[0].Piece != 2 {
		t.Errorf("hits %+v, want the user message only", res.Hits)
	}
}

// A reply kept while its call's request was dropped is still searchable,
// through its model.responded text; a reply that is searched as a reply is
// not found twice.
func TestSearchReplyWithoutItsRequest(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	if err := s.KeepReply(body.Reply{Exchange: "x-lost", Status: 200, Stop: "end_turn", Time: time.Now(),
		Message: json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"the orphan answer"}]}`)}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []event.Event{
		{ID: "e1", Kind: event.ModelRequested, Session: "s1", Turn: "t_1", Exchange: "x-lost"},
		{ID: "e2", Kind: event.ModelResponded, Session: "s1", Turn: "t_1", Exchange: "x-lost", Text: "the orphan answer"},
	} {
		if _, _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	res := search(t, s, SearchQuery{Q: "orphan"})
	if len(res.Hits) != 1 || res.Hits[0].Source != "event" || res.Hits[0].Kind != "model.responded" || res.Hits[0].Call != "x-lost" {
		t.Fatalf("hits %+v, want the reply's event text", res.Hits)
	}
	// history's x-1 reply is kept with its request: one hit, as the reply.
	h := history(t)
	n := 0
	for _, hit := range search(t, h, SearchQuery{Q: "reading needle.go"}).Hits {
		n++
		if hit.Source != "event" {
			t.Errorf("unexpected hit %+v", hit)
		}
	}
	if n != 0 {
		t.Errorf("x-1's model.responded text was searched beside its kept reply")
	}
}

// A codex reply is kept as a list of output items, and the next request
// carries each item on its own, byte for byte under own replay. Each item is
// searched once, as the reply; an item carrying tool definitions
// (tool_search_output) is left out, as in a request.
func TestSearchCodexReplyItemsOnce(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	now := time.Now()
	call := `{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"grep yakneedle"}`
	tools := `{"type":"tool_search_output","call_id":"c2","tools":[{"name":"t","description":"a yakdefinition"}]}`
	msg := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"looking for the yakneedle"}]}`
	user := body.Piece{Kind: body.InputItem, Label: "user", JSON: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}`)}
	for _, c := range []body.Call{
		{Exchange: "x-1", Session: "s1", Vendor: "openai", Time: now,
			Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{user}}},
		{Exchange: "x-2", Session: "s1", Vendor: "openai", Time: now.Add(time.Second),
			Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{user,
				{Kind: body.InputItem, Label: "assistant", JSON: json.RawMessage(msg)},
				{Kind: body.InputItem, Label: "custom_tool_call", JSON: json.RawMessage(call)},
				{Kind: body.InputItem, Label: "tool_search_output", JSON: json.RawMessage(tools)}}}},
	} {
		if err := s.KeepCall(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.KeepReply(body.Reply{Exchange: "x-1", Status: 200, Stop: "completed", Time: now,
		Message: json.RawMessage(`[` + msg + `,` + call + `,` + tools + `]`)}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []event.Event{
		{Kind: event.ModelRequested, Session: "s1", Turn: "t_1", Exchange: "x-1"},
		{Kind: event.ModelResponded, Session: "s1", Turn: "t_1", Exchange: "x-1"},
		{Kind: event.ModelRequested, Session: "s1", Turn: "t_1", Exchange: "x-2"},
	} {
		e.ID, e.Time = "evt_"+e.Exchange+string(e.Kind), now
		if _, _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	res := search(t, s, SearchQuery{Q: "yakneedle"})
	if len(res.Hits) != 2 {
		t.Fatalf("hits %+v, want the message and the call once each", res.Hits)
	}
	roles := map[string]bool{}
	for _, h := range res.Hits {
		if h.Source != "reply" || h.Kind != "input_item" || h.Read != "/api/v0/calls/x-1/pieces/reply" {
			t.Errorf("hit %+v, want a reply item", h)
		}
		roles[h.Role] = true
	}
	if !roles["assistant"] || !roles["custom_tool_call"] {
		t.Errorf("roles %v", roles)
	}
	if res := search(t, s, SearchQuery{Q: "yakdefinition"}); len(res.Hits) != 0 {
		t.Errorf("a tool definition in a reply was searched: %+v", res.Hits)
	}
	// The stored reply is unchanged: one list, as replay reads it.
	if r, ok, err := s.Reply("x-1"); err != nil || !ok || string(r.Message) != `[`+msg+`,`+call+`,`+tools+`]` {
		t.Errorf("stored reply changed: %v %v %s", ok, err, r.Message)
	}
}
