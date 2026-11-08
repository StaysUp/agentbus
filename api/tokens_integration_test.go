//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// childEnv starts an API whose turns run a shell that writes its environment
// to <dir>/env-<n> and waits for <dir>/release to exist. Session "other" has a
// stored call, x-other, and a turn.
func childEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	script := `n=0; while [ -e "$D/env-$n" ]; do n=$((n+1)); done; env > "$D/env-$n.tmp"; mv "$D/env-$n.tmp" "$D/env-$n"; ` +
		`i=0; while [ ! -e "$D/release" ] && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string) (run.Launch, error) {
		// The service's own environment, as the real Wire passes it, with
		// the main token in it as a person might have exported it.
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "AGENTBUS_TOKEN=" + token, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	e := stackEnv(t, stack{wire: wire})
	t.Cleanup(func() { os.WriteFile(filepath.Join(dir, "release"), nil, 0o600) })
	st := e.store
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
	return e, dir
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
	if vars["AGENTBUS_URL"] != e.srv.URL || !strings.HasPrefix(tok, "abt_") || len(tok) != len("abt_")+64 {
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
		// Not even its own turn: only the main token stops a turn.
		{"POST", "/api/v0/turns/" + s.Turn + "/stop", "", 403},
		{"POST", "/api/v0/turns/t_other/stop", "", 403},
		{"GET", "/api/v0/sessions", "", 403},
		{"GET", "/api/v0/events?session=" + own, "", 403},
		{"GET", "/api/v0/agents", "", 403},
		{"GET", "/api/v0/version", "", 403},
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
	// The refused stop stopped nothing.
	if end, err := e.store.TurnEnd(own, s.Turn); err != nil || end.Stopped || end.ExitCode == nil || *end.ExitCode != 0 {
		t.Errorf("the turn after a turn token's stop: %+v %v, want exit 0, not stopped", end, err)
	}
	if code, _ := as(t, e, tok, "GET", "/api/v0/sessions/"+own+"/events", ""); code != http.StatusUnauthorized {
		t.Fatalf("the token after session.closed: %d, want 401", code)
	}
	if n := e.eng.LiveTokens(); n != 0 {
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

// The token stops working when the child's own process exits, even while a
// process it left behind holds its output open, which keeps the turn from
// closing.
func TestTurnTokenRevokedAtProcessExitNotPipeClose(t *testing.T) {
	dir := t.TempDir()
	script := `sleep 3 & env > "$D/env-0.tmp"; mv "$D/env-0.tmp" "$D/env-0"; exit 0`
	e := resumableEnv(t, func(agent, baseURL string) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"}, Env: []string{"PATH=/usr/bin:/bin", "D=" + dir}}, nil
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
