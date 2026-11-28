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
	"slices"
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
	t.Helper()
	dir := t.TempDir()
	script := `for a; do p="$a"; done; echo '{"type":"result","session_id":"sid-'$$'"}'; ` +
		`case "$p" in "[AgentBus]"*) printf '%s' "$p" > "$D/delivered-$$"; exit 0;; quick|"Review a change"*|"Review the changes"*) exit 0;; esac; ` +
		`printf '%s\n' "$@" > "$D/args-$p"; env > "$D/env-$p.tmp"; pwd > "$D/pwd-$p.tmp"; mv "$D/pwd-$p.tmp" "$D/pwd-$p"; mv "$D/env-$p.tmp" "$D/env-$p"; ` +
		`i=0; while [ ! -e "$D/release-$p" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	return stackEnv(t, stack{wire: wire, permissions: perm}), dir
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

// pwdOf waits for the working folder the turn of prompt wrote. A fake agent
// writes it to a .tmp file and renames it, so the file appears whole: written
// in place, it exists empty until the shell writes the folder, and a read in
// between returns "" (seen on Ubuntu CI, 2026-11-26).
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

// deliverWait is how long deliverUntil waits for a reply. A delivery follows a
// chain of turns (the asker ends, the asked turn runs and ends, the on fires,
// the reply turn starts), and under -race every queue call waits in line for
// the queue's one database connection behind each runner slot's take. A poll
// that takes 0.1ms idle took up to 1.9s there, and a loaded macOS CI runner
// went past 15s (PR #109, 2026-11-27). A reply that never comes still fails.
const deliverWait = 2 * time.Minute

// recordReader reads the record's events of a kind for an on over a session,
// as agentbus up's control loop does (cmd/agentbus, recordEvents).
func recordReader(st *store.Store) queue.RecordReader {
	return func(kind string, after int64, limit int) ([]queue.RecordEvent, error) {
		evs, err := st.EventsOfKind(kind, after, limit)
		if err != nil {
			return nil, err
		}
		out := make([]queue.RecordEvent, len(evs))
		for i, e := range evs {
			out[i] = queue.RecordEvent{ID: e.ID, Seq: e.Seq, Time: e.Time, Workspace: e.Workspace, Turn: e.Turn}
		}
		return out, nil
	}
}

// deliverUntil waits until session holds want delivered results.
func deliverUntil(t *testing.T, e *env, session string, want int) []store.Turn {
	t.Helper()
	for deadline := time.Now().Add(deliverWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		// Mirror the sweep: record each ended turn's end, then fire the ons
		// that read it from the record (C0).
		if _, _, err := jobs.RecordTurnEnds(e.q, e.store, nil, time.Now().Add(time.Hour), 200); err != nil {
			t.Fatal(err)
		}
		if _, err := e.q.FireOns(queue.DefaultTeam, e.store.TurnEnded, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := e.q.FireWatchOns(queue.DefaultTeam, recordReader(e.store), nil, nil, nil); err != nil {
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

// ask drives the three steps ask_agent runs (his API rules, 2026-11-21 and
// 2026-11-27): a turn token opens a session in its own workspace, registers an
// on over it that delivers its turns' replies to the token's own session, then
// starts the asked turn in it, so the asked agent shares the asker's copy.
// body is the turn body with its agent and prompt; ask injects the session it
// opened, and returns the asked turn. TestAskAgentAppDelivers drives the real
// app through MCP end to end.
func ask(t *testing.T, e *env, tok, body string) Started {
	t.Helper()
	// A turn's idempotency key covers every step, so a retry reuses the same
	// session, the same on and the same turn, as the ask_agent app does.
	var fields struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	_ = json.Unmarshal([]byte(body), &fields)
	sessBody, onKey := `{}`, ""
	if fields.IdempotencyKey != "" {
		sessBody = `{"idempotency_key":"` + fields.IdempotencyKey + `:s"}`
		onKey = `,"idempotency_key":"` + fields.IdempotencyKey + `:o"`
	}
	code, out := as(t, e, tok, "POST", "/api/v0/sessions", sessBody)
	if code != 201 {
		t.Fatalf("ask: open a session in the asker's workspace: %d %s", code, out)
	}
	var sc SessionCreated
	if err := json.Unmarshal([]byte(out), &sc); err != nil {
		t.Fatal(err)
	}
	if code, out := as(t, e, tok, "POST", "/api/v0/ons", `{"event":"turn.completed","filter":{"session":"`+sc.Session+`"}`+onKey+`}`); code != 201 {
		t.Fatalf("ask: register the on over %s: %d %s", sc.Session, code, out)
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

// The real ask_agent app, end to end through MCP: one tool call opens a
// session, registers the on over it and starts the turn (its three Arazzo
// steps, apps.go), the asked turn says who asked it, a retry returns the same
// turn (the derived idempotency key), and the reply is delivered back to the
// asking session.
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
	// A retry (the SAME JSON-RPC id) returns the same turn, not a second one.
	// This is API idempotency: the asked turn is only queued, not yet run.
	if turn2, _ := callAskAgent(t, e, tok, call); turn2 != turn {
		t.Fatalf("a retry started a new turn: %s vs %s", turn2, turn)
	}
	// A FRESH call (a different JSON-RPC id) with the same arguments is a new
	// ask: a different turn, not the completed one (found in review 2026-11-19).
	fresh := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":"quick"}}}`
	turn3, session3 := callAskAgent(t, e, tok, fresh)
	if turn3 == turn {
		t.Fatalf("a fresh ask with the same args reused the completed turn: %s", turn3)
	}
	// GET /api/v0/ons lists the ons the asks registered over the sessions they
	// opened, for the main token and for the asking turn's own, by the session
	// they deliver to: one per ask, a retry adding none.
	for _, tk := range []string{token, tok} {
		if got := onWatches(t, e, tk, root.Session); !slices.Equal(got, []string{session, session3}) {
			t.Errorf("GET /api/v0/ons?session=root before delivery: %v, want [%s %s]", got, session, session3)
		}
	}
	// The asked turns run in the asker's workspace lane, after the asker's turn
	// ends, so release root; then the runner starts the first asked turn (check
	// its origin) and BOTH replies come back to the asking session as later turns.
	release(dir, "root")
	asked := waitAskedTurn(t, e, session)
	if o := asked.Origin; o == nil || o.Kind != event.OriginAsked || o.Turn != root.Turn {
		t.Errorf("asked turn origin %+v, want asked by %s", o, root.Turn)
	}
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
	// An on over a session stays after it fires: a later turn of the asked
	// session comes back too.
	if got := onWatches(t, e, token, root.Session); !slices.Equal(got, []string{session, session3}) {
		t.Fatalf("ons after delivery: %v, want both still listed", got)
	}
}

// onWatches is what the ons delivering to session watch, as tok lists them:
// the watched session, or the turn of an on over one turn.
func onWatches(t *testing.T, e *env, tok, session string) []string {
	t.Helper()
	code, out := as(t, e, tok, "GET", "/api/v0/ons?session="+session, "")
	var l OnList
	if code != 200 || json.Unmarshal([]byte(out), &l) != nil {
		t.Fatalf("GET /api/v0/ons: %d %s", code, out)
	}
	watched := []string{}
	for _, on := range l.Ons {
		if on.Filter != nil {
			watched = append(watched, on.Filter.Session)
		} else {
			watched = append(watched, on.Turn)
		}
	}
	return watched
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

// review_turn with no turn reviews the asker's workspace's whole unapplied set:
// the app still starts a review turn and delivers its reply, with no turn id in
// the call. The reviewer's prompt points at the workspace changes route.
func TestReviewTurnAppNoTurnDelivers(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"review_turn\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\n"), 0o644)
	// A turn that ends with a change, then a live follow-up turn drives the review.
	w := e.firstTurn(t, folder, "claude", "work", true)
	tokenOf(t, dir, "work")
	os.WriteFile(filepath.Join(pwdOf(t, dir, "work"), "a.txt"), []byte("two\n"), 0o644)
	release(dir, "work")
	waitEnded(t, e, w.Turn)

	startTurn(t, e, `{"agent":"claude","session":"`+w.Session+`","prompt":"drive"}`)
	tok := tokenOf(t, dir, "drive")
	// No turn argument: the whole unapplied set is reviewed.
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"review_turn","arguments":{"agent":"codex"}}}`
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
		t.Fatalf("review_turn (no turn) failed: %s", out)
	}
	var r struct{ Turn, Session string }
	if json.Unmarshal([]byte(env.Result.Content[0].Text), &r) != nil || r.Turn == "" {
		t.Fatalf("review_turn (no turn) output %q", env.Result.Content[0].Text)
	}
	release(dir, "drive")
	got := deliverUntil(t, e, w.Session, 1)
	if got[0].Origin == nil || got[0].Origin.Turn != r.Turn {
		t.Fatalf("the review reply was not delivered: %+v", got)
	}
}

// ask_agent's startTurn step does not wait for a runner (POST /turns never
// waits, C1 2026-11-25): it returns once the turn is recorded, so registerOn runs in
// the same tool call even while the turn is only queued. Here the asked session
// is in root's workspace (ask_agent opens it there), so the asked turn waits in
// the workspace's lane while root runs, and cannot start during the call — yet
// the app returns its id and its on is registered. When root ends and the asked
// turn finally runs, its reply still delivers.
func TestAskAgentAppQueuedStillGetsOn(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"ask_agent\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	repo := gitRepo(t)
	root := e.firstTurn(t, repo, "claude", "root", true)
	tok := tokenOf(t, dir, "root") // root is running now, in the workspace's lane

	turn, asked := callAskAgent(t, e, tok,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":"quick"}}}`)
	if turn == "" {
		t.Fatal("ask_agent returned no turn")
	}
	// The asked turn is only queued: root holds the workspace's lane.
	j, err := e.q.JobByTurn(queue.DefaultTeam, turn)
	if err != nil || j.State != queue.Queued {
		t.Fatalf("asked turn state %q (err %v), want queued", j.State, err)
	}
	// Yet its on is registered, watching the asked session, to deliver to root.
	ons, err := e.q.ListOns(queue.DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range ons {
		if o.Filter.Session == asked && o.Session == root.Session {
			found = true
		}
	}
	if !found {
		t.Fatalf("no on watching the asked session %s for %s: %+v", asked, root.Session, ons)
	}
	// Release root; the lane frees, the asked turn runs and ends, and its reply
	// delivers back to root through the on registered while it was queued.
	release(dir, "root")
	got := deliverUntil(t, e, root.Session, 1)
	if got[0].Origin == nil || got[0].Origin.Turn != turn {
		t.Fatalf("the queued turn's reply did not deliver: %+v", got)
	}
}

// The 2026-11-26 case through the API and a runner over HTTP (Grok review
// evt_e3e61c1e333c80c6, P1): the person's coordinator asks for three builders
// while its turn runs; ask_agent opens their sessions in its workspace, so
// they queue in its lane. The person then sends the coordinator a message.
// When the coordinator's turn ends, the message runs before any builder: a
// queued delegated turn holds no copy. Before the fix it ran after all three.
func TestPersonsMessageGoesAheadOfAskedBuilders(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"ask_agent\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	root := e.firstTurn(t, gitRepo(t), "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	for i, b := range []string{"b1", "b2", "b3"} {
		turn, _ := callAskAgent(t, e, tok, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":%q}}}`, i+1, b))
		if j, err := e.q.JobByTurn(queue.DefaultTeam, turn); err != nil || j.State != queue.Queued {
			t.Fatalf("builder %s: %q %v, want queued behind the coordinator", b, j.State, err)
		}
	}
	msg := startTurn(t, e, fmt.Sprintf(`{"session":%q,"agent":"claude","prompt":"next"}`, root.Session))
	if msg.State != "queued" || msg.Ahead != 1 {
		t.Fatalf("the person's message: %+v, want queued behind the coordinator's running turn only", msg)
	}
	release(dir, "root")
	tokenOf(t, dir, "next")
	for _, b := range []string{"b1", "b2", "b3"} {
		if _, err := os.Stat(filepath.Join(dir, "args-"+b)); err == nil {
			t.Fatalf("builder %s ran before the person's message", b)
		}
	}
	for _, p := range []string{"next", "b1", "b2", "b3"} {
		release(dir, p)
	}
}

// Through the API (Codex review evt_ffeb7b5ccdb2ee05): a turn token that
// starts a turn in its own root session, the person's session, starts
// delegated work, counted against the limit. The person's own next message in
// that session is not.
func TestAgentTurnInTheRootSessionIsDelegated(t *testing.T) {
	e, dir := askEnvWith(t, "")
	root := e.firstTurn(t, gitRepo(t), "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	code, out := as(t, e, tok, "POST", "/api/v0/turns", fmt.Sprintf(`{"session":%q,"agent":"claude","prompt":"again"}`, root.Session))
	var st Started
	if code != 202 || json.Unmarshal([]byte(out), &st) != nil {
		t.Fatalf("the agent's turn: %d %s", code, out)
	}
	j, err := e.q.JobByTurn(queue.DefaultTeam, st.Turn)
	d, derr := e.q.JobDelegated(queue.DefaultTeam, j.ID)
	if err != nil || derr != nil || !d || j.Tree != j.Session {
		t.Fatalf("the agent's turn in its root session: %+v %v, want delegated", j, err)
	}
	mine := startTurn(t, e, fmt.Sprintf(`{"session":%q,"agent":"claude","prompt":"mine"}`, root.Session))
	m, err := e.q.JobByTurn(queue.DefaultTeam, mine.Turn)
	if d, derr := e.q.JobDelegated(queue.DefaultTeam, m.ID); err != nil || derr != nil || d {
		t.Fatalf("the person's message: %+v %v, want not delegated", m, err)
	}
	for _, p := range []string{"root", "again", "mine"} {
		release(dir, p)
	}
}

// Client disconnect: the caller's request context is already cancelled when the
// handler runs, yet the app's steps run server-side, the on before the turn, so
// the reply still delivers (his call (a), 2026-11-19).
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
	// The on was recorded, despite the dead client.
	ons, err := e.q.ListOns(queue.DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	if len(ons) != 1 || ons[0].Session != root.Session || ons[0].Filter.Session == "" {
		t.Fatalf("want one on delivering to root, got %+v", ons)
	}
	release(dir, "root")
	got := deliverUntil(t, e, root.Session, 1)
	if got[0].Origin == nil || got[0].Origin.Session != ons[0].Filter.Session {
		t.Fatalf("the reply did not deliver after the client disconnected: %+v", got)
	}
}

// Caller stopped: the asked session's on is recorded before its turn, so
// stopping the caller's own turn afterward cannot strand the reply — it still
// delivers to the caller's session.
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
	// Stop the caller's own turn; its reply's on was already written.
	if code, out := as(t, e, token, "POST", "/api/v0/turns/"+root.Turn+"/stop", ""); code != 202 {
		t.Fatalf("stop the caller's turn: %d %s", code, out)
	}
	got := deliverUntil(t, e, root.Session, 1)
	if got[0].Origin == nil || got[0].Origin.Turn != turn {
		t.Fatalf("the reply did not deliver after the caller was stopped: %+v", got)
	}
}

// The public on routes honour the caller's token (0.6.8): a turn token
// registers an on over a session in its reach, its own session's too as the
// watched one, delivering to a session in its reach (its own by default), and
// removes an on its own turn made or one that delivers within its reach
// (O2.3); the main token names any session to watch and the session to deliver
// to. An on may not deliver in a circle.
func TestOnAPIAuth(t *testing.T) {
	e, dir := askEnv(t)
	p := e.firstTurn(t, gitRepo(t), "claude", "parent", true)
	ptok := tokenOf(t, dir, "parent")
	other := e.firstTurn(t, gitRepo(t), "claude", "other", true)
	child := ask(t, e, ptok, `{"agent":"claude","prompt":"child"}`)
	on := func(watch, to string) string {
		b := `{"event":"turn.completed","filter":{"session":"` + watch + `"}`
		if to != "" {
			b += `,"session":"` + to + `"`
		}
		return b + `}`
	}

	// A target outside its reach is refused; without one, its own is used,
	// and the on ask registered over the child's session is the one returned.
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", on(child.Session, other.Session)); code != 403 || !strings.Contains(out, turnSessionRefused) {
		t.Fatalf("deliver to a session outside its reach: %d %s, want 403", code, out)
	}
	code, out := as(t, e, ptok, "POST", "/api/v0/ons", on(child.Session, ""))
	if code != 201 {
		t.Fatalf("watch the child's session: %d %s", code, out)
	}
	var ov OnView
	if err := json.Unmarshal([]byte(out), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Filter == nil || ov.Filter.Session != child.Session || ov.Session != p.Session || ov.Turn != "" {
		t.Fatalf("on %+v, want filter.session %s session %s", ov, child.Session, p.Session)
	}
	// A watched session outside its reach: refused.
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", on(other.Session, "")); code != 403 || !strings.Contains(out, turnSessionRefused) {
		t.Fatalf("watch a session outside its reach: %d %s, want 403", code, out)
	}
	// A session it opened, though its own turn started nothing there yet.
	code, out = as(t, e, ptok, "POST", "/api/v0/sessions", `{}`)
	var gs SessionCreated
	if code != 201 || json.Unmarshal([]byte(out), &gs) != nil {
		t.Fatalf("open a session: %d %s", code, out)
	}
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", on(gs.Session, "")); code != 201 || !strings.Contains(out, `"session":"`+p.Session+`"`) {
		t.Fatalf("watch a session it opened: %d %s, want 201 to its own session", code, out)
	}
	// A key with '/' is refused, the same validator POST /turns uses: this is
	// what reserves the internal delivery/ result/ apply/ namespaces (his call,
	// 2026-11-19; found by grok).
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", `{"event":"turn.completed","filter":{"session":"`+gs.Session+`"},"idempotency_key":"delivery/x"}`); code != 400 {
		t.Fatalf("on with a '/' key: %d %s, want 400", code, out)
	}
	// Its own session into itself, or the child's results back into the
	// child: a circle, 409.
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", on(p.Session, "")); code != 409 || !strings.Contains(out, "circle") {
		t.Fatalf("watch its own session into itself: %d %s, want 409", code, out)
	}
	if code, out := as(t, e, ptok, "POST", "/api/v0/ons", on(p.Session, child.Session)); code != 409 {
		t.Fatalf("deliver the parent's results to the child it watches: %d %s, want 409", code, out)
	}
	// The main token must name a session to deliver to (400), and a session
	// that does not exist is 404.
	if resp, out := do(t, e.req(t, "POST", "/api/v0/ons", strings.NewReader(on(child.Session, "")))); resp.StatusCode != 400 {
		t.Fatalf("main without session: %d %s, want 400", resp.StatusCode, out)
	}
	if resp, out := do(t, e.req(t, "POST", "/api/v0/ons", strings.NewReader(on("s_nope", p.Session)))); resp.StatusCode != 404 {
		t.Fatalf("main unknown session: %d %s, want 404", resp.StatusCode, out)
	}
	// The main token may watch any session and deliver to any it names.
	if resp, out := do(t, e.req(t, "POST", "/api/v0/ons", strings.NewReader(on(other.Session, p.Session)))); resp.StatusCode != 201 {
		t.Fatalf("main any session: %d %s, want 201", resp.StatusCode, out)
	}
	// A different turn cannot remove the parent's on; the maker can. "other" runs
	// in its own workspace beside the asker, so its token is a valid different
	// turn -- the asked "child" cannot run while the asker's turn is parked (the
	// workspace lane), so its token is not available here.
	otok := tokenOf(t, dir, "other")
	if code, out := as(t, e, otok, "DELETE", "/api/v0/ons/"+ov.ID, ""); code != 403 {
		t.Fatalf("remove another's on: %d %s, want 403", code, out)
	}
	if code, out := as(t, e, ptok, "DELETE", "/api/v0/ons/"+ov.ID, ""); code != 204 {
		t.Fatalf("maker remove: %d %s, want 204", code, out)
	}
	// Control's built-in on (the put-away) is no agent's to turn off.
	builtin, err := e.q.RegisterRecordOn(queue.DefaultTeam, "workspace.applied", queue.Action{Queue: queue.KindPutAway}, "builtin/put_away/workspace.applied", 0)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := as(t, e, ptok, "DELETE", "/api/v0/ons/"+builtin.ID, ""); code != 403 {
		t.Fatalf("a turn token removes the built-in on: %d %s, want 403", code, out)
	}
	release(dir, "parent")
	release(dir, "other")
}

// An on does not count as work asked for (0.6.8; the coordinator's gap of
// 2026-11-27, whose ons used its whole tree budget). With max_jobs_per_tree =
// 2, a turn that asked for one job registers ons freely and may still ask for
// one more job; the next is refused.
func TestOnsDoNotCountAsAskedWork(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("[limits]\nmax_jobs_per_tree = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	e.firstTurn(t, gitRepo(t), "claude", "parent", true)
	ptok := tokenOf(t, dir, "parent")
	ask(t, e, ptok, `{"agent":"claude","prompt":"child"}`)
	for range 3 {
		code, out := as(t, e, ptok, "POST", "/api/v0/sessions", `{}`)
		var sc SessionCreated
		if code != 201 || json.Unmarshal([]byte(out), &sc) != nil {
			t.Fatalf("open a session: %d %s", code, out)
		}
		if code, out := as(t, e, ptok, "POST", "/api/v0/ons", `{"event":"turn.completed","filter":{"session":"`+sc.Session+`"}}`); code != 201 {
			t.Fatalf("an on: %d %s", code, out)
		}
	}
	if code, out := askRaw(t, e, ptok, `{"agent":"claude","prompt":"quick"}`); code != 202 {
		t.Fatalf("the second ask after four ons: %d %s, want it to start", code, out)
	}
	if code, out := askRaw(t, e, ptok, `{"agent":"claude","prompt":"quick"}`); code != 403 || !strings.Contains(out, "(Turns per request tree in Settings, Limits)") {
		t.Fatalf("the third ask: %d %s, want the tree cap", code, out)
	}
	release(dir, "parent")
	release(dir, "child")
}

// The coordinator's case (0.6.8, landing gap of 2026-11-27): a coordinator
// asks a builder with ask_agent, and the builder asks for a review with
// review_turn. The one on ask_agent registered over the builder's session
// carries EVERY turn of it that ends to the coordinator: the builder's own
// turn, then the builder's later turn that carries the review's result. No
// on is registered by hand. Before, the coordinator heard only the builder's
// first turn, and the review reached the builder alone.
func TestReviewResultReachesWhoAskedForTheWork(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("allow = [\"ask_agent\", \"review_turn\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	coord := e.firstTurn(t, gitRepo(t), "claude", "coord", true)
	ctok := tokenOf(t, dir, "coord")
	_, builder := callAskAgent(t, e, ctok,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_agent","arguments":{"agent":"claude","prompt":"builder"}}}`)
	// The builder shares the coordinator's workspace lane: it runs once the
	// coordinator's turn ends.
	release(dir, "coord")
	btok := tokenOf(t, dir, "builder")
	code, out := as(t, e, btok, "POST", "/api/v0/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"review_turn","arguments":{"agent":"codex"}}}`)
	if code != 200 || strings.Contains(out, `"isError":true`) {
		t.Fatalf("review_turn from the builder: %d %s", code, out)
	}
	release(dir, "builder")
	// The builder hears the review; then the coordinator hears both turns.
	intoBuilder := deliverUntil(t, e, builder, 1)
	got := deliverUntil(t, e, coord.Session, 2)
	from := map[string]bool{}
	for _, tn := range got {
		if tn.Origin == nil || tn.Origin.Session != builder {
			t.Fatalf("a result in the coordinator's session not from the builder: %+v", tn.Origin)
		}
		from[tn.Origin.Turn] = true
	}
	if !from[intoBuilder[0].ID] {
		t.Fatalf("the builder's turn that carries the review (%s) did not reach the coordinator: %v", intoBuilder[0].ID, from)
	}
	// One on delivers to the coordinator: the one ask_agent registered.
	if ons := onWatches(t, e, token, coord.Session); !slices.Equal(ons, []string{builder}) {
		t.Fatalf("ons delivering to the coordinator: %v, want the one over the builder's session", ons)
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
	w0 := pwdOf(t, dir, "parent")
	if w0 == repo {
		t.Fatalf("the parent ran in the repo itself")
	}
	// The asker's token reads across its tree, and a turn carries no folder or
	// mode of its own.
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v0/sessions/" + c1.Session + "/events", "", 200},
		{"GET", "/api/v0/turns?session=" + c2.Session, "", 200},
		{"GET", "/api/v0/turns/" + c1.Turn, "", 200},
		{"GET", "/api/v0/sessions/" + c1.Session, "", 200},
		{"POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","isolate":false}`, 400},
		{"POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","folder":"` + filepath.Dir(repo) + `"}`, 400},
	} {
		if code, out := as(t, e, ptok, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("parent %s %s: %d %s, want %d", tc.method, tc.path, code, out, tc.want)
		}
	}

	// The children run one at a time in the asker's copy, after the asker's turn
	// ends (the workspace lane). While "one" runs, its token reads across the
	// tree but writes to no other session and does not stop the person's work.
	release(dir, "parent")
	ctok := tokenOf(t, dir, "one")
	w1 := pwdOf(t, dir, "one")
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v0/sessions/" + p.Session + "/events", "", 200},
		{"GET", "/api/v0/sessions/" + c2.Session + "/events", "", 200},
		// It may stop any turn it may read, as its owner may (O2.3): the
		// person's turn has ended here, so the stop passes the door and is 409.
		// A continuation in its reach is TestAskTokenContinuesTheAskedSession.
		{"POST", "/api/v0/turns/" + p.Turn + "/stop", "", 409},
	} {
		if code, out := as(t, e, ctok, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("child %s %s: %d %s, want %d", tc.method, tc.path, code, out, tc.want)
		}
	}
	release(dir, "one")
	w2 := pwdOf(t, dir, "two")
	release(dir, "two")
	if w1 != w0 || w2 != w0 {
		t.Fatalf("asked turns not in the asker's copy: parent %s, one %s, two %s", w0, w1, w2)
	}
	// Both asked turns ran; each names who asked it.
	for _, c := range []Started{c1, c2} {
		list, err := e.store.Turns(store.TurnQuery{Session: c.Session, Limit: 5})
		if err != nil || len(list.Turns) != 1 {
			t.Fatalf("child %s: %v %+v", c.Session, err, list)
		}
		if o := list.Turns[0].Origin; o == nil || o.Kind != event.OriginAsked || o.Session != p.Session || o.Turn != p.Turn {
			t.Errorf("child %s origin %+v", c.Session, o)
		}
	}
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
	// Depth: a chain root -> d1 -> d2, and d2's ask of d3 is refused at the depth
	// cap. The chain shares one workspace and runs one at a time, so release each
	// asker to let the next run and get its token.
	e.firstTurn(t, folder, "claude", "root", true)
	ask(t, e, tokenOf(t, dir, "root"), `{"agent":"claude","prompt":"d1"}`)
	release(dir, "root")
	ask(t, e, tokenOf(t, dir, "d1"), `{"agent":"claude","prompt":"d2"}`)
	release(dir, "d1")
	code, out := askRaw(t, e, tokenOf(t, dir, "d2"), `{"agent":"claude","prompt":"d3"}`)
	if code != 403 || !strings.Contains(out, "levels") || !strings.Contains(out, "(Depth in Settings, Limits)") {
		t.Errorf("depth 3: %d %s", code, out)
	}
	release(dir, "d2")
	// Per-turn cap: an asker's asks are counted when recorded, so a parked turn
	// reaches the cap without any asked turn running. A fresh turn, so the depth
	// chain's asks do not count against it.
	e.firstTurn(t, folder, "claude", "capper", true)
	ctok := tokenOf(t, dir, "capper")
	for i := 1; i <= settings.DefaultMaxJobsPerTurn; i++ {
		ask(t, e, ctok, `{"agent":"claude","prompt":"quick"}`)
	}
	code, out = askRaw(t, e, ctok, `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, fmt.Sprint(settings.DefaultMaxJobsPerTurn)) {
		t.Errorf("past the per-turn cap: %d %s", code, out)
	}
	release(dir, "capper")

	// A turn's own token may rename its own session.
	ro := e.firstTurn(t, folder, "claude", "ro", true)
	code, out = as(t, e, tokenOf(t, dir, "ro"), "PATCH", "/api/v0/sessions/"+ro.Session, `{"title":"x"}`)
	if code != 200 {
		t.Errorf("a turn could not rename its own session: %d %s", code, out)
	}
	release(dir, "ro")
}

// permissions.toml decides what a turn token may do, as the turn took it when
// it started (O2.4): a cap changed in the file, or a broken file, applies from
// the next turn and never reaches a running one, and a cost cap stops asking
// once the tree has spent it. Within its request tree a turn token always reads, lists
// agents, starts, reviews, stops and renames: the switches 0.5.2 retired
// still read, and change nothing. In a copy, a session of another tree in its
// own copy is outside its authority, and it reaches nothing there. No
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
	// A cap raised while the turn runs reaches the next turn, not this one.
	write("[limits]\nmax_jobs_per_turn = 3\n")
	if code, out := askRaw(t, e, tok, `{"agent":"claude","prompt":"quick"}`); code != 403 || !strings.Contains(out, "at most 2 turns or reviews") {
		t.Fatalf("a cap raised mid-turn reached the running turn: %d %s", code, out)
	}
	// Phase 1 checks that need the asker's own live token: a broken file does
	// not reach it either, and the routes that stay closed to a turn token
	// (they take the main token), while rename in its own tree is allowed.
	write("[agent_api]\napply = all\n")
	if code, out := as(t, e, tok, "GET", "/api/v0/harnesses", ""); code != 200 {
		t.Errorf("a file broken mid-turn reached the running turn: %d %s", code, out)
	}
	write("")
	if code, out := as(t, e, tok, "PATCH", "/api/v0/sessions/"+root.Session, `{"title":"Named by a turn"}`); code != 200 || !strings.Contains(out, "Named by a turn") {
		t.Errorf("rename in the tree: %d %s", code, out)
	}
	for _, r := range [][3]string{
		{"POST", "/api/v0/agents", `{"name":"x","harness":"claude","access":"subscription"}`},
		{"PUT", "/api/v0/agents/a_x", `{"name":"x","harness":"claude","access":"subscription"}`},
		{"DELETE", "/api/v0/agents/a_x", ""},
		// The settings: a turn token reads them but never writes them (O2.3).
		{"PUT", "/api/v0/settings/permissions", `{"forbid":[]}`},
		{"PUT", "/api/v0/settings/instructions", `{"text":"x"}`},
	} {
		if code, out := as(t, e, tok, r[0], r[1], r[2]); code != 403 || !strings.Contains(out, "main token") {
			t.Errorf("%s %s: %d %s, want 403", r[0], r[1], code, out)
		}
	}
	for _, path := range []string{"/api/v0/settings/permissions", "/api/v0/settings/instructions"} {
		if code, out := as(t, e, tok, "GET", path, ""); code != 200 {
			t.Errorf("GET %s: %d %s, want 200", path, code, out)
		}
	}

	// Phase 2: the asked "child" runs once the asker's turn ends, under the
	// file as it is then: every retired switch off, scope "session", and a
	// cost cap. Its own token reads and renames within its tree with the
	// retired switches off, and reaches nothing outside it.
	write("[agent_api]\nscope = \"session\"\nread = false\nlist_agents = false\nstart = false\nreview = false\ncancel = false\nrename = false\n" +
		"[limits]\nmax_cost_usd_per_tree = 0.25\n")
	release(dir, "root")
	child := tokenOf(t, dir, "child")
	// Another tree: a session the person started on their own, in its own
	// workspace, so it runs beside the child.
	other := e.firstTurn(t, t.TempDir(), "claude", "other", true)
	// Every retired switch off, scope "session": the file reads, and the
	// child's token still reads, lists, starts, reviews, stops and renames
	// within its tree, the person's session included.
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
	// Outside its tree, nothing: not to read, rename or stop.
	for _, tc := range []struct {
		name, method, path, body, want string
	}{
		{"read another tree", "GET", "/api/v0/sessions/" + other.Session + "/search?q=x", "", "in its authority"},
		{"read another tree by query", "GET", "/api/v0/turns?session=" + other.Session, "", "in its authority"},
		{"rename another tree's session", "PATCH", "/api/v0/sessions/" + other.Session, `{"title":"x"}`, "in its authority"},
		{"stop another tree's turn", "POST", "/api/v0/turns/" + other.Turn + "/stop", "", "stops only turns in the sessions it may read"},
	} {
		if code, out := as(t, e, child, tc.method, tc.path, tc.body); code != 403 || !strings.Contains(out, tc.want) {
			t.Errorf("%s: %d %s, want 403 with %q", tc.name, code, out, tc.want)
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
	// A session in its reach, so the refusal is the cost cap's: the cap is
	// checked where the job is made, after the reach.
	inReach := `{"session":"` + first.Session + `","agent":"claude","prompt":"quick"}`
	code, out = as(t, e, child, "POST", "/api/v0/turns", inReach)
	if code != 403 || !strings.Contains(out, "(Cost per request tree in Settings, Limits)") || !strings.Contains(out, "$0.30") {
		t.Errorf("past the cost cap: %d %s", code, out)
	}
	if again := ask(t, e, child, keyed); !again.Existing || again.Turn != first.Turn {
		t.Errorf("a retry past the cost cap: %+v, want turn %s again", again, first.Turn)
	}
	// The cap turned off while the child runs reaches the next turn only.
	write("[limits]\nmax_cost_usd_per_tree = 0\n")
	if code, out := as(t, e, child, "POST", "/api/v0/turns", inReach); code != 403 || !strings.Contains(out, "Cost per request tree") {
		t.Errorf("a cap turned off mid-turn reached the running turn: %d %s", code, out)
	}
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
	// A turn carries no folder: a turn token naming one is refused.
	if code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","folder":"`+t.TempDir()+`"}`); code != 400 {
		t.Errorf("a turn naming a folder: %d %s, want 400", code, out)
	}
	// The asked turn runs in the asker's copy, not a copy of its own and not the
	// person's folder. It waits in the workspace lane for the asker's turn, so
	// release root first; the copy outlives the turn.
	ask(t, e, tok, `{"agent":"claude","prompt":"in-copy"}`)
	release(dir, "root")
	if got := pwdOf(t, dir, "in-copy"); got != copyDir {
		t.Errorf("the asked turn ran in %q, want the asker's copy %q", got, copyDir)
	}
	release(dir, "in-copy")
}

// A turn token's ask answers at once (C1, 2026-11-25: wait is gone). The
// asked turn runs in the asker's workspace lane after the asker's turn ends,
// so it answers queued, and the answer comes back as a later turn. A wait
// query a client still sends is ignored.
func TestAskAnswersAtOnce(t *testing.T) {
	e, dir := askEnv(t)
	e.firstTurn(t, t.TempDir(), "claude", "root", true)
	tok := tokenOf(t, dir, "root")
	for _, path := range []string{"/api/v0/turns", "/api/v0/turns?wait=true", "/api/v0/turns?wait=false"} {
		code, out := as(t, e, tok, "POST", "/api/v0/sessions", `{}`)
		if code != 201 {
			t.Fatalf("open a session: %d %s", code, out)
		}
		var sc SessionCreated
		if err := json.Unmarshal([]byte(out), &sc); err != nil {
			t.Fatal(err)
		}
		body := `{"session":"` + sc.Session + `","agent":"claude","prompt":"q"}`
		if code, out := as(t, e, tok, "POST", path, body); code != 202 || !strings.Contains(out, `"state":"queued"`) {
			t.Errorf("%s: %d %s, want 202 queued", path, code, out)
		}
	}
	release(dir, "root")
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
	// The asked turn waits in the workspace lane for the asker's turn, so release
	// root first; the worktree outlives the turn.
	release(dir, "root")
	if got := pwdOf(t, dir, "child"); got != worktree {
		t.Errorf("the asked turn ran in %q, want the asker's worktree %q", got, worktree)
	}
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
	// The asked child runs in the asker's copy, after the asker's turn ends (the
	// workspace lane). Release root; its edit and its diff outlive the turn, and
	// the child reads both once it runs.
	release(dir, "root")
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

// A delivery past a cap is refused, and the record says so (his rule,
// 2026-11-28; Codex review t_58b5f94f2ace24d3, P1). A delivered turn is a turn
// its on's maker asked for, so with one job per turn, the parent's two ons
// over its own session, into two sessions of its own, deliver once: the
// second is refused, and delivery.refused in that session says why.
func TestDeliveryPastACapIsRefusedOnTheRecord(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("[limits]\nmax_jobs_per_turn = 1\nmax_jobs_per_tree = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := askEnvWith(t, perm)
	p := e.firstTurn(t, gitRepo(t), "claude", "parent", true)
	ptok := tokenOf(t, dir, "parent")
	var into []string
	for range 2 {
		code, out := as(t, e, ptok, "POST", "/api/v0/sessions", `{}`)
		var sc SessionCreated
		if code != 201 || json.Unmarshal([]byte(out), &sc) != nil {
			t.Fatalf("open a session: %d %s", code, out)
		}
		if code, out := as(t, e, ptok, "POST", "/api/v0/ons", `{"event":"turn.completed","filter":{"session":"`+p.Session+`"},"session":"`+sc.Session+`"}`); code != 201 {
			t.Fatalf("an on: %d %s", code, out)
		}
		into = append(into, sc.Session)
	}
	release(dir, "parent")
	waitEnded(t, e, p.Turn)
	delivered := func(session string) []queue.Job {
		all, err := e.q.List(queue.DefaultTeam, queue.Filter{Session: session}, 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []queue.Job
		for _, j := range all {
			if j.Links.ResultOf != nil && j.Links.ResultOf.Turn == p.Turn {
				out = append(out, j)
			}
		}
		return out
	}
	var why string
	for deadline := time.Now().Add(deliverWait); time.Now().Before(deadline) && why == ""; time.Sleep(50 * time.Millisecond) {
		if _, _, err := jobs.RecordTurnEnds(e.q, e.store, nil, time.Now().Add(time.Hour), 200); err != nil {
			t.Fatal(err)
		}
		if _, err := e.q.FireWatchOns(queue.DefaultTeam, recordReader(e.store), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if code, out := as(t, e, token, "GET", "/api/v0/sessions/"+into[1]+"/events", ""); code == 200 && strings.Contains(out, `"kind":"delivery.refused"`) {
			why = out
		}
	}
	if d := delivered(into[0]); len(d) != 1 || d[0].Links.Chain == nil || d[0].Links.Chain.By != p.Turn {
		t.Fatalf("into the first session: %+v, want one delivery the parent asked for", d)
	}
	if d := delivered(into[1]); len(d) != 0 {
		t.Fatalf("into the second session: %+v, want none", d)
	}
	if !strings.Contains(why, "(Turns per request in Settings, Limits)") || !strings.Contains(why, `"turn":"`+p.Turn+`"`) {
		t.Fatalf("the refusal on the record: %q, want the per-turn cap and the parent's turn as the work", why)
	}
}
