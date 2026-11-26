package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// seedApproval writes an approval.requested event to control's record, as the
// gateway that refused the command would — including a gateway on a separate
// runner, whose only trace on control is the relayed event. Control holds no
// in-memory state for it; the list and the answer come from the record alone.
func seedApproval(t *testing.T, e *env, id, session, turn string, commands, rules []string) {
	t.Helper()
	_, _, err := e.store.Append(event.Event{
		ID: "evt-" + id, Kind: event.ApprovalRequested, Session: session, Turn: turn,
		Approval: id, ToolID: "toolu_" + id, ToolName: "Bash", Rule: firstOf(rules),
		Commands: commands, Rules: rules, Time: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// decidedText returns the text of the approval.decided event for id, if one was
// written.
func decidedText(t *testing.T, e *env, id string) (string, bool) {
	t.Helper()
	evs, err := e.store.ApprovalEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == event.ApprovalDecided && ev.Approval == id {
			return ev.Text, true
		}
	}
	return "", false
}

func askStack(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "permissions.toml"), []byte("ask = [\"git push\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return stackEnv(t, stack{permissions: filepath.Join(dir, "permissions.toml")})
}

// The bug this fixes: an ask command refused by a gateway on a separate runner
// reaches control only as a record event, with no in-memory registry on
// control. GET /api/v0/approvals must still list it, and an answer must still
// work, because control derives both from the record. The id carries a foreign
// run (ap-REMOTE-1), so nothing in this process holds it.
func TestApprovalsListFromRecord(t *testing.T) {
	e := askStack(t)
	seedApproval(t, e, "ap-REMOTE-1", "s1", "t_gone", []string{"git push"}, []string{"git push"})

	_, out := send(t, e, "GET", "/api/v0/approvals", "", "")
	if !strings.Contains(out, "ap-REMOTE-1") || !strings.Contains(out, "git push") {
		t.Fatalf("the remote runner's approval was not listed from the record:\n%s", out)
	}

	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap-REMOTE-1", "", `{"decision":"allow"}`); resp.StatusCode != 204 {
		t.Fatalf("allow: %d %s", resp.StatusCode, out)
	}
	if _, out := send(t, e, "GET", "/api/v0/approvals", "", ""); strings.Contains(out, "ap-REMOTE-1") {
		t.Fatalf("the answered approval is still listed:\n%s", out)
	}
	if txt, ok := decidedText(t, e, "ap-REMOTE-1"); !ok || !strings.Contains(txt, "allowed from now on by a person") {
		t.Fatalf("approval.decided was not recorded: %q ok=%v", txt, ok)
	}
}

// The listed approval names the session's AGENT (harness), not the shell tool:
// the person sees "claude" (a cockpit shows its product name), not the "Bash"
// the command ran under. The harness comes from the session's turn.started.
func TestApprovalsListShowsHarnessNotTool(t *testing.T) {
	e := askStack(t)
	if _, _, err := e.store.Append(event.Event{ID: "ts-1", Kind: event.TurnStarted,
		Session: "s1", Turn: "t_1", Harness: "claude", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	seedApproval(t, e, "ap1", "s1", "t_1", []string{"git push"}, []string{"git push"})
	resp, out := send(t, e, "GET", "/api/v0/approvals", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	if !strings.Contains(out, `"agent":"claude"`) {
		t.Fatalf("the approval did not name the session's harness as the agent:\n%s", out)
	}
	if strings.Contains(out, `"agent":"Bash"`) {
		t.Fatalf("the approval named the shell tool as the agent:\n%s", out)
	}
}

// A held call carries its commands once, as the commands array; it no longer
// also sends them joined in a command string (one fact, one place).
func TestApprovalsListCommandsOnly(t *testing.T) {
	e := askStack(t)
	seedApproval(t, e, "ap1", "s1", "t_1", []string{"git push", "rm -rf x"}, []string{"git push"})
	resp, out := send(t, e, "GET", "/api/v0/approvals", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	if !strings.Contains(out, `"commands":["git push","rm -rf x"]`) {
		t.Fatalf("the approval did not carry its commands array:\n%s", out)
	}
	if strings.Contains(out, `"command":`) {
		t.Fatalf("the approval still carries the joined command string:\n%s", out)
	}
}

// Allow moves each approved command from ask to allow in the settings (so the
// runner allows it from the next turn) and records the answer. The continuation
// turn is skipped here because the turn that asked is gone.
func TestDecideApprovalAllowMovesAskToAllow(t *testing.T) {
	e := askStack(t)
	seedApproval(t, e, "ap1", "s1", "t_gone", []string{"git push"}, []string{"git push"})
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"always"}`); resp.StatusCode != 204 {
		t.Fatalf("always: %d %s", resp.StatusCode, out)
	}
	_, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	var p settings.Policy
	if err := json.Unmarshal([]byte(out), &p); err != nil || !slices.Contains(p.Allow, "git push") || len(p.Ask) != 0 {
		t.Fatalf("the command was not moved from ask to allow:\n%s", out)
	}
	if txt, ok := decidedText(t, e, "ap1"); !ok || txt != "allowed from now on by a person" {
		t.Fatalf("the answer was not recorded: %q ok=%v", txt, ok)
	}
}

// Refuse records the answer and changes no settings.
func TestDecideApprovalRefuseRecordsOnly(t *testing.T) {
	e := askStack(t)
	seedApproval(t, e, "ap1", "s1", "t_gone", []string{"git push"}, []string{"git push"})
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"refuse"}`); resp.StatusCode != 204 {
		t.Fatalf("refuse: %d %s", resp.StatusCode, out)
	}
	_, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if !strings.Contains(out, `"ask":["git push"]`) || strings.Contains(out, `"allow":["git push"]`) {
		t.Fatalf("refuse changed the settings:\n%s", out)
	}
	if txt, ok := decidedText(t, e, "ap1"); !ok || txt != "refused by a person" {
		t.Fatalf("the answer was not recorded: %q ok=%v", txt, ok)
	}
}

// assertNoContinuation fails if any job in the session is a continuation turn:
// H1 starts none — an allow releases the held reply in the turn already open, a
// refuse or a timeout ends it, and neither costs an extra turn (the review-loop
// case the earlier design suffered). It also fails if a stray turn was queued.
func assertNoContinuation(t *testing.T, e *env) {
	t.Helper()
	jobs, err := e.q.List(queue.DefaultTeam, queue.Filter{Session: "s1"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if strings.HasPrefix(j.Spec.Prompt, "A person approved:") {
			t.Fatalf("a continuation turn was started: %q", j.Spec.Prompt)
		}
	}
}

// The review-loop case the brief names: the earlier hold design started an
// automatic next turn on an approval, so a reviewing agent that kept proposing
// gated commands spawned a turn per approval. H1 starts NONE — for every
// decision, once, always and refuse alike. An allow runs in the turn the gateway
// already holds; a refuse or a timeout ends it.
func TestDecideApprovalNoContinuationTurn(t *testing.T) {
	for _, decision := range []string{"once", "always", "refuse"} {
		e := askStack(t)
		// The turn that asked, so there would be an agent to continue on if H1
		// still continued.
		orig, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "",
			queue.Spec{Session: "s1", Harness: "claude", AgentID: "a1", Access: "subscription", Prompt: "do a thing"},
			queue.Links{}, queue.Limits{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		seedApproval(t, e, "ap1", "s1", orig.Turn, []string{"git push"}, []string{"git push"})
		if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"`+decision+`"}`); resp.StatusCode != 204 {
			t.Fatalf("%s: %d %s", decision, resp.StatusCode, out)
		}
		assertNoContinuation(t, e)
		if _, ok := decidedText(t, e, "ap1"); !ok {
			t.Fatalf("%s: the answer was not recorded", decision)
		}
	}
}

// Allow moves the command to allow in the settings, for later turns, and starts
// no continuation (the held reply runs in the turn already open).
func TestDecideApprovalAllowMovesAndDoesNotContinue(t *testing.T) {
	e := askStack(t)
	orig, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "",
		queue.Spec{Session: "s1", Harness: "claude", AgentID: "a1", Access: "subscription", Prompt: "do a thing"},
		queue.Links{}, queue.Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedApproval(t, e, "ap1", "s1", orig.Turn, []string{"git push"}, []string{"git push"})
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"allow"}`); resp.StatusCode != 204 {
		t.Fatalf("allow: %d %s", resp.StatusCode, out)
	}
	assertNoContinuation(t, e)
	_, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	var p settings.Policy
	if err := json.Unmarshal([]byte(out), &p); err != nil || !slices.Contains(p.Allow, "git push") {
		t.Fatalf("the command was not moved to allow:\n%s", out)
	}
}

// Once writes nothing to the settings and starts no continuation; it records the
// answer so the held reply is released.
func TestDecideApprovalOnce(t *testing.T) {
	e := askStack(t)
	orig, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "",
		queue.Spec{Session: "s1", Harness: "claude", AgentID: "a1", Access: "subscription", Prompt: "do a thing"},
		queue.Links{}, queue.Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedApproval(t, e, "ap1", "s1", orig.Turn, []string{"make deploy"}, []string{"make deploy"})
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"once"}`); resp.StatusCode != 204 {
		t.Fatalf("once: %d %s", resp.StatusCode, out)
	}
	assertNoContinuation(t, e)
	if _, out := send(t, e, "GET", "/api/v0/settings/permissions", "", ""); strings.Contains(out, `"allow":["make deploy"]`) {
		t.Fatalf("allow once wrote to the settings:\n%s", out)
	}
	if txt, ok := decidedText(t, e, "ap1"); !ok || txt != "allowed once by a person" {
		t.Fatalf("the answer was not recorded: %q ok=%v", txt, ok)
	}
}

// Two Allows at once (two tabs, a retry) are answered once: one wins (204), the
// other is refused (409), and neither starts a continuation.
func TestDecideApprovalConcurrentAllow(t *testing.T) {
	e := askStack(t)
	orig, _, err := e.q.CreateTurnLinked(queue.DefaultTeam, "",
		queue.Spec{Session: "s1", Harness: "claude", AgentID: "a1", Access: "subscription", Prompt: "x"},
		queue.Links{}, queue.Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedApproval(t, e, "ap1", "s1", orig.Turn, []string{"git push"}, []string{"git push"})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"allow"}`)
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	got := map[int]int{}
	for _, c := range codes {
		got[c]++
	}
	if got[204] != 1 || got[409] != 1 {
		t.Fatalf("want one 204 and one 409, got %v", codes)
	}
	assertNoContinuation(t, e)
}

// Two Allows on DIFFERENT commands at once both succeed: the settings write
// retries the concurrent-change conflict rather than failing one with 500.
func TestDecideApprovalConcurrentDifferentCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "permissions.toml"), []byte("ask = [\"git push\", \"npm publish\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := stackEnv(t, stack{permissions: filepath.Join(dir, "permissions.toml")})
	seedApproval(t, e, "ap1", "s1", "t_gone", []string{"git push"}, []string{"git push"})
	seedApproval(t, e, "ap2", "s1", "t_gone", []string{"npm publish"}, []string{"npm publish"})
	var wg sync.WaitGroup
	codes := map[string]int{}
	var mu sync.Mutex
	for _, id := range []string{"ap1", "ap2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			resp, _ := send(t, e, "POST", "/api/v0/approvals/"+id, "", `{"decision":"allow"}`)
			mu.Lock()
			codes[id] = resp.StatusCode
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	if codes["ap1"] != 204 || codes["ap2"] != 204 {
		t.Fatalf("both allows should succeed: %v", codes)
	}
	_, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	if !strings.Contains(out, "git push") || !strings.Contains(out, "npm publish") || !strings.Contains(out, `"ask":[]`) {
		t.Fatalf("both commands were not moved to allow:\n%s", out)
	}
}

// When the turn that asked is gone, allow still moves the command to allow and
// starts no continuation.
func TestDecideApprovalAllowAskingTurnGone(t *testing.T) {
	e := askStack(t)
	seedApproval(t, e, "ap1", "s1", "t_gone", []string{"git push"}, []string{"git push"})
	if resp, out := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"allow"}`); resp.StatusCode != 204 {
		t.Fatalf("allow: %d %s", resp.StatusCode, out)
	}
	jobs, err := e.q.List(queue.DefaultTeam, queue.Filter{Session: "s1"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a continuation started for a gone turn: %d jobs", len(jobs))
	}
	_, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
	var p settings.Policy
	if err := json.Unmarshal([]byte(out), &p); err != nil || !slices.Contains(p.Allow, "git push") {
		t.Fatalf("the command was not moved to allow:\n%s", out)
	}
}

// An unknown id is 404, and an id already answered is 409 the second time:
// the same reply as an answer that meets one still in progress.
func TestDecideApprovalUnknownAndAnswered(t *testing.T) {
	e := askStack(t)
	if resp, _ := send(t, e, "POST", "/api/v0/approvals/nope", "", `{"decision":"allow"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown id: %d", resp.StatusCode)
	}
	seedApproval(t, e, "ap1", "s1", "t_gone", []string{"git push"}, []string{"git push"})
	if resp, _ := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"refuse"}`); resp.StatusCode != 204 {
		t.Fatalf("first answer: %d", resp.StatusCode)
	}
	if resp, _ := send(t, e, "POST", "/api/v0/approvals/ap1", "", `{"decision":"refuse"}`); resp.StatusCode != 409 {
		t.Fatalf("an already-answered id was not 409: %d", resp.StatusCode)
	}
}
