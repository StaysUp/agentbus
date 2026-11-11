package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// getJSON GETs path with the main token into v, and answers the status.
func getJSON(t *testing.T, e *env, path string, v any) int {
	t.Helper()
	resp, out := do(t, e.req(t, "GET", path, nil))
	if resp.StatusCode == http.StatusOK && v != nil {
		if err := json.Unmarshal([]byte(out), v); err != nil {
			t.Fatalf("GET %s: %v %s", path, err, out)
		}
	}
	return resp.StatusCode
}

// A turn is known from the moment it is asked for: with no runner, POST
// /turns answers its id and state queued, GET /turns/{id} answers it queued,
// GET /turns lists it first, and a stop makes it read stopped at once.
func TestQueuedTurnIsKnownAndStops(t *testing.T) {
	e := newJobsEnv(t)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"/tmp","prompt":"wait for a runner"}`)))
	var st Started
	if resp.StatusCode != 202 || json.Unmarshal([]byte(raw), &st) != nil || st.State != "queued" || st.Turn == "" || st.Session == "" || st.BaseURL != "" {
		t.Fatalf("POST /turns: %d %s", resp.StatusCode, raw)
	}
	var one OneTurn
	if code := getJSON(t, e, "/api/v0/turns/"+st.Turn, &one); code != 200 || one.Turn.State != "queued" || one.Turn.Session != st.Session ||
		one.Turn.Prompt != "wait for a runner" || one.Turn.Kind != "turn" || one.Turn.Asked == nil || one.Turn.Number != 0 || !one.Turn.Started.IsZero() {
		t.Fatalf("GET the queued turn: %d %+v", code, one.Turn)
	}
	// It has not started, so it has no started; asked says when it was asked.
	if _, raw := do(t, e.req(t, "GET", "/api/v0/turns/"+st.Turn, nil)); strings.Contains(raw, `"started"`) || !strings.Contains(raw, `"asked"`) {
		t.Errorf("the queued turn: %s", raw)
	}
	appendAll(t, e.store, event.Event{ID: "e1", Kind: event.TurnStarted, Time: time.Now().UTC(), Session: "other", Turn: "t_old", Text: "older"})
	var list TurnList
	if code := getJSON(t, e, "/api/v0/turns", &list); code != 200 || len(list.Turns) != 2 || list.Turns[0].ID != st.Turn || list.Turns[1].ID != "t_old" {
		t.Fatalf("GET /turns: %d %+v, want the queued turn first", code, list.Turns)
	}
	if code := getJSON(t, e, "/api/v0/turns?session="+st.Session, &list); code != 200 || len(list.Turns) != 1 || list.Turns[0].ID != st.Turn {
		t.Fatalf("GET /turns?session=: %d %+v", code, list.Turns)
	}
	if code := getJSON(t, e, "/api/v0/turns?session=other", &list); code != 200 || len(list.Turns) != 1 || list.Turns[0].ID != "t_old" {
		t.Fatalf("GET /turns?session=other: %d %+v", code, list.Turns)
	}
	resp, raw = do(t, e.req(t, "POST", "/api/v0/turns/"+st.Turn+"/stop", nil))
	var stopping Stopping
	if resp.StatusCode != 202 || json.Unmarshal([]byte(raw), &stopping) != nil || stopping.Turn != st.Turn || stopping.Session != st.Session {
		t.Fatalf("stop the queued turn: %d %s", resp.StatusCode, raw)
	}
	if code := getJSON(t, e, "/api/v0/turns/"+st.Turn, &one); code != 200 || one.Turn.State != "stopped" || !one.Turn.Stopped || one.Turn.Ended == nil {
		t.Fatalf("the stopped queued turn: %d %+v", code, one.Turn)
	}
	// Once ended it is still listed, as stopped, and a second stop is 409.
	if code := getJSON(t, e, "/api/v0/turns?session="+st.Session, &list); code != 200 || len(list.Turns) != 1 ||
		list.Turns[0].ID != st.Turn || list.Turns[0].State != "stopped" {
		t.Fatalf("GET /turns after the stop: %d %+v", code, list.Turns)
	}
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns/"+st.Turn+"/stop", nil)); resp.StatusCode != 409 {
		t.Fatalf("a second stop: %d %s", resp.StatusCode, raw)
	}
}

// A turn stopped while queued never started, so the record holds none: the
// list shows it where it was asked for, among the turns that started, on
// exactly one page.
func TestStoppedQueuedTurnIsListed(t *testing.T) {
	e := newJobsEnv(t)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"/tmp","prompt":"stop me"}`)))
	var st Started
	if resp.StatusCode != 202 || json.Unmarshal([]byte(raw), &st) != nil || st.State != "queued" {
		t.Fatalf("POST /turns: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns/"+st.Turn+"/stop", nil)); resp.StatusCode != 202 {
		t.Fatalf("stop: %d %s", resp.StatusCode, raw)
	}
	// It has no changes to apply, and the refusal says it never started.
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns/"+st.Turn+"/apply", nil)); resp.StatusCode != 400 || !strings.Contains(raw, applyNeverStarted) {
		t.Errorf("apply of a turn that never started: %d %s", resp.StatusCode, raw)
	}
	now := time.Now().UTC()
	appendAll(t, e.store,
		event.Event{ID: "e1", Kind: event.TurnStarted, Time: now.Add(-time.Hour), Session: st.Session, Turn: "t_old", Text: "older"},
		event.Event{ID: "e2", Kind: event.TurnStarted, Time: now.Add(time.Hour), Session: st.Session, Turn: "t_new", Text: "newer"})
	ids := func(l TurnList) []string {
		var out []string
		for _, tn := range l.Turns {
			out = append(out, tn.ID+":"+tn.State)
		}
		return out
	}
	var list TurnList
	for _, path := range []string{"/api/v0/turns", "/api/v0/turns?session=" + st.Session} {
		if code := getJSON(t, e, path, &list); code != 200 || len(list.Turns) != 3 || list.Turns[0].ID != "t_new" ||
			list.Turns[1].ID != st.Turn || list.Turns[1].State != "stopped" || list.Turns[2].ID != "t_old" {
			t.Fatalf("GET %s: %d %v", path, code, ids(list))
		}
	}
	if code := getJSON(t, e, "/api/v0/turns?session=other", &list); code != 200 || len(list.Turns) != 0 {
		t.Fatalf("another session: %d %v", code, ids(list))
	}
	// One turn a page: the stopped turn is on the second page only.
	var seen []string
	path := "/api/v0/turns?limit=1&session=" + st.Session
	for range 4 {
		if code := getJSON(t, e, path, &list); code != 200 {
			t.Fatalf("GET %s: %d", path, code)
		}
		seen = append(seen, ids(list)...)
		if list.NextBefore == nil {
			break
		}
		path = "/api/v0/turns?limit=1&session=" + st.Session + "&before=" + strconv.FormatInt(*list.NextBefore, 10)
	}
	if want := []string{"t_new:running", st.Turn + ":stopped", "t_old:running"}; strings.Join(seen, " ") != strings.Join(want, " ") {
		t.Errorf("pages: %v, want %v", seen, want)
	}
}

// Pages go down by seq, and a later turn can carry an earlier clock (another
// session, another runner): a turn that never started is still on exactly one
// page of the unfiltered list (agy review round 2, 2026-11-11).
func TestNeverStartedOnOnePageAcrossClocks(t *testing.T) {
	e := newJobsEnv(t)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"/tmp","prompt":"stop me"}`)))
	var st Started
	if resp.StatusCode != 202 || json.Unmarshal([]byte(raw), &st) != nil {
		t.Fatalf("POST /turns: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns/"+st.Turn+"/stop", nil)); resp.StatusCode != 202 {
		t.Fatalf("stop: %d %s", resp.StatusCode, raw)
	}
	// The stopped turn was asked for now. In seq order the started turns'
	// clocks go: 2h ago, 2h ahead, 1h ago, and equal to another.
	now := time.Now().UTC()
	appendAll(t, e.store,
		event.Event{ID: "e1", Kind: event.TurnStarted, Time: now.Add(-2 * time.Hour), Session: "sa", Turn: "t_1", Text: "one"},
		event.Event{ID: "e2", Kind: event.TurnStarted, Time: now.Add(2 * time.Hour), Session: "sb", Turn: "t_2", Text: "two"},
		event.Event{ID: "e3", Kind: event.TurnStarted, Time: now.Add(-time.Hour), Session: "sa", Turn: "t_3", Text: "three"},
		event.Event{ID: "e4", Kind: event.TurnStarted, Time: now.Add(-time.Hour), Session: "sc", Turn: "t_4", Text: "four"})
	for limit := 1; limit <= 4; limit++ {
		seen := map[string]int{}
		path := "/api/v0/turns?limit=" + strconv.Itoa(limit)
		for range 10 {
			var list TurnList
			if code := getJSON(t, e, path, &list); code != 200 {
				t.Fatalf("GET %s: %d", path, code)
			}
			for _, tn := range list.Turns {
				seen[tn.ID]++
			}
			if list.NextBefore == nil {
				break
			}
			path = "/api/v0/turns?limit=" + strconv.Itoa(limit) + "&before=" + strconv.FormatInt(*list.NextBefore, 10)
		}
		for _, id := range []string{st.Turn, "t_1", "t_2", "t_3", "t_4"} {
			if seen[id] != 1 {
				t.Errorf("limit %d: %s listed %d times: %v", limit, id, seen[id], seen)
			}
		}
	}
}

// A queued turn says who asked for it, as its turn.started will once it
// starts, without the queue's job id.
func TestQueuedTurnOrigin(t *testing.T) {
	e := newJobsEnv(t)
	asked, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "", queue.Spec{Harness: "claude", Prompt: "asked"},
		queue.Links{Asker: &queue.Asker{Session: "s_ask", Turn: "t_ask"}, Chain: &queue.Chain{Root: "s_ask", Depth: 1}},
		queue.Limits{MaxDepth: 2, MaxPerTurn: 10, MaxPerTree: 30})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Session: "s_ask", Prompt: "[AgentBus] done"},
		queue.Links{ResultOf: &queue.ResultOf{Job: "job_x", Kind: queue.KindReview, Session: "s_work", Turn: "t_work", Agent: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	var one OneTurn
	if code := getJSON(t, e, "/api/v0/turns/"+asked.Turn, &one); code != 200 || one.Turn.Origin == nil ||
		*one.Turn.Origin != (event.Origin{Kind: event.OriginAsked, Session: "s_ask", Turn: "t_ask"}) {
		t.Fatalf("an asked turn, queued: %d %+v", code, one.Turn.Origin)
	}
	if code := getJSON(t, e, "/api/v0/turns/"+result.Turn, &one); code != 200 || one.Turn.Origin == nil ||
		*one.Turn.Origin != (event.Origin{Kind: event.OriginResult, Session: "s_work", Turn: "t_work", TurnKind: queue.KindReview, Agent: "codex"}) {
		t.Fatalf("a delivered result, queued: %d %+v", code, one.Turn.Origin)
	}
}

// A turn's number is its place in its session, from 1, in the order its
// turns started; each session counts its own.
func TestTurnNumber(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	appendAll(t, e.store,
		event.Event{ID: "e1", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_a", Text: "one"},
		event.Event{ID: "e2", Kind: event.TurnStarted, Time: now, Session: "s2", Turn: "t_x", Text: "other"},
		event.Event{ID: "e3", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_b", Text: "two"},
		event.Event{ID: "e4", Kind: event.TurnStarted, Time: now, Session: "s1", Turn: "t_c", Text: "three"})
	var list TurnList
	if code := getJSON(t, e, "/api/v0/turns", &list); code != 200 {
		t.Fatal(code)
	}
	got := map[string]int{}
	for _, tn := range list.Turns {
		got[tn.ID] = tn.Number
	}
	if got["t_a"] != 1 || got["t_b"] != 2 || got["t_c"] != 3 || got["t_x"] != 1 {
		t.Errorf("numbers %v", got)
	}
	var one OneTurn
	if code := getJSON(t, e, "/api/v0/turns/t_b", &one); code != 200 || one.Turn.Number != 2 || one.Turn.Kind != "turn" {
		t.Errorf("GET /turns/t_b: %d %+v", code, one.Turn)
	}
}

// A review's refusals: a reviewer of the wrong shape, an unknown field, a
// turn this service does not know, and a turn that has not ended.
func TestReviewRefusals(t *testing.T) {
	e := newJobsEnv(t)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"/tmp","prompt":"x"}`)))
	var st Started
	if resp.StatusCode != 202 || json.Unmarshal([]byte(raw), &st) != nil {
		t.Fatalf("POST /turns: %d %s", resp.StatusCode, raw)
	}
	for _, c := range []struct {
		turn, body string
		want       int
		says       string
	}{
		{st.Turn, `{}`, 400, "agent is required"},
		{st.Turn, `{"model":"m"}`, 400, "agent is required"},
		{st.Turn, `{"agent":"-x"}`, 400, "agent must match"},
		{st.Turn, `{"agent":"claude","model":"a b"}`, 400, "model must match"},
		{st.Turn, `{"agent":"claude","extra":1}`, 400, ""},
		{st.Turn, `{"agent":"claude","idempotency_key":"a b"}`, 400, ""},
		{st.Turn, `not json`, 400, ""},
		{"t_none", `{"agent":"claude"}`, 404, ""},
		// Queued: it has not ended, so it cannot be reviewed.
		{st.Turn, `{"agent":"claude"}`, 409, ""},
	} {
		if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns/"+c.turn+"/review", strings.NewReader(c.body))); resp.StatusCode != c.want || !strings.Contains(raw, c.says) {
			t.Errorf("review %s %s: %d %s, want %d %q", c.turn, c.body, resp.StatusCode, raw, c.want, c.says)
		}
	}
}

// The runner protocol is described on its own, never in the client API's
// description, and both pages and both plain files are served without the
// token.
func TestRunnerProtocolDescribedApart(t *testing.T) {
	doc, rdoc := loadSpec(t), loadRunnerSpec(t)
	for p := range doc.Paths.Map() {
		if strings.HasPrefix(p, "/api/v0/runner/") {
			t.Errorf("the client API describes %s", p)
		}
	}
	for _, tg := range doc.Tags {
		if tg.Name == "Runners" || tg.Name == "Attempts" || tg.Name == "Blobs" {
			t.Errorf("the client API has the tag %s", tg.Name)
		}
	}
	n := 0
	for p, item := range rdoc.Paths.Map() {
		if !strings.HasPrefix(p, "/api/v0/runner/") {
			t.Errorf("the runner protocol describes %s", p)
		}
		for m, op := range item.Operations() {
			n++
			public := op.Security != nil && len(*op.Security) == 0
			if public != (p == "/api/v0/runner/openapi.json") {
				t.Errorf("%s %s: public = %v", m, p, public)
			}
		}
	}
	if n != 18 {
		t.Errorf("the runner protocol has %d operations, want 18", n)
	}
	e := newEnv(t)
	for p, want := range map[string]string{
		"/api/v0/openapi.json":        "agentbus client API",
		"/api/v0/runner/openapi.json": "agentbus runner protocol",
		"/docs":                       "/api/v0/openapi.json",
		"/docs/runner":                "/api/v0/runner/openapi.json",
		"/docs/llms.txt":              "/api/v0/turns",
		"/docs/reference.md":          "/api/v0/turns",
	} {
		r, _ := http.NewRequest("GET", e.srv.URL+p, nil)
		resp, out := do(t, r)
		if resp.StatusCode != 200 || !strings.Contains(out, want) {
			t.Errorf("GET %s without a token: %d, want 200 with %q", p, resp.StatusCode, want)
		}
	}
}

// A turn runs until its job has ended: the record says finished as the agent
// exits, and the turn still reads running until its runner reports its
// changes. A stopped turn reads stopped at once.
func TestTurnRunsUntilItsJobEnds(t *testing.T) {
	a := &API{}
	now := time.Now()
	ended := now.Add(time.Second)
	for _, c := range []struct {
		record   string
		jobEnded *time.Time
		want     string
	}{
		{store.TurnFinished, nil, queue.TurnRunning},
		{store.TurnFailed, nil, queue.TurnRunning},
		{store.TurnStopped, nil, queue.TurnStopped},
		{store.TurnFinished, &ended, store.TurnFinished},
		{store.TurnStopped, &ended, store.TurnStopped},
	} {
		j := queue.Job{Kind: queue.KindTurn, Created: now, Ended: c.jobEnded}
		v := a.view(store.Turn{ID: "t_1", State: c.record, Ended: &ended}, &j)
		if v.State != c.want || (v.State == queue.TurnRunning) != (v.Ended == nil) {
			t.Errorf("record %s, job ended %v: state %s, ended %v; want %s", c.record, c.jobEnded != nil, v.State, v.Ended, c.want)
		}
	}
}
