//go:build integration

package api

// Runner-backed tests for the eight review findings the workspace model
// eliminates (DESIGN_workspaces.md section 6, as the "Decided" and "API rules"
// sections adapt it). Each finding came from deriving a session's mode and
// first-turn status across session.created, turn.started, jobs.spec and
// queue.sessions. With the folder and mode on workspace.created before any turn,
// and a turn that carries only its session, each is impossible. Where an earlier
// test already pins a finding, it is named here rather than repeated:
//
//   #1 first and second turn share the workspace copy: TestWorkspaceTurnsShareTheCopy (here).
//   #2 an ask from a later turn stays in the asker's sandbox copy: TestAskFromALaterTurnStaysInTheSandbox (here);
//      the turn number cannot change it, since an ask takes the asker's workspace id, never a turn's Isolate.
//   #3 a second turn queued before the first starts: TestNextTurnQueuesWhileAnotherIsStarting (turn_test.go) --
//      a second turn while the first is starting returns 202 queued, ahead 1, and both run in order.
//   #4 an idempotent retry across the turn's lifecycle: TestTurnWaitFalseRetryAfterStop (jobs_test.go, queued then stopped)
//      and TestIdempotencyKey (turnsapi_integration, same key same turn; a different session/fields is 409).
//   #5 a turn after a queued or cancelled first turn: TestNextTurnQueuesWhileAnotherIsStarting (queued first turn, then a
//      third after both end) and TestTurnWaitFalseRetryAfterStop (a turn stopped while queued, then its retry).
//   #6 a new session reports sandbox: true while its first turn runs: TestNewSessionReportsSandboxWhileTurnRuns (here).
//   #7 changes while the first turn runs report isolated by the workspace's sandbox: TestSessionChangesFirstTurnRunning
//      (changes_integration) -- a sandbox workspace answers isolated: true while its first turn runs, a direct one false.
//   #8 a sandbox turn ignores isolation = none, and a turn carries no folder: TestSandboxTrueIgnoresIsolationNone (here),
//      and a turn naming a folder is refused in TestAskInOwnCopy.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// findingsEnv runs each turn as a shell that writes its working folder to
// <dir>/pwd-<prompt> and appends its prompt to trace.txt in that folder, so a
// test can see which copy a turn ran in and whether a later turn sees an earlier
// one's writes. A prompt that starts "hold" waits for <dir>/release-<prompt>.
func findingsEnv(t *testing.T, perm string) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	script := `for a; do p="$a"; done; echo '{"type":"result","session_id":"sid-'$$'"}'; ` +
		`pwd > "$D/pwd-$p"; printf '%s\n' "$p" >> trace.txt; env > "$D/env-$p.tmp"; mv "$D/env-$p.tmp" "$D/env-$p"; ` +
		`case "$p" in hold*) i=0; while [ ! -e "$D/release-$p" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done;; esac`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	return stackEnv(t, stack{wire: wire, permissions: perm}), dir
}

// #1: a workspace's first and second turn run in one copy, named after the
// workspace, and the second sees the first's file writes. (Parked P1.1: the
// first turn of an explicit session set spec.Session, so the runner treated it
// as a continuation and no copy was made.)
func TestWorkspaceTurnsShareTheCopy(t *testing.T) {
	e, dir := findingsEnv(t, "")
	folder := t.TempDir()
	ref, machine := e.folderRef(t, folder)
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	t1 := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"one"}`)
	waitEnded(t, e, t1.Turn)
	t2 := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"two"}`)
	waitEnded(t, e, t2.Turn)
	copyDir := pwdOf(t, dir, "one")
	if copyDir == folder {
		t.Fatalf("the first turn ran in the folder, not a copy")
	}
	if copyDir != pwdOf(t, dir, "two") {
		t.Fatalf("the two turns ran in different copies: %s and %s", copyDir, pwdOf(t, dir, "two"))
	}
	if !strings.HasSuffix(copyDir, filepath.Join("sessions", ws)) {
		t.Errorf("the copy %s is not named after the workspace %s", copyDir, ws)
	}
	if b, err := os.ReadFile(filepath.Join(copyDir, "trace.txt")); err != nil || string(b) != "one\ntwo\n" {
		t.Errorf("the copy's trace is %q (%v), want the second turn to see the first's write", b, err)
	}
}

// #2: an ask from a later turn (not the first) stays in the asker's sandbox
// copy, never the person's folder. (Parked P1.2: ask_agent from turn 2+ read
// aj.Spec.Isolate -- false on a continuation -- and escaped into the real
// folder.) An ask takes the asker's workspace id, so the turn it is asked from
// cannot change where the asked turn runs.
func TestAskFromALaterTurnStaysInTheSandbox(t *testing.T) {
	e, dir := askEnv(t)
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "a.txt"), []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref, machine := e.folderRef(t, folder)
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	// Turn 1 ends; turn 2 is held running and asks from its own token.
	t1 := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"quick"}`)
	waitEnded(t, e, t1.Turn)
	startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"two"}`)
	tok2 := tokenOf(t, dir, "two")
	ask(t, e, tok2, `{"agent":"claude","prompt":"child"}`)
	// The asked turn ran in the asker's copy, not the person's folder, and the
	// folder is untouched.
	if got := pwdOf(t, dir, "child"); got == folder || !strings.HasSuffix(got, filepath.Join("sessions", ws)) {
		t.Errorf("the asked turn ran in %q, want the asker's copy of workspace %s, not the folder %s", got, ws, folder)
	}
	if b, err := os.ReadFile(filepath.Join(folder, "a.txt")); err != nil || string(b) != "orig\n" {
		t.Errorf("the person's folder changed to %q, want it untouched by the sandboxed ask", b)
	}
	for _, p := range []string{"two", "child"} {
		release(dir, p)
	}
}

// #6: a session whose first turn is still running reports sandbox: true. (Parked
// P1.2: a one-call sandbox session had no sessions row until turn 1 ended, so
// GET /sessions reported sandbox: false while it ran.) The workspace records the
// mode at creation, so Store.Workspace answers sandbox: true at once.
func TestNewSessionReportsSandboxWhileTurnRuns(t *testing.T) {
	e, dir := findingsEnv(t, "")
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"hold"}`)
	tokenOf(t, dir, "hold") // the first turn is running now
	for _, path := range []string{"/api/v0/sessions/" + sess, "/api/v0/sessions"} {
		if _, out := do(t, e.req(t, "GET", path, nil)); !strings.Contains(out, `"sandbox":true`) {
			t.Errorf("GET %s while the first turn runs: %s, want sandbox: true", path, out)
		}
	}
	release(dir, "hold")
}

// #8: a sandbox workspace isolates in a copy even when the isolation setting is
// none -- the workspace's sandbox is the one switch, and the setting only picks
// the backend. (Parked P2.5: isolation = none contradicted sandbox: true.)
func TestSandboxTrueIgnoresIsolationNone(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("isolation = \""+settings.IsolationNone+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, dir := findingsEnv(t, perm)
	folder := t.TempDir()
	ref, machine := e.folderRef(t, folder)
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	t1 := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"one"}`)
	waitEnded(t, e, t1.Turn)
	if pwd := pwdOf(t, dir, "one"); pwd == folder {
		t.Errorf("the sandbox turn ran in the folder %s despite isolation = none", folder)
	}
	if _, out := do(t, e.req(t, "GET", "/api/v0/sessions/"+sess, nil)); !strings.Contains(out, `"sandbox":true`) {
		t.Errorf("GET /sessions/%s: %s, want sandbox: true", sess, out)
	}
}
