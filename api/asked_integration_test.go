//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// askEnv starts an API whose turns run a shell. Each turn writes its
// environment to <dir>/env-<prompt> and its working folder to
// <dir>/pwd-<prompt>, then waits for <dir>/release-<prompt>. A prompt
// "quick", and a review, end at once; a delivered result ("[AgentBus] ...") is written to
// <dir>/delivered-<pid> and ends at once. Every turn prints a resume id.
func askEnv(t *testing.T) (*env, string) {
	t.Helper()
	return askEnvWith(t, "")
}

// askEnvWith is askEnv with permissions.toml at perm ("" for none).
func askEnvWith(t *testing.T, perm string) (*env, string) {
	return askEnvCap(t, perm, 16)
}

// askEnvCap is askEnvWith with a chosen runner capacity, so a test can hold the
// one slot and keep an asked turn queued.
func askEnvCap(t *testing.T, perm string, maxRuns int) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	script := `for a; do p="$a"; done; echo '{"type":"result","session_id":"sid-'$$'"}'; ` +
		`case "$p" in "[AgentBus]"*) printf '%s' "$p" > "$D/delivered-$$"; exit 0;; quick|"Review a change"*) exit 0;; esac; ` +
		`printf '%s\n' "$@" > "$D/args-$p"; env > "$D/env-$p.tmp"; pwd > "$D/pwd-$p"; mv "$D/env-$p.tmp" "$D/env-$p"; ` +
		`i=0; while [ ! -e "$D/release-$p" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	return stackEnv(t, stack{wire: wire, maxRuns: maxRuns, permissions: perm}), dir
}

// firstTurn runs the three calls a first turn now needs (POST /workspaces, POST
// /sessions, POST /turns) and returns the started turn, for an integration test
// that used one-call POST /turns with a folder. sandbox picks the workspace
// mode (true: a copy behind a wall; false: the folder as it is).
func (e *env) firstTurn(t *testing.T, folder, agent, prompt string, sandbox bool) Started {
	t.Helper()
	sess := e.newSession(t, folder, sandbox)
	return startTurn(t, e, fmt.Sprintf(`{"session":%q,"agent":%q,"prompt":%q}`, sess, agent, prompt))
}

// tokenOf waits for the turn with this prompt and returns its token.
func tokenOf(t *testing.T, dir, prompt string) string {
	t.Helper()
	p := filepath.Join(dir, "env-"+prompt)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b, err := os.ReadFile(p)
		if err == nil {
			if strings.Contains(string(b), token) {
				t.Fatal("the main token reached the child")
			}
			for _, kv := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(kv, "AGENTBUS_TOKEN="); ok {
					return v
				}
			}
			t.Fatalf("turn %q has no token", prompt)
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %q wrote no environment", prompt)
		}
	}
}

func release(dir, prompt string) { os.WriteFile(filepath.Join(dir, "release-"+prompt), nil, 0o600) }

func pwdOf(t *testing.T, dir, prompt string) string {
	t.Helper()
	p := filepath.Join(dir, "pwd-"+prompt)
	for deadline := time.Now().Add(10 * time.Second); ; {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %q wrote no working folder", prompt)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gitRepo is a folder in a git repo with one commit.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "first"}} {
		if args[0] == "add" {
			os.WriteFile(filepath.Join(dir, "app.txt"), []byte("one\n"), 0o600)
		}
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	real, _ := filepath.EvalSymlinks(dir)
	return real
}

// deliverUntil runs the delivery loop, as agentbus up does every second,
// until the session has want delivered-result turns, and returns them.
// waitAskedTurn waits for an asked session's one turn to be stored, which under
// (d) happens when a runner starts it, after startTurn returned it queued.
func waitAskedTurn(t *testing.T, e *env, session string) store.Turn {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		list, err := e.store.Turns(store.TurnQuery{Session: session, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Turns) >= 1 {
			return list.Turns[0]
		}
	}
	t.Fatalf("asked session %s stored no turn", session)
	return store.Turn{}
}

func deliverUntil(t *testing.T, e *env, session string, want int) []store.Turn {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		// Mirror the sweep: record each ended turn's end, then fire the ons
		// that read it from the record (C0).
		if _, _, err := jobs.RecordTurnEnds(e.q, e.store, nil, time.Now().Add(time.Hour), 200); err != nil {
			t.Fatal(err)
		}
		if _, err := e.q.FireOns(queue.DefaultTeam, e.store.TurnEnded, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		list, err := e.store.Turns(store.TurnQuery{Session: session, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var got []store.Turn
		for _, tn := range list.Turns {
			if tn.Origin != nil && tn.Origin.Kind == event.OriginResult {
				got = append(got, tn)
			}
		}
		if len(got) >= want {
			return got
		}
	}
	t.Fatalf("fewer than %d results delivered to %s", want, session)
	return nil
}

// ask does what the ask_agent app does over the public API: start a turn, then
// register the on that delivers its reply to the asking session (B15: delivery
// is no longer automatic on POST /turns; the app registers the on itself). The
// turn token may register an on for the turn its own ask just started, so this
// is exactly the app's two steps, here spelled out. TestAskAgentApp drives the
// real app through MCP end to end.
// ask drives the two steps ask_agent runs (his API rules, 2026-11-21): a turn
// token opens a session in its own workspace, then starts the asked turn in it,
// so the asked agent shares the asker's copy. body is the turn body with its
// agent and prompt; ask injects the session it opened. It then registers a
// delivery on, as the app does, and returns the asked turn.
func ask(t *testing.T, e *env, tok, body string) Started {
	t.Helper()
	// A turn's idempotency key covers both steps, so a retry reuses the same
	// session and the same turn, as the ask_agent app does.
	var fields struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	_ = json.Unmarshal([]byte(body), &fields)
	sessBody := `{}`
	if fields.IdempotencyKey != "" {
		sessBody = `{"idempotency_key":"` + fields.IdempotencyKey + `:s"}`
	}
	code, out := as(t, e, tok, "POST", "/api/v0/sessions", sessBody)
	if code != 201 {
		t.Fatalf("ask: open a session in the asker's workspace: %d %s", code, out)
	}
	var sc SessionCreated
	if err := json.Unmarshal([]byte(out), &sc); err != nil {
		t.Fatal(err)
	}
	turnBody := strings.Replace(body, "{", `{"session":"`+sc.Session+`",`, 1)
	code, out = as(t, e, tok, "POST", "/api/v0/turns", turnBody)
	if code != 202 {
		t.Fatalf("ask %s: %d %s", turnBody, code, out)
	}
	var s Started
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatal(err)
	}
	if code, out := as(t, e, tok, "POST", "/api/v0/ons", `{"turn":"`+s.Turn+`"}`); code != 201 {
		t.Fatalf("register delivery on for %s: %d %s", s.Turn, code, out)
	}
	return s
}

// askRaw does ask's two steps but returns the turn POST's status and body
// instead of asserting it started, for a test that expects the start to be
// capped (depth, per-turn jobs, cost): the cap fires when the turn is created,
// so the session is opened first and the turn refused.
func askRaw(t *testing.T, e *env, tok, body string) (int, string) {
	t.Helper()
	code, out := as(t, e, tok, "POST", "/api/v0/sessions", `{}`)
	if code != 201 {
		t.Fatalf("askRaw: open a session: %d %s", code, out)
	}
	var sc SessionCreated
	if err := json.Unmarshal([]byte(out), &sc); err != nil {
		t.Fatal(err)
	}
	return as(t, e, tok, "POST", "/api/v0/turns", strings.Replace(body, "{", `{"session":"`+sc.Session+`",`, 1))
}

// callAskAgent drives the ask_agent app through the MCP endpoint with a turn's
// own token and returns the started turn and session from the app's result.
func callAskAgent(t *testing.T, e *env, tok, body string) (turn, session string) {
	t.Helper()
	code, out := as(t, e, tok, "POST", "/api/v0/mcp", body)
	if code != 200 {
		t.Fatalf("mcp ask_agent: %d %s", code, out)
	}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("mcp reply %s: %v", out, err)
	}
	if env.Result.IsError || len(env.Result.Content) == 0 {
		t.Fatalf("ask_agent app failed: %s", out)
	}
	var r struct{ Turn, Session string }
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &r); err != nil {
		t.Fatalf("app output %q: %v", env.Result.Content[0].Text, err)
	}
	return r.Turn, r.Session
}

// The real ask_agent app, end to end through MCP: one tool call starts the
// turn and registers the delivery on (its two Arazzo steps, apps.go), the
// asked turn says who asked it, a retry returns the same turn (the derived
// idempotency key), and the reply is delivered back to the asking session.
func TestAskAgentAppDelivers(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"ask_agent\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	repo := gitRepo(t)
	root := e.firstTurn(t, repo, "claude", "root", true)
	tok := tokenOf(t, dir, "root")

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":"quick"}}}`
	turn, session := callAskAgent(t, e, tok, call)
	if turn == "" || session == "" {
		t.Fatalf("ask_agent returned turn=%q session=%q", turn, session)
	}
	// startTurn no longer waits (wait=false, (d)): the asked turn is queued at
	// return, so wait for the runner to start it before reading its event.
	asked := waitAskedTurn(t, e, session)
	if o := asked.Origin; o == nil || o.Kind != event.OriginAsked || o.Turn != root.Turn {
		t.Errorf("asked turn origin %+v, want asked by %s", o, root.Turn)
	}
	// A retry (the SAME JSON-RPC id) returns the same turn, not a second one.
	if turn2, _ := callAskAgent(t, e, tok, call); turn2 != turn {
		t.Fatalf("a retry started a new turn: %s vs %s", turn2, turn)
	}
	// A FRESH call (a different JSON-RPC id) with the same arguments is a new
	// ask: a different turn, not the completed one (found in review 2026-11-19).
	fresh := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":"quick"}}}`
	turn3, _ := callAskAgent(t, e, tok, fresh)
	if turn3 == turn {
		t.Fatalf("a fresh ask with the same args reused the completed turn: %s", turn3)
	}
	// Each app registered its on, so BOTH asked replies come back to the asking
	// session as later turns.
	release(dir, "root")
	got := deliverUntil(t, e, root.Session, 2)
	delivered := map[string]bool{}
	for _, tn := range got {
		if tn.Origin != nil {
			delivered[tn.Origin.Turn] = true
		}
	}
	if !delivered[turn] || !delivered[turn3] {
		t.Fatalf("the asked replies were not both delivered to root (want %s and %s): %+v", turn, turn3, got)
	}
}

// The review_turn app end to end: it starts a review turn (ask_agent with a
// review prompt naming the reviewed turn) and delivers the reviewer's reply to
// the asking session. The verdict is that reply; core parses nothing.
func TestReviewTurnAppDelivers(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"review_turn\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\n"), 0o644)
	// The turn to review: a normal turn that ends with a change.
	w := e.firstTurn(t, folder, "claude", "work", true)
	tokenOf(t, dir, "work")
	os.WriteFile(filepath.Join(pwdOf(t, dir, "work"), "a.txt"), []byte("two\n"), 0o644)
	release(dir, "work")
	waitEnded(t, e, w.Turn)

	// A live follow-up turn in that session drives the review (the real shape:
	// you review a turn that has ended, from a turn that is still running).
	startTurn(t, e, `{"agent":"claude","session":"`+w.Session+`","prompt":"drive"}`)
	tok := tokenOf(t, dir, "drive")
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"review_turn","arguments":{"turn":"` + w.Turn + `","agent":"codex"}}}`
	code, out := as(t, e, tok, "POST", "/api/v0/mcp", call)
	if code != 200 {
		t.Fatalf("review_turn call: %d %s", code, out)
	}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(out), &env) != nil || env.Result.IsError || len(env.Result.Content) == 0 {
		t.Fatalf("review_turn failed: %s", out)
	}
	var r struct{ Turn, Session string }
	if json.Unmarshal([]byte(env.Result.Content[0].Text), &r) != nil || r.Turn == "" {
		t.Fatalf("review_turn output %q", env.Result.Content[0].Text)
	}
	// The reviewer's reply is delivered to the driving turn's session (w's),
	// behind the drive turn, which must end first.
	release(dir, "drive")
	got := deliverUntil(t, e, w.Session, 1)
	if got[0].Origin == nil || got[0].Origin.Turn != r.Turn {
		t.Fatalf("the review reply was not delivered: %+v", got)
	}
}

// ask_agent's startTurn step does not wait for a runner (wait=false, his (d)
// rule 2026-11-19): it returns once the turn is recorded, so registerOn runs in
// the same tool call even while the turn is only queued. Here the root turn
// holds the one runner slot, so the asked turn cannot start during the call —
// yet the app returns its id and its on is registered. When root ends and the
// asked turn finally runs, its reply still delivers.
func TestAskAgentAppQueuedStillGetsOn(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"ask_agent\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvCap(t, perm, 1) // one runner slot: root occupies it
	repo := gitRepo(t)
	root := e.firstTurn(t, repo, "claude", "root", true)
	tok := tokenOf(t, dir, "root") // root is running now, holding the slot

	turn, _ := callAskAgent(t, e, tok,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":"quick"}}}`)
	if turn == "" {
		t.Fatal("ask_agent returned no turn")
	}
	// The asked turn is only queued: no runner is free to start it.
	j, err := e.q.JobByTurn(queue.DefaultTeam, turn)
	if err != nil || j.State != queue.Queued {
		t.Fatalf("asked turn state %q (err %v), want queued", j.State, err)
	}
	// Yet its on is registered, watching the queued turn, to deliver to root.
	ons, err := e.q.ListOns(queue.DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range ons {
		if o.Subject == turn && o.Session == root.Session {
			found = true
		}
	}
	if !found {
		t.Fatalf("no on watching the queued turn %s for %s: %+v", turn, root.Session, ons)
	}
	// Release root; the slot frees, the asked turn runs and ends, and its reply
	// delivers back to root through the on registered while it was queued.
	release(dir, "root")
	got := deliverUntil(t, e, root.Session, 1)
	if got[0].Origin == nil || got[0].Origin.Turn != turn {
		t.Fatalf("the queued turn's reply did not deliver: %+v", got)
	}
}

// Client disconnect: the caller's request context is already cancelled when the
// handler runs, yet the atomic create records the turn AND its on server-side in
// one write, so the reply still delivers. Before atomic delivery the on was a
// separate step a disconnect could drop (his call (a), 2026-11-19).
func TestAskAgentDisconnectStillDelivers(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"ask_agent\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	repo := gitRepo(t)
	root := e.firstTurn(t, repo, "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is gone before the handler runs
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":"quick"}}}`
	req := httptest.NewRequest("POST", "http://127.0.0.1:"+e.port+"/api/v0/mcp", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	e.api.ServeHTTP(httptest.NewRecorder(), req)
	// The on was recorded atomically, despite the dead client.
	ons, err := e.q.ListOns(queue.DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	if len(ons) != 1 || ons[0].Session != root.Session {
		t.Fatalf("want one on delivering to root, got %+v", ons)
	}
	release(dir, "root")
	got := deliverUntil(t, e, root.Session, 1)
	if got[0].Origin == nil || got[0].Origin.Turn != ons[0].Subject {
		t.Fatalf("the reply did not deliver after the client disconnected: %+v", got)
	}
}

// Caller stopped: the asked turn's on is recorded atomically with the turn, so
// stopping the caller's own turn afterward cannot strand the reply — it still
// delivers to the caller's session. This was the residual window (a stop between
// startTurn and registerOn) before delivery became one write.
func TestAskAgentCallerStoppedStillDelivers(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"ask_agent\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	repo := gitRepo(t)
	root := e.firstTurn(t, repo, "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	turn, _ := callAskAgent(t, e, tok,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":"quick"}}}`)
	// Stop the caller's own turn; its reply's on was already written with the turn.
	if code, out := as(t, e, token, "POST", "/api/v0/turns/"+root.Turn+"/stop", ""); code != 202 {
		t.Fatalf("stop the caller's turn: %d %s", code, out)
	}
	got := deliverUntil(t, e, root.Session, 1)
	if got[0].Origin == nil || got[0].Origin.Turn != turn {
		t.Fatalf("the reply did not deliver after the caller was stopped: %+v", got)
	}
}

// The public on routes honour the caller's token: a turn token registers an
// on only for a turn its tree started, always delivering to its own session,
// and removes only an on its own turn made; the main token names any turn and
// the session. (B15: ons dedupe by idempotency_key only, so a keyless register
// is a new on each time — several may watch one turn.)
func TestOnAPIAuth(t *testing.T) {
	e, dir := askEnv(t)
	p := e.firstTurn(t, gitRepo(t), "claude", "parent", true)
	ptok := tokenOf(t, dir, "parent")
	other := e.firstTurn(t, gitRepo(t), "claude", "other", true)
	child := ask(t, e, ptok, `{"agent":"claude","prompt":"child"}`)

	// In-tree: the session is forced to the token's own, so one it sends is ignored.
	code, out := as(t, e, ptok, "POST", "/api/v0/ons", `{"turn":"`+child.Turn+`","session":"s_not_mine"}`)
	if code != 201 {
		t.Fatalf("register in-tree: %d %s", code, out)
	}
	var ov OnView
	if err := json.Unmarshal([]byte(out), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Turn != child.Turn || ov.Session != p.Session {
		t.Fatalf("on %+v, want turn %s session %s", ov, child.Turn, p.Session)
	}
	// Out of tree: refused.
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", `{"turn":"`+other.Turn+`"}`); code != 403 {
		t.Fatalf("out-of-tree register: %d %s, want 403", code, out)
	}
	// A key with '/' is refused, the same validator POST /turns uses: this is
	// what reserves the internal delivery/ result/ apply/ namespaces, so a
	// caller cannot register "delivery/<key>" and collide with a turn's own
	// delivery on (his call, 2026-11-19; found by grok). Exercises the collision
	// path and asserts it is closed at the route.
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", `{"turn":"`+child.Turn+`","idempotency_key":"delivery/x"}`); code != 400 {
		t.Fatalf("on with a '/' key: %d %s, want 400", code, out)
	}
	// A turn may not subscribe to its own turn (that would be a self-
	// continuation dodging the asked-work caps).
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", `{"turn":"`+p.Turn+`"}`); code != 403 {
		t.Fatalf("self register: %d %s, want 403", code, out)
	}
	// The main token must name a session (an asked turn without one is 400),
	// and a nonexistent turn is 404.
	if resp, out := do(t, e.req(t, "POST", "/api/v0/ons", strings.NewReader(`{"turn":"`+child.Turn+`"}`))); resp.StatusCode != 400 {
		t.Fatalf("main without session: %d %s, want 400", resp.StatusCode, out)
	}
	if resp, out := do(t, e.req(t, "POST", "/api/v0/ons", strings.NewReader(`{"turn":"t_nope","session":"`+p.Session+`"}`))); resp.StatusCode != 404 {
		t.Fatalf("main unknown turn: %d %s, want 404", resp.StatusCode, out)
	}
	// B15 (his call, 2026-11-19): the main token may watch any turn and deliver
	// to any session it names — core no longer confines an on to the watched
	// turn's tree (made safe by the self-retire checks, B13b). So delivering
	// child's reply to an unrelated session is allowed now.
	if resp, out := do(t, e.req(t, "POST", "/api/v0/ons", strings.NewReader(`{"turn":"`+other.Turn+`","session":"`+p.Session+`"}`))); resp.StatusCode != 201 {
		t.Fatalf("main any-turn: %d %s, want 201", resp.StatusCode, out)
	}
	if resp, out := do(t, e.req(t, "POST", "/api/v0/ons", strings.NewReader(`{"turn":"`+child.Turn+`","session":"`+p.Session+`"}`))); resp.StatusCode != 201 {
		t.Fatalf("main deliver: %d %s, want 201", resp.StatusCode, out)
	}
	// A different turn cannot remove the parent's on; the maker can.
	ctok := tokenOf(t, dir, "child")
	if code, out := as(t, e, ctok, "DELETE", "/api/v0/ons/"+ov.ID, ""); code != 403 {
		t.Fatalf("remove another's on: %d %s, want 403", code, out)
	}
	if code, out := as(t, e, ptok, "DELETE", "/api/v0/ons/"+ov.ID, ""); code != 204 {
		t.Fatalf("maker remove: %d %s, want 204", code, out)
	}
}

// A turn asks two agents with its token, in the person's folder: each runs in
// a worktree of its own, says who asked it, and its result comes back to the
// asking session as a later turn of its own, naming its worktree. A review it
// asks for comes back too. The asked turns read the tree, never write to
// their asker's session, and cancel only asked work.
func TestAskFanOutAndDeliver(t *testing.T) {
	e, dir := askEnv(t)
	repo := gitRepo(t)
	p := e.firstTurn(t, repo, "claude", "parent", true)
	ptok := tokenOf(t, dir, "parent")

	c1 := ask(t, e, ptok, `{"agent":"claude","prompt":"one"}`)
	c2 := ask(t, e, ptok, `{"agent":"codex","prompt":"two"}`)
	ctok := tokenOf(t, dir, "one")
	tokenOf(t, dir, "two")
	// The asked turns share the asker's copy (his decision, 2026-11-21): one
	// workspace, one copy, not the repo itself.
	w0, w1, w2 := pwdOf(t, dir, "parent"), pwdOf(t, dir, "one"), pwdOf(t, dir, "two")
	if w1 != w0 || w2 != w0 || w0 == repo {
		t.Fatalf("asked turns not in the asker's copy: parent %s, one %s, two %s, repo %s", w0, w1, w2, repo)
	}
	// The asked turns say who asked.
	for _, c := range []Started{c1, c2} {
		list, err := e.store.Turns(store.TurnQuery{Session: c.Session, Limit: 5})
		if err != nil || len(list.Turns) != 1 {
			t.Fatalf("child %s: %v %+v", c.Session, err, list)
		}
		if o := list.Turns[0].Origin; o == nil || o.Kind != event.OriginAsked || o.Session != p.Session || o.Turn != p.Turn {
			t.Errorf("child %s origin %+v", c.Session, o)
		}
	}
	// Reads across the tree work both ways; nothing outside it.
	for _, tc := range []struct {
		tok, method, path, body string
		want                    int
	}{
		{ptok, "GET", "/api/v0/sessions/" + c1.Session + "/events", "", 200},
		{ptok, "GET", "/api/v0/turns?session=" + c2.Session, "", 200},
		{ptok, "GET", "/api/v0/turns/" + c1.Turn, "", 200},
		{ptok, "GET", "/api/v0/sessions/" + c1.Session, "", 200},
		{ctok, "GET", "/api/v0/sessions/" + p.Session + "/events", "", 200},
		{ctok, "GET", "/api/v0/sessions/" + c2.Session + "/events", "", 200},
		// The asked turn writes to no session but one its own tree opened: not
		// its asker's, and not its own (it did not open its own session).
		{ctok, "POST", "/api/v0/turns", `{"agent":"claude","session":"` + p.Session + `","prompt":"x"}`, 403},
		{ctok, "POST", "/api/v0/turns", `{"agent":"claude","session":"` + c1.Session + `","prompt":"x"}`, 403},
		// It may not stop the person's work.
		{ctok, "POST", "/api/v0/turns/" + p.Turn + "/stop", "", 403},
		// A turn carries no folder or mode of its own: naming one is a 400.
		{ptok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","isolate":false}`, 400},
		{ptok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","folder":"` + filepath.Dir(repo) + `"}`, 400},
	} {
		if code, out := as(t, e, tc.tok, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s %s %s: %d %s, want %d", tc.tok[:8], tc.method, tc.path, code, out, tc.want)
		}
	}

	release(dir, "parent")
	release(dir, "one")
	release(dir, "two")
	got := deliverUntil(t, e, p.Session, 2)
	byTurn := map[string]store.Turn{}
	for _, tn := range got {
		byTurn[tn.Origin.Turn] = tn
	}
	for c, w := range map[Started]string{c1: w1, c2: w2} {
		tn, ok := byTurn[c.Turn]
		if !ok {
			t.Fatalf("no result of %s delivered; got %+v", c.Turn, got)
		}
		// The list carries no prompt; the delivered prompt is read from the
		// single turn (the record).
		prompt, _, _, err := e.store.TurnText(tn.Session, tn.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The asked turn ran in the asker's copy (w == w0, checked above), and
		// the delivered result names the asked turn, not any copy path.
		if tn.Origin.Session != c.Session || tn.Origin.TurnKind != "turn" ||
			!strings.HasPrefix(prompt, "[AgentBus]") || !strings.Contains(prompt, "Turn: "+c.Turn+", finished") ||
			strings.Contains(prompt, "sessions/") || w != w0 {
			t.Errorf("result of %s in %s: %+v\n%s", c.Turn, w, tn.Origin, prompt)
		}
		// On the parent's own agent.
		if tn.Harness != "claude" {
			t.Errorf("result of %s ran on %s", c.Turn, tn.Harness)
		}
	}

	// B15: the review route is gone; a review is the review_turn app (its own
	// end-to-end test). A follow-up turn still continues the asker's session, and
	// a stop of asked work in the tree is allowed.
	j1, err := e.q.JobByTurn(queue.DefaultTeam, c1.Turn)
	if err != nil || j1.Ended == nil {
		t.Fatalf("child job: %v %+v", err, j1)
	}
	startTurn(t, e, `{"agent":"claude","session":"`+p.Session+`","prompt":"again"}`)
	atok := tokenOf(t, dir, "again")
	c3 := ask(t, e, atok, `{"agent":"claude","prompt":"three"}`)
	if code, out := as(t, e, atok, "POST", "/api/v0/turns/"+c3.Turn+"/stop", ""); code != 202 {
		t.Errorf("stop asked work: %d %s", code, out)
	}
	release(dir, "again")
	_ = got
}

// A chain of asks stops at the depth cap, and one turn's asks stop at the
// per-turn cap; each refusal says why. A turn's own token may rename its own
// session.
func TestAskCaps(t *testing.T) {
	e, dir := askEnv(t)
	folder := t.TempDir()
	e.firstTurn(t, folder, "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	ask(t, e, tok, `{"agent":"claude","prompt":"d1"}`)
	ask(t, e, tokenOf(t, dir, "d1"), `{"agent":"claude","prompt":"d2"}`)
	code, out := askRaw(t, e, tokenOf(t, dir, "d2"), `{"agent":"claude","prompt":"d3"}`)
	if code != 403 || !strings.Contains(out, "levels") || !strings.Contains(out, "(Depth in Settings, Limits)") {
		t.Errorf("depth 3: %d %s", code, out)
	}
	for i := 1; i < settings.DefaultMaxJobsPerTurn; i++ {
		ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)
	}
	code, out = askRaw(t, e, tok, `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, fmt.Sprint(settings.DefaultMaxJobsPerTurn)) {
		t.Errorf("past the per-turn cap: %d %s", code, out)
	}

	// A turn's own token may rename its own session.
	ro := e.firstTurn(t, folder, "claude", "ro", true)
	code, out = as(t, e, tokenOf(t, dir, "ro"), "PATCH", "/api/v0/sessions/"+ro.Session, `{"title":"x"}`)
	if code != 200 {
		t.Errorf("a turn could not rename its own session: %d %s", code, out)
	}
	for _, p := range []string{"root", "d1", "d2", "ro"} {
		release(dir, p)
	}
}

// permissions.toml decides what a turn token may do, read for each request:
// a cap changed in the file applies to the next request, a cost cap stops
// asking once the tree has spent it, and a broken file refuses every turn
// token's request. Within its request tree a turn token always reads, lists
// agents, starts, reviews, stops and renames: the switches 0.5.2 retired
// still read, and change nothing. Outside its tree it reaches nothing. No
// setting opens the routes that change agents or tokens.
func TestAskPolicyFromPermissions(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(perm, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[limits]\nmax_jobs_per_turn = 2\n")
	e, dir := askEnvWith(t, perm)
	folder := t.TempDir()
	root := e.firstTurn(t, folder, "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)
	ask(t, e, tok, `{"agent":"claude","prompt":"child"}`)
	code, out := askRaw(t, e, tok, `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, "at most 2 turns or reviews") || !strings.Contains(out, "(Turns per request in Settings, Limits)") {
		t.Fatalf("past max_jobs_per_turn = 2: %d %s", code, out)
	}
	// The file is read again for the next request.
	write("[limits]\nmax_jobs_per_turn = 3\n")
	ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)

	child := tokenOf(t, dir, "child")
	// Another tree: a session the person started on their own.
	other := e.firstTurn(t, t.TempDir(), "claude", "other", true)
	// Every retired switch off, scope "session": the file reads, and the
	// child's token still reads, lists, starts, reviews, stops and renames
	// within its tree, the person's session included.
	write("[agent_api]\nscope = \"session\"\nread = false\nlist_agents = false\nstart = false\nreview = false\ncancel = false\nrename = false\n")
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"read the person's session", "GET", "/api/v0/sessions/" + root.Session + "/search?q=x", ""},
		{"read by query", "GET", "/api/v0/turns?session=" + root.Session, ""},
		{"read events by query", "GET", "/api/v0/sessions/" + root.Session + "/events?turn=" + root.Turn, ""},
		{"list agents", "GET", "/api/v0/harnesses", ""},
		{"rename the person's session", "PATCH", "/api/v0/sessions/" + root.Session, `{"title":"Named in the tree"}`},
	} {
		if code, out := as(t, e, child, tc.method, tc.path, tc.body); code != 200 {
			t.Errorf("%s with the retired switches off: %d %s, want 200", tc.name, code, out)
		}
	}
	started := ask(t, e, child, `{"agent":"claude","prompt":"quick"}`)
	if code, out := as(t, e, child, "POST", "/api/v0/turns/"+started.Turn+"/stop", ""); code == 403 {
		t.Errorf("stop a turn its tree asked for: %d %s", code, out)
	}
	// Outside its tree, nothing: not to read, rename, review or stop.
	for _, tc := range []struct {
		name, method, path, body, want string
	}{
		{"read another tree", "GET", "/api/v0/sessions/" + other.Session + "/search?q=x", "", "only its own session"},
		{"read another tree by query", "GET", "/api/v0/turns?session=" + other.Session, "", "only its own session"},
		{"rename another tree's session", "PATCH", "/api/v0/sessions/" + other.Session, `{"title":"x"}`, "only its own session"},
		{"stop another tree's turn", "POST", "/api/v0/turns/" + other.Turn + "/stop", "", "request tree asked for"},
		{"stop its tree's own root", "POST", "/api/v0/turns/" + root.Turn + "/stop", "", "request tree asked for"},
	} {
		for _, k := range []string{tok, child} {
			if code, out := as(t, e, k, tc.method, tc.path, tc.body); code != 403 || !strings.Contains(out, tc.want) {
				t.Errorf("%s: %d %s, want 403 with %q", tc.name, code, out, tc.want)
			}
		}
	}
	// A broken file refuses every turn token's request.
	write("[agent_api]\napply = all\n")
	if code, out := as(t, e, tok, "GET", "/api/v0/harnesses", ""); code != 503 || !strings.Contains(out, "cannot be used") {
		t.Errorf("broken file: %d %s", code, out)
	}
	// Every switch on, the routes that change agents stay closed to a turn
	// token: they take the main token, always. rename is on by default, in
	// the tree.
	write("")
	if code, out := as(t, e, tok, "PATCH", "/api/v0/sessions/"+root.Session, `{"title":"Named by a turn"}`); code != 200 || !strings.Contains(out, "Named by a turn") {
		t.Errorf("rename in the tree: %d %s", code, out)
	}
	for _, r := range [][3]string{
		{"POST", "/api/v0/agents", `{"name":"x","harness":"claude","access":"subscription"}`},
		{"PUT", "/api/v0/agents/a_x", `{"name":"x","harness":"claude","access":"subscription"}`},
		{"DELETE", "/api/v0/agents/a_x", ""},
		// The settings: a turn token neither reads nor writes them.
		{"GET", "/api/v0/settings/permissions", ""},
		{"PUT", "/api/v0/settings/permissions", `{"forbid":[]}`},
		{"GET", "/api/v0/settings/instructions", ""},
		{"PUT", "/api/v0/settings/instructions", `{"text":"x"}`},
	} {
		if code, out := as(t, e, tok, r[0], r[1], r[2]); code != 403 || !strings.Contains(out, "main token") {
			t.Errorf("%s %s: %d %s, want 403", r[0], r[1], code, out)
		}
	}
	// A cost cap: once the tree has spent it, asking stops; 0 is off. A
	// retry of a request already made still gets its job.
	const keyed = `{"agent":"claude","prompt":"quick","idempotency_key":"k-cost"}`
	first := ask(t, e, child, keyed)
	cost := 0.30
	if _, _, err := e.store.Append(event.Event{ID: "evt_cost", Kind: event.ModelResponded, Time: time.Now().UTC(),
		Session: root.Session, Turn: root.Turn, Exchange: "x-cost", Status: 200, CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	write("[limits]\nmax_cost_usd_per_tree = 0.25\n")
	code, out = as(t, e, child, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, "(Cost per request tree in Settings, Limits)") || !strings.Contains(out, "$0.30") {
		t.Errorf("past the cost cap: %d %s", code, out)
	}
	if again := ask(t, e, child, keyed); !again.Existing || again.Turn != first.Turn {
		t.Errorf("a retry past the cost cap: %+v, want turn %s again", again, first.Turn)
	}
	write("[limits]\nmax_cost_usd_per_tree = 0\n")
	ask(t, e, child, `{"agent":"claude","prompt":"quick"}`)
	for _, p := range []string{"root", "child"} {
		release(dir, p)
	}
}

// An ask runs in the asker's workspace, so the asked agent shares the asker's
// copy (his decision, 2026-11-21: asks edit the same copy). A turn carries no
// folder of its own.
func TestAskInOwnCopy(t *testing.T) {
	e, dir := askEnv(t)
	folder := t.TempDir()
	os.MkdirAll(filepath.Join(folder, "sub"), 0o755)
	os.WriteFile(filepath.Join(folder, "sub", "x.txt"), []byte("x"), 0o644)
	e.firstTurn(t, folder, "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	copyDir := pwdOf(t, dir, "root")
	if copyDir == folder {
		t.Fatalf("the root turn ran in the folder itself")
	}
	// The asked turn runs in the asker's copy, not a copy of its own and not the
	// person's folder.
	ask(t, e, tok, `{"agent":"claude","prompt":"in-copy"}`)
	if got := pwdOf(t, dir, "in-copy"); got != copyDir {
		t.Errorf("the asked turn ran in %q, want the asker's copy %q", got, copyDir)
	}
	// A turn carries no folder: a turn token naming one is refused.
	if code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","folder":"`+t.TempDir()+`"}`); code != 400 {
		t.Errorf("a turn naming a folder: %d %s, want 400", code, out)
	}
	for _, p := range []string{"root", "in-copy"} {
		release(dir, p)
	}
}

// A session in a git repo with the git backend runs in a worktree, not the
// repo. An asked agent shares the asker's worktree (the one copy), so it works
// on the worktree as it is, not on the repo.
func TestAskFromAWorktree(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("isolation = \""+settings.IsolationGit+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	repo := gitRepo(t)
	e.firstTurn(t, repo, "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	worktree := pwdOf(t, dir, "root")
	if worktree == repo {
		t.Fatalf("the root turn ran in the repo itself")
	}
	ask(t, e, tok, `{"agent":"claude","prompt":"child"}`)
	if got := pwdOf(t, dir, "child"); got != worktree {
		t.Errorf("the asked turn ran in %q, want the asker's worktree %q", got, worktree)
	}
	release(dir, "root")
	release(dir, "child")
}

// An asked agent is told where it was asked from: the asking session and
// turn in its environment, and a line in its instructions. With its own
// token it reads what the asking turn changed, once that turn has ended,
// although its own copy does not hold the change.
func TestAskedAgentReadsTheAskersChanges(t *testing.T) {
	e, dir := askEnv(t)
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\n"), 0o644)
	root := e.firstTurn(t, folder, "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	os.WriteFile(filepath.Join(pwdOf(t, dir, "root"), "a.txt"), []byte("two\n"), 0o644)
	ask(t, e, tok, `{"agent":"claude","prompt":"child"}`)
	childTok := tokenOf(t, dir, "child")
	b, _ := os.ReadFile(filepath.Join(dir, "env-child"))
	for _, want := range []string{"AGENTBUS_ASKED_BY_SESSION=" + root.Session + "\n", "AGENTBUS_ASKED_BY_TURN=" + root.Turn + "\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the child's environment lacks %q", want)
		}
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args-child"))
	// The line points the agent at the changes route through agentbus-api (curl
	// form here, no API client on PATH in the test), not at a turn_changes tool.
	if want := "/api/v0/turns/" + root.Turn + "/changes?session=" + root.Session; !strings.Contains(string(args), want) {
		t.Errorf("the child's instructions do not name %s: %s", want, args)
	}
	// The asked agent shares the asker's copy (his decision, 2026-11-21), so it
	// reads the asker's uncommitted edit directly, in the same folder.
	if cp, rp := pwdOf(t, dir, "child"), pwdOf(t, dir, "root"); cp != rp {
		t.Errorf("the child ran in %q, want the asker's copy %q", cp, rp)
	}
	if got, _ := os.ReadFile(filepath.Join(pwdOf(t, dir, "child"), "a.txt")); string(got) != "two\n" {
		t.Errorf("the child's copy holds %q, want the asker's edit", got)
	}
	// The record says the same, from the asking turn's token: the child's
	// own turn names who asked it.
	code, out := as(t, e, childTok, "GET", "/api/v0/turns", "")
	var own struct {
		Turns []struct {
			Origin *event.Origin `json:"origin"`
		} `json:"turns"`
	}
	if err := json.Unmarshal([]byte(out), &own); code != 200 || err != nil || len(own.Turns) != 1 || own.Turns[0].Origin == nil ||
		own.Turns[0].Origin.Kind != event.OriginAsked || own.Turns[0].Origin.Session != root.Session || own.Turns[0].Origin.Turn != root.Turn {
		t.Errorf("the child's own turn: %d %s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "env-root")); strings.Contains(string(b), "AGENTBUS_ASKED_BY") {
		t.Error("the person's own turn says it was asked")
	}
	release(dir, "root")
	diffPath := "/api/v0/turns/" + root.Turn + "/diff?session=" + root.Session
	changesPath := "/api/v0/turns/" + root.Turn + "/changes?session=" + root.Session
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		code, out := as(t, e, childTok, "GET", diffPath, "")
		if code != 200 {
			t.Fatalf("the child reads the root's diff: %d %s", code, out)
		}
		var d struct {
			Files *struct {
				Count int `json:"count"`
			} `json:"files"`
		}
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatal(err)
		}
		if d.Files != nil {
			ccode, cout := as(t, e, childTok, "GET", changesPath, "")
			if ccode != 200 {
				t.Fatalf("the child reads the root's changes: %d %s", ccode, cout)
			}
			var p struct {
				Changes []struct {
					Path string  `json:"path"`
					Diff *string `json:"diff"`
				} `json:"changes"`
			}
			if err := json.Unmarshal([]byte(cout), &p); err != nil {
				t.Fatal(err)
			}
			if len(p.Changes) != 1 || p.Changes[0].Path != "a.txt" || p.Changes[0].Diff == nil || !strings.Contains(*p.Changes[0].Diff, "+two") {
				t.Fatalf("the root's changes: %s", cout)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the root's diff has no files: %s", out)
		}
	}
	release(dir, "child")
}
