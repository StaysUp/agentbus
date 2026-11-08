//go:build integration

package api

// TestOpenAPIResponsesMatch runs a real child process, so it runs with the
// integration tag. The other OpenAPI checks are in openapi_test.go.

import (
	"encoding/json"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// fill stores one event of every framework kind in session s1, with the
// fields each carries in real traffic.
func fill(t *testing.T, st *store.Store) {
	t.Helper()
	now := time.Now().UTC()
	in, lat, cost, yes, no, code := 100, int64(840), 0.0123, true, false, 0
	u := &event.Usage{InputTokens: in, OutputTokens: 20, CacheReadTokens: 3000, CacheWriteTokens: 40,
		CacheWrite5mTokens: 40, ServiceTier: "standard", InferenceGeo: "global",
		ServerToolUse: map[string]int{"web_search_requests": 1}}
	base := event.Event{Time: now, Session: "s1", Exchange: "x-1", Vendor: "anthropic", Client: "claude-cli/2.1.318", Turn: "t_1"}
	evs := []event.Event{
		{Kind: event.TurnStarted, Text: "Fix the login bug", Agent: "claude", Folder: "/Users/ana/app"},
		{Kind: event.ModelRequested, Model: "claude-sonnet-5-5", ToolsOffered: []string{"Read", "Edit"}},
		{Kind: event.ModelResponded, Model: "claude-sonnet-5-5", Text: "Reading it.", StopReason: "tool_use",
			Status: 200, Usage: u, LatencyMS: &lat, CostUSD: &cost, CostComplete: &yes},
		{Kind: event.ToolRequested, ToolID: "toolu_1", ToolName: "Read", ToolInput: json.RawMessage(`{"file_path":"/a.go"}`)},
		{Kind: event.ToolCompleted, ToolID: "toolu_1", ToolOutput: "package a", ToolError: &no},
		{Kind: event.ModelResponded, Status: 529, Error: "overloaded"},
		{Kind: event.TurnCompleted, Outcome: event.Finished, StopReason: "end_turn"},
		{Kind: event.SchemaUnexpected, Unexpected: &event.Unexpected{Where: "response.content[].type", Value: "server_tool_use_v2"}},
	}
	for i, e := range evs {
		e.Time, e.Session, e.Exchange, e.Vendor, e.Client, e.Turn = base.Time, base.Session, base.Exchange, base.Vendor, base.Client, base.Turn
		e.ID = "e-1-" + strconv.Itoa(i)
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []event.Event{
		{ID: "e-1-90", Kind: event.EventsDropped, Time: now, Vendor: "anthropic", Dropped: 3, Sessions: []string{"s1"}, Error: "the event output did not keep up"},
		{ID: "e-1-91", Kind: event.EventsDropped, Time: now, Vendor: "openai", Dropped: 1, AllSessions: true},
		// An older store's registration: nothing writes it now, readers
		// still meet it.
		{ID: "evt_0", Kind: event.SessionRegistered, Time: now, Session: "s1", Agent: "claude", Folder: "/Users/ana/app"},
		{ID: "evt_4", Kind: event.SessionOpened, Time: now, Session: "s1", Hold: "hold_2", Agent: "claude", Folder: "/Users/ana/app"},
		{ID: "evt_5", Kind: event.SessionClosed, Time: now, Session: "s1", Hold: "hold_2", ExitCode: &code, ResumeID: "abc-2"},
	} {
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

// The service's real responses match the spec: sessions, a page of events of
// every kind, the stream, a program's event, a run's 202, and
// errors.
func TestOpenAPIResponsesMatch(t *testing.T) {
	doc := loadSpec(t)
	folder := t.TempDir()
	// A run's child: a shell that prints claude's result line and exits.
	e := resumableEnv(t, func(agent, baseURL string) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", `echo '{"type":"result","session_id":"sess-1"}'`, "sh"}}, nil
	})
	fill(t, e.store)

	check(t, doc, e, "POST", "/api/v0/events", `{"kind":"x.review.finding","session":"s1","text":"Line 12.","data":{"line":12}}`, true, 201)
	check(t, doc, e, "POST", "/api/v0/turns", `{"agent":"claude","folder":"`+folder+`","prompt":"hi"}`, true, 202)

	var snap store.Snapshot
	if err := json.Unmarshal(check(t, doc, e, "GET", "/api/v0/sessions", "", true, 200), &snap); err != nil {
		t.Fatal(err)
	}
	q := "?store=" + url.QueryEscape(snap.Store) + "&after=0&limit=100"
	var page struct{ Events []map[string]any }
	if err := json.Unmarshal(check(t, doc, e, "GET", "/api/v0/sessions/s1/events"+q, "", true, 200), &page); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, ev := range page.Events {
		kinds[ev["kind"].(string)] = true
	}
	for _, k := range []string{"turn.started", "model.requested", "model.responded", "tool.requested", "tool.completed",
		"turn.completed", "session.registered", "session.opened", "session.closed", "events.dropped", "schema.unexpected", "x.review.finding"} {
		if !kinds[k] {
			t.Errorf("the page has no %s to check", k)
		}
	}
	check(t, doc, e, "GET", "/api/v0/agents", "", true, 200)
	var turns store.TurnList
	if err := json.Unmarshal(check(t, doc, e, "GET", "/api/v0/turns", "", true, 200), &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns.Turns) == 0 {
		t.Error("GET /api/v0/turns listed no turn to check")
	}
	check(t, doc, e, "GET", "/api/v0/turns?session=s1&limit=1", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/turns/t_1/events"+q, "", true, 200)
	check(t, doc, e, "GET", "/api/v0/turns/t_1/events"+q+"&session=s1", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/turns/t_1/events?session=a.b", "", true, 400)
	check(t, doc, e, "GET", "/api/v0/turns?limit=0", "", true, 400)
	check(t, doc, e, "GET", "/api/v0/turns/t_1/events?store=st_other&after=0", "", true, 409)
	if err := e.store.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: time.Now(), Size: 60,
		Request: body.Request{Top: json.RawMessage(`{"model":"claude-sonnet-5-5"}`), Pieces: []body.Piece{
			{Kind: body.System, Label: "text", JSON: json.RawMessage(`"Be brief."`)},
			{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role":"user","content":"hi"}`)}}}}); err != nil {
		t.Fatal(err)
	}
	check(t, doc, e, "GET", "/api/v0/calls/x-1/context", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/context?full=false", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/calls/x-2/context", "", true, 404)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/context?full=maybe", "", true, 400)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/pieces/1", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/pieces/0", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/pieces/2", "", true, 404)
	check(t, doc, e, "GET", "/api/v0/calls/x-1/pieces/reply", "", true, 404)
	var found store.SearchResult
	if err := json.Unmarshal(check(t, doc, e, "GET", "/api/v0/sessions/s1/search?q=login&limit=5", "", true, 200), &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Hits) == 0 {
		t.Error("the search found no hit to check")
	}
	check(t, doc, e, "GET", "/api/v0/sessions/s1/search?q=hi&limit=1", "", true, 200)
	check(t, doc, e, "GET", "/api/v0/sessions/s1/search", "", true, 400)
	check(t, doc, e, "GET", "/api/v0/openapi.json", "", false, 200)

	// Errors.
	check(t, doc, e, "GET", "/api/v0/sessions", "", false, 401)
	check(t, doc, e, "GET", "/api/v0/sessions/s1/events?store=st_other&after=0", "", true, 409)
	check(t, doc, e, "GET", "/api/v0/sessions/s1/events?store=x", "", true, 400)
	check(t, doc, e, "POST", "/api/v0/turns", `{"session":"nobody","prompt":"next"}`, true, 404)
	check(t, doc, e, "POST", "/api/v0/turns", `{"agent":"other","folder":"/","prompt":"hi"}`, true, 400)

	// The stream: every message is framed as the spec says, and its data
	// is an Event.
	ev := doc.Components.Schemas["Event"].Value
	s := openStream(t, e, "?store="+url.QueryEscape(snap.Store)+"&after=0")
	n := 0
	for n < len(page.Events) {
		id, kind, data := s.next(t)
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatalf("data is not JSON: %s", data)
		}
		if want := snap.Store + "/" + strconv.Itoa(int(v["seq"].(float64))); id != want || kind != v["kind"] {
			t.Errorf("framing: id %q event %q, data seq %v kind %v", id, kind, v["seq"], v["kind"])
		}
		var anyV any
		_ = json.Unmarshal([]byte(data), &anyV)
		if err := ev.VisitJSON(anyV, openapi3.EnableJSONSchema2020()); err != nil {
			t.Errorf("a streamed event does not match Event: %v\n%s", err, data)
		}
		n++
	}

	// The Event schema refuses what it should, or it checks nothing.
	for _, bad := range []string{
		`{"seq":1,"id":"a","kind":"turn.completed","time":"2026-11-05T09:00:00Z","exchange":"","vendor":""}`,
		`{"seq":1,"id":"a","kind":"x.Bad","time":"2026-11-05T09:00:00Z","exchange":"","vendor":"program","session":"s"}`,
		`{"id":"a","kind":"turn.started","time":"2026-11-05T09:00:00Z","exchange":"","vendor":""}`,
	} {
		var v any
		_ = json.Unmarshal([]byte(bad), &v)
		if ev.VisitJSON(v, openapi3.EnableJSONSchema2020()) == nil {
			t.Errorf("Event accepts %s", bad)
		}
	}
}
