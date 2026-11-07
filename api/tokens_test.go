package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// childEnv starts an API whose turns run a shell that writes its environment
// to <dir>/env-<n> and waits for <dir>/release to exist. Session "other" has a
// stored call, x-other, and a turn.
func childEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	script := `n=0; while [ -e "$D/env-$n" ]; do n=$((n+1)); done; env > "$D/env-$n.tmp"; mv "$D/env-$n.tmp" "$D/env-$n"; ` +
		`i=0; while [ ! -e "$D/release" ] && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string) (Launch, error) {
		// The service's own environment, as the real Wire passes it, with
		// the main token in it as a person might have exported it.
		return Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "AGENTBUS_TOKEN=" + token, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port, Wire: wire})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() {
		os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
		srv.Close()
		a.StopRuns()
		st.Close()
	})
	now := time.Now().UTC()
	for _, e := range []event.Event{
		{ID: "o1", Kind: event.TurnStarted, Time: now, Session: "other", Turn: "t_other", Text: "secret plans"},
		{ID: "o2", Kind: event.ModelRequested, Time: now, Session: "other", Turn: "t_other", Exchange: "x-other"},
	} {
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.KeepCall(body.Call{Exchange: "x-other", Session: "other", Vendor: "anthropic", Time: now,
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{{Kind: body.Message, Label: "user",
			JSON: json.RawMessage(`{"role":"user","content":"secret plans"}`)}}}}); err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, api: a, store: st, port: port}, dir
}

// childVars waits for the n-th child's environment and returns its
// variables.
func childVars(t *testing.T, dir string, n int) map[string]string {
	t.Helper()
	p := filepath.Join(dir, "env-"+string(rune('0'+n)))
	var b []byte
	for deadline := time.Now().Add(10 * time.Second); ; {
		var err error
		if b, err = os.ReadFile(p); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d wrote no environment", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(string(b), token) {
		t.Fatal("the main token reached the child")
	}
	vars := map[string]string{}
	for _, kv := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	return vars
}

func startTurn(t *testing.T, e *env, body string) Started {
	t.Helper()
	resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(body)))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /turns: %d %s", resp.StatusCode, out)
	}
	var s Started
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// as sends a request with tok in place of the main token.
func as(t *testing.T, e *env, tok, method, path, body string) (int, string) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	var r *http.Request
	if rd != nil {
		r = e.req(t, method, path, rd)
	} else {
		r = e.req(t, method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, out := do(t, r)
	noToken(t, resp, out)
	if strings.Contains(out, tok) {
		t.Fatalf("%s %s: the turn token is in the reply", method, path)
	}
	return resp.StatusCode, out
}

func waitSessionClosed(t *testing.T, st *store.Store, session string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		recs, err := st.After(0, store.Filter{Session: session, Only: true}, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Kind == event.SessionClosed {
				return
			}
		}
	}
	t.Fatal("no session.closed")
}

// A turn's child gets its own token and the service's address. The token reads
// its own session on the read routes and nothing else, and stops working once
// the turn's session.closed is written. The main token is unaffected.
func TestTurnToken(t *testing.T) {
	e, dir := childEnv(t)
	folder := t.TempDir()
	s := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"the first needle"}`)
	vars := childVars(t, dir, 0)
	tok := vars["AGENTBUS_TOKEN"]
	if vars["AGENTBUS_URL"] != e.srv.URL || !strings.HasPrefix(tok, turnTokenPrefix) || len(tok) != len(turnTokenPrefix)+64 {
		t.Fatalf("child AGENTBUS_URL %q, AGENTBUS_TOKEN of %d bytes", vars["AGENTBUS_URL"], len(tok))
	}
	if vars["ANTHROPIC_BASE_URL"] != e.srv.URL+"/s/"+s.ID+"/t/"+s.Turn {
		t.Errorf("child ANTHROPIC_BASE_URL %q", vars["ANTHROPIC_BASE_URL"])
	}

	// A stored call of the turn's own session.
	if _, _, err := e.store.Append(event.Event{ID: "m1", Kind: event.ModelRequested, Time: time.Now(), Session: s.ID, Turn: s.Turn, Exchange: "x-own"}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.KeepCall(body.Call{Exchange: "x-own", Session: s.ID, Vendor: "anthropic", Time: time.Now(),
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{{Kind: body.Message, Label: "user",
			JSON: json.RawMessage(`{"role":"user","content":"the first needle"}`)}}}}); err != nil {
		t.Fatal(err)
	}

	own, other := s.ID, "other"
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v0/turns", "", 200},
		{"GET", "/api/v0/turns?session=" + own, "", 200},
		{"GET", "/api/v0/turns?session=" + other, "", 403},
		{"GET", "/api/v0/turns?session=" + own + "&session=" + other, "", 403},
		{"GET", "/api/v0/turns/" + s.Turn + "/events", "", 200},
		{"GET", "/api/v0/turns/t_other/events?session=" + other, "", 403},
		{"GET", "/api/v0/sessions/" + own + "/events", "", 200},
		{"GET", "/api/v0/sessions/" + other + "/events", "", 403},
		{"GET", "/api/v0/sessions/" + own + "/search?q=needle", "", 200},
		{"GET", "/api/v0/sessions/" + other + "/search?q=secret", "", 403},
		{"GET", "/api/v0/calls/x-own/context", "", 200},
		{"GET", "/api/v0/calls/x-own/pieces/0", "", 200},
		{"GET", "/api/v0/calls/x-other/context", "", 403},
		{"GET", "/api/v0/calls/x-other/pieces/0", "", 403},
		{"GET", "/api/v0/calls/x-none/pieces/0", "", 404},
		{"GET", "/api/v0/openapi.json", "", 200},
		{"GET", "/docs/llms.txt", "", 200},
		// Writes, and every read that is not limited to one session.
		{"POST", "/api/v0/turns", `{"session":"` + own + `","prompt":"again"}`, 403},
		{"POST", "/api/v0/turns", `{"agent":"claude","folder":"` + folder + `","prompt":"new"}`, 403},
		{"POST", "/api/v0/events", `{"kind":"x.a.b","session":"` + own + `","text":"hi"}`, 403},
		{"GET", "/api/v0/sessions", "", 403},
		{"GET", "/api/v0/events?session=" + own, "", 403},
		{"GET", "/api/v0/agents", "", 403},
	} {
		code, out := as(t, e, tok, tc.method, tc.path, tc.body)
		if code != tc.want {
			t.Errorf("turn token %s %s: %d %s, want %d", tc.method, tc.path, code, out, tc.want)
		}
		if strings.Contains(out, "secret plans") {
			t.Errorf("turn token %s %s: another session's text in the reply", tc.method, tc.path)
		}
	}
	// Without session, GET /turns lists the token's own session only.
	_, out := as(t, e, tok, "GET", "/api/v0/turns", "")
	var list store.TurnList
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Turns) != 1 || list.Turns[0].Session != own {
		t.Errorf("GET /turns with a turn token: %s", out)
	}
	// The main token still reads everything, with or without a turn running.
	for _, p := range []string{"/api/v0/sessions", "/api/v0/turns?session=other", "/api/v0/calls/x-other/pieces/0", "/api/v0/sessions/other/search?q=secret"} {
		if resp, out := do(t, e.req(t, "GET", p, nil)); resp.StatusCode != 200 {
			t.Errorf("main token GET %s: %d %s", p, resp.StatusCode, out)
		}
	}

	// The session's next turn gets a token of its own.
	os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
	waitSessionClosed(t, e.store, own)
	if code, _ := as(t, e, tok, "GET", "/api/v0/sessions/"+own+"/events", ""); code != http.StatusUnauthorized {
		t.Fatalf("the token after session.closed: %d, want 401", code)
	}
	if n := e.api.turnTokens.live(); n != 0 {
		t.Errorf("%d live tokens after the turn", n)
	}
	os.Remove(filepath.Join(dir, "release"))
	s2 := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"second"}`)
	tok2 := childVars(t, dir, 1)["AGENTBUS_TOKEN"]
	if tok2 == "" || tok2 == tok {
		t.Fatal("the second turn did not get a new token")
	}
	if code, _ := as(t, e, tok2, "GET", "/api/v0/sessions/"+s2.ID+"/events", ""); code != 200 {
		t.Errorf("the second turn's token: %d", code)
	}
	if code, _ := as(t, e, tok2, "GET", "/api/v0/sessions/"+own+"/events", ""); code != 403 {
		t.Errorf("the second turn's token on the first turn's session: %d, want 403", code)
	}
	if code, _ := as(t, e, tok, "GET", "/api/v0/sessions/"+own+"/events", ""); code != 401 {
		t.Errorf("the first token while a second turn runs: %d, want 401", code)
	}
}

// A turn whose child cannot start leaves no token behind.
func TestTurnTokenRevokedWhenChildDoesNotStart(t *testing.T) {
	e := resumableEnv(t, func(agent, baseURL string) (Launch, error) {
		return Launch{Path: filepath.Join(t.TempDir(), "missing")}, nil
	})
	resp, out := do(t, e.req(t, "POST", "/api/v0/turns", strings.NewReader(`{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"x"}`)))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if n := e.api.turnTokens.live(); n != 0 {
		t.Errorf("%d live tokens after a child that did not start", n)
	}
}

func TestTurnEnv(t *testing.T) {
	got := turnEnv(nil, []string{"A=1", "AGENTBUS_TOKEN=main", "AGENTBUS_URL=x"}, "http://127.0.0.1:1", "abt_1")
	if strings.Join(got, " ") != "A=1 AGENTBUS_URL=http://127.0.0.1:1 AGENTBUS_TOKEN=abt_1" {
		t.Errorf("turnEnv: %v", got)
	}
}

// GET /api/v0/sessions/{id}/search refuses a bad query, and pages.
func TestSearchRoute(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	for i, text := range []string{"one needle", "two needle", "three needle"} {
		if _, _, err := e.store.Append(event.Event{ID: "e" + string(rune('0'+i)), Kind: event.TurnStarted, Time: now,
			Session: "s1", Turn: "t_" + string(rune('a'+i)), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(q string) (int, store.SearchResult, string) {
		resp, out := do(t, e.req(t, "GET", "/api/v0/sessions/s1/search"+q, nil))
		var res store.SearchResult
		_ = json.Unmarshal([]byte(out), &res)
		return resp.StatusCode, res, out
	}
	for _, q := range []string{"", "?q=", "?q=a%00b", "?q=%ff", "?q=" + strings.Repeat("a", 257), "?q=a&q=b",
		"?q=a&limit=0", "?q=a&limit=201", "?q=a&before=x", "?q=a&before=1"} {
		if code, _, out := get(q); code != 400 {
			t.Errorf("%q: %d %s, want 400", q, code, out)
		}
	}
	code, res, out := get("?q=NEEDLE&limit=2")
	if code != 200 || len(res.Hits) != 2 || res.Hits[0].Snippet != "three needle" || res.NextBefore == nil {
		t.Fatalf("page 1: %d %s", code, out)
	}
	code, res, out = get("?q=NEEDLE&limit=2&before=" + url.QueryEscape(*res.NextBefore))
	if code != 200 || len(res.Hits) != 1 || res.Hits[0].Snippet != "one needle" || res.NextBefore != nil {
		t.Fatalf("page 2: %d %s", code, out)
	}
	if code, res, _ := get("?q=haystack"); code != 200 || len(res.Hits) != 0 || res.NextBefore != nil {
		t.Errorf("no match: %d %+v", code, res)
	}
}

// GET /api/v0/calls/{id}/pieces/{n} answers the stored bytes, unchanged.
func TestCallPieceRoute(t *testing.T) {
	e := newEnv(t)
	msg := `{"role":"user", "content":"<a> & é"}`
	if err := e.store.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: time.Now(),
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{
			{Kind: body.System, Label: "text", JSON: json.RawMessage(`"Be brief."`)},
			{Kind: body.Message, Label: "user", JSON: json.RawMessage(msg)}}}}); err != nil {
		t.Fatal(err)
	}
	resp, out := do(t, e.req(t, "GET", "/api/v0/calls/x-1/pieces/1", nil))
	sum := sha256.Sum256([]byte(msg))
	h := resp.Header
	if resp.StatusCode != 200 || out != msg || h.Get("Content-Type") != "application/json" ||
		h.Get("X-Piece-Hash") != hex.EncodeToString(sum[:]) || h.Get("X-Piece-Kind") != "message" ||
		h.Get("X-Piece-Label") != "user" || h.Get("X-Piece-Count") != "2" {
		t.Fatalf("piece 1: %d %q %v", resp.StatusCode, out, h)
	}
	if resp, out := do(t, e.req(t, "GET", "/api/v0/calls/x-1/pieces/0", nil)); resp.StatusCode != 200 || out != `"Be brief."` {
		t.Errorf("piece 0: %d %q", resp.StatusCode, out)
	}
	for _, p := range []string{"/api/v0/calls/x-1/pieces/2", "/api/v0/calls/x-1/pieces/reply", "/api/v0/calls/x-2/pieces/0",
		"/api/v0/calls/x-1/pieces/01", "/api/v0/calls/x-1/pieces/-1"} {
		if resp, out := do(t, e.req(t, "GET", p, nil)); resp.StatusCode != 404 {
			t.Errorf("%s: %d %s, want 404", p, resp.StatusCode, out)
		}
	}
	reply := `{"role":"assistant","content":[{"type":"text","text":"ok"}]}`
	if err := e.store.KeepReply(body.Reply{Exchange: "x-1", Status: 200, Stop: "end_turn", Time: time.Now(), Message: json.RawMessage(reply)}); err != nil {
		t.Fatal(err)
	}
	if resp, out := do(t, e.req(t, "GET", "/api/v0/calls/x-1/pieces/reply", nil)); resp.StatusCode != 200 || out != reply ||
		resp.Header.Get("X-Piece-Label") != "assistant" {
		t.Errorf("reply: %d %q", resp.StatusCode, out)
	}
}

// The token stops working when the child's own process exits, even while a
// process it left behind holds its output open, which keeps the turn from
// closing.
func TestTurnTokenRevokedAtProcessExitNotPipeClose(t *testing.T) {
	dir := t.TempDir()
	script := `sleep 3 & env > "$D/env-0.tmp"; mv "$D/env-0.tmp" "$D/env-0"; exit 0`
	e := resumableEnv(t, func(agent, baseURL string) (Launch, error) {
		return Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"}, Env: []string{"PATH=/usr/bin:/bin", "D=" + dir}}, nil
	})
	s := startTurn(t, e, `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"x"}`)
	tok := childVars(t, dir, 0)["AGENTBUS_TOKEN"]
	revoked := false
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if code, _ := as(t, e, tok, "GET", "/api/v0/sessions/"+s.ID+"/events", ""); code == http.StatusUnauthorized {
			revoked = true
			break
		}
	}
	if !revoked {
		t.Fatal("the token still works 1.5s after the child exited")
	}
	// The left-behind sleep still holds the output, so the turn is not
	// closed yet: the token ended with the process, not with the pipes.
	recs, err := e.store.After(0, store.Filter{Session: s.ID, Only: true}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Kind == event.SessionClosed {
			t.Fatal("session.closed already stored; the test no longer holds the pipes open")
		}
	}
	waitSessionClosed(t, e.store, s.ID)
}

// With an origin configured, a piece's headers are exposed to that origin's
// browser cockpit; without one, nothing is exposed.
func TestCallPieceExposesHeadersToOrigin(t *testing.T) {
	e := newEnv(t) // configured with origin
	if err := e.store.KeepCall(body.Call{Exchange: "x-1", Session: "s1", Vendor: "anthropic", Time: time.Now(),
		Request: body.Request{Top: json.RawMessage(`{}`), Pieces: []body.Piece{{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role":"user","content":"hi"}`)}}}}); err != nil {
		t.Fatal(err)
	}
	r := e.req(t, "GET", "/api/v0/calls/x-1/pieces/0", nil)
	r.Header.Set("Origin", origin)
	resp, _ := do(t, r)
	exp := resp.Header.Get("Access-Control-Expose-Headers")
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("%d, allow-origin %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	for _, h := range []string{"X-Piece-Kind", "X-Piece-Hash", "X-Piece-Count", "X-Piece-Label"} {
		if !strings.Contains(exp, h) {
			t.Errorf("Access-Control-Expose-Headers %q lacks %s", exp, h)
		}
	}
	r = e.req(t, "GET", "/api/v0/calls/x-1/pieces/0", nil)
	r.Header.Set("Origin", "http://evil.example")
	if resp, _ := do(t, r); resp.Header.Get("Access-Control-Expose-Headers") != "" {
		t.Error("headers exposed to an origin that is not allowed")
	}
}
