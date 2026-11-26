//go:build integration

package api

// A test for each defect the 2026-11-21 agy review found in the workspace
// cutover, so the fix for each is pinned. The fixes all follow one rule: the
// workspace is the key -- turn.started carries it, and the copy, its head turn,
// its changes and its one-turn-per-ask boundary resolve by workspace id, not by
// session.
//
//   R1 a failed start on a loaded copy must not remove it: TestFailedAskedTurnKeepsTheAskersCopy
//   R2 an ask is one turn, no continuation: TestAskTokenCannotContinueTheAskedSession
//   R3 an asked turn diffs from the workspace's latest checkpoint, not base: TestAskedTurnDiffsFromTheWorkspaceHead
//   R4 a workspace's head_turn is populated: TestWorkspaceHeadTurnResolvesByWorkspace
//   R5 changes before the first turn read the workspace's sandbox: TestChangesBeforeFirstTurnReadSandbox
//   R6 an asked session names its asker before its turn starts: TestAskedSessionShowsOriginBeforeTurn

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// R3-P1.1: a delivered result runs in the asker's workspace (the one copy), so
// its turn.started carries the asker's workspace rather than running in the
// folder as it is with no changeset. (Review round 3 P1: deliverResult built a
// spec with no Workspace.)
func TestDeliveredResultRunsInTheAskersWorkspace(t *testing.T) {
	e, dir := askEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	root := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"hold"}`)
	tok := tokenOf(t, dir, "hold")
	ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`) // ends at once, delivers its result to root
	release(dir, "hold")                                  // the delivery turn runs once root's turn frees the session
	got := deliverUntil(t, e, root.Session, 1)
	recs, err := e.store.After(0, store.Filter{Turn: got[0].ID}, 50)
	if err != nil {
		t.Fatal(err)
	}
	var started *event.Event
	for i := range recs {
		var ev event.Event
		if recs[i].Kind == event.TurnStarted && json.Unmarshal(recs[i].Data, &ev) == nil && ev.Turn == got[0].ID {
			started = &ev
		}
	}
	if started == nil || started.Workspace != ws {
		t.Errorf("the delivered result's turn.started workspace is %v, want the asker's %s", started, ws)
	}
}

// R3-P1.4: a workspace's applied_turn resolves by workspace id, so an apply
// shows on GET /api/v0/workspaces/{id} even for a ws_ workspace (the apply
// writes workspace.applied). (Review round 3 P1: applied_turn matched
// turn.applied by session, never the ws_ id.)
func TestWorkspaceAppliedTurnResolvesByWorkspace(t *testing.T) {
	e, _ := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	turn := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"edit"}`)
	waitEnded(t, e, turn.Turn)
	if code, st := applyAs(t, e, token, turn.Turn); code != 200 || st.State != "applied" {
		t.Fatalf("apply: %d %+v", code, st)
	}
	var wsp store.Workspace
	for deadline := time.Now().Add(10 * time.Second); ; {
		if code := getJSON(t, e, "/api/v0/workspaces/"+ws, &wsp); code == 200 && wsp.AppliedTurn == turn.Turn {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /workspaces/%s applied_turn %q, want %q", ws, wsp.AppliedTurn, turn.Turn)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// R3-P1.5: POST /api/v0/workspaces with a folder_ref and no machine records the
// folder registry's machine, not an empty one, so apply and changes can route
// later. (Review round 3 P1: an empty machine broke apply/changes.)
func TestWorkspaceFromFolderRefGetsItsMachine(t *testing.T) {
	e, _ := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(`{"folder_ref":"`+ref+`"}`)))
	var wsp store.Workspace
	if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &wsp) != nil || wsp.Machine != machine {
		t.Errorf("workspace from folder_ref with no machine: %d %s, want machine %q", resp.StatusCode, raw, machine)
	}
}

// R4-P1.5: two first turns of two sessions in one workspace, started at once,
// share the one copy -- the runner opens it once and the other loads it, rather
// than racing in the copy backend. (Review round 4 P1: no per-workspace lock.)
func TestConcurrentFirstTurnsShareOneCopy(t *testing.T) {
	e, dir := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	s1 := e.makeSession(t, ws)
	s2 := e.makeSession(t, ws)
	codes := make(chan int, 2)
	post := func(sess, prompt string) {
		resp, _ := do(t, e.req(t, "POST", "/api/v0/turns",
			strings.NewReader(`{"session":"`+sess+`","agent":"claude","prompt":"`+prompt+`"}`)))
		codes <- resp.StatusCode
	}
	go post(s1, "one")
	go post(s2, "two")
	for range 2 {
		if c := <-codes; c != http.StatusAccepted {
			t.Fatalf("concurrent first turn: %d", c)
		}
	}
	a, b := pwdOf(t, dir, "one"), pwdOf(t, dir, "two")
	if a != b || !strings.HasSuffix(a, filepath.Join("sessions", ws)) {
		t.Errorf("turns ran in %q and %q, want the one copy of workspace %s", a, b, ws)
	}
}

// R4-P2.7: a second session of a workspace sees the workspace's unapplied
// changes, even before it has run a turn of its own. (Review round 4 P2:
// workspaceID looked only in jobs, so a turnless session resolved to itself.)
func TestSecondSessionSeesWorkspaceChanges(t *testing.T) {
	e, _ := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	s1 := e.makeSession(t, ws)
	s2 := e.makeSession(t, ws) // a second session, no turn of its own
	t1 := startTurn(t, e, `{"session":"`+s1+`","agent":"claude","prompt":"edit"}`)
	waitEnded(t, e, t1.Turn)
	// s1's changes are the workspace's, so s2 sees the same unapplied changes.
	c1 := changesOf(t, e, s1)
	c2 := changesOf(t, e, s2)
	if !c1.Isolated || !c1.Unapplied || c1.Count == 0 {
		t.Fatalf("s1 changes: %+v", c1)
	}
	if !c2.Isolated || !c2.Unapplied || c2.Count != c1.Count {
		t.Errorf("s2 changes: %+v, want the workspace's %d unapplied like s1", c2, c1.Count)
	}
	// A fresh preview of s2 is not marked stale: previewStale resolves the
	// workspace, so s2's lack of turns of its own does not falsely stale it.
	if c2.Stale || c2.Why != "" {
		t.Errorf("s2 fresh preview: stale=%v why=%q, want a usable (not stale) preview", c2.Stale, c2.Why)
	}
}

// R5-P1-b: a turn that fails to start never removes the workspace's copy, even
// one it just made -- the copy belongs to the workspace, not the turn, so a
// concurrent or later turn of the workspace keeps it. (Round 5 P1: the
// failed-start teardown deleted a copy another turn was using.)
func TestFailedFirstTurnLeavesTheCopy(t *testing.T) {
	e := stackEnv(t, stack{wire: func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: filepath.Join(t.TempDir(), "missing-binary")}, nil
	}})
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns",
		strings.NewReader(`{"session":"`+sess+`","agent":"claude","prompt":"x"}`)))
	var st Started
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal([]byte(raw), &st) != nil {
		t.Fatalf("turn with a missing binary: %d %s, want 202", resp.StatusCode, raw)
	}
	if j := waitEnded(t, e, st.Turn); j.State != queue.Failed {
		t.Fatalf("turn with a missing binary ended %s, want it to fail to start", j.State)
	}
	// It made the copy before the start failed; the failed turn did not remove it.
	if _, err := os.Stat(filepath.Join(e.workdir, ws)); err != nil {
		t.Errorf("the workspace copy %s is gone after a failed start: %v", filepath.Join(e.workdir, ws), err)
	}
}

// R5-P1-c: a first turn of a new session in a direct (sandbox false) workspace
// that has a folder but no folder_ref (a gateway/hook or migrated workspace)
// runs in the workspace's folder, read from the workspace record -- not refused
// with a folder error because the session has no prior turn to read a folder
// from. (Round 5 P1: prepare read the folder from the session continuation.)
func TestNewSessionInDirectWorkspaceRuns(t *testing.T) {
	e, _ := reviewEnv(t)
	folder := t.TempDir()
	const wsID, sess = "ws_d2d2d2d2d2d2d2d2", "d1d1d1d1d1d1d1d1"
	sb := false
	for _, ev := range []event.Event{
		{ID: "wc_direct", Kind: event.WorkspaceCreated, Time: time.Now(), Workspace: wsID, Folder: folder, Sandbox: &sb},
		{ID: "sc_direct", Kind: event.SessionCreated, Time: time.Now(), Session: sess, Workspace: wsID},
	} {
		if _, _, err := e.store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	resp, raw := do(t, e.req(t, "POST", "/api/v0/turns",
		strings.NewReader(`{"session":"`+sess+`","agent":"claude","prompt":"run"}`)))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first turn in a direct workspace: %d %s, want 202, not a folder error", resp.StatusCode, raw)
	}
	var st Started
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	waitEnded(t, e, st.Turn)
	if _, err := os.Stat(filepath.Join(folder, "run.txt")); err != nil {
		t.Errorf("the turn did not run in the workspace's folder %s: %v", folder, err)
	}
}

// reviewEnv runs each turn as a shell: a "hold" prompt writes marker.txt into
// its copy and waits for release; any other prompt writes <prompt>.txt into its
// copy and ends. Every turn writes its folder to <dir>/pwd-<prompt> and its
// environment to <dir>/env-<prompt>.
func reviewEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	script := `for a; do p="$a"; done; echo '{"type":"result","session_id":"sid-'$$'"}'; ` +
		`pwd > "$D/pwd-$p"; env > "$D/env-$p.tmp"; mv "$D/env-$p.tmp" "$D/env-$p"; ` +
		`case "$p" in hold*) printf held > marker.txt; i=0; while [ ! -e "$D/release-$p" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done;; ` +
		`*) printf '%s\n' "$p" > "$p.txt";; esac`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	return stackEnv(t, stack{wire: wire}), dir
}

// R1: a turn that LOADED an existing workspace copy and then fails to start must
// not remove the copy -- it belongs to the workspace's other turns. (Review P1.1:
// the load path set at.ws/at.space, so the start-failed cleanup closed the shared
// copy, discarding the asker's edits.)
func TestFailedAskedTurnKeepsTheAskersCopy(t *testing.T) {
	dir := t.TempDir()
	// The first turn (the asker) runs a shell that writes a marker and holds;
	// every later turn (the asked one) gets a missing binary and fails to start.
	held := `pwd > "$D/pwd-$p"; env > "$D/env-$p.tmp"; mv "$D/env-$p.tmp" "$D/env-$p"; printf held > marker.txt; ` +
		`i=0; while [ ! -e "$D/release-$p" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done`
	var once sync.Once
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			return run.Launch{Path: "/bin/sh", Args: []string{"-c", `for a; do p="$a"; done; echo '{"type":"result","session_id":"sid-'$$'"}'; ` + held, "sh"},
				Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
		}
		return run.Launch{Path: filepath.Join(t.TempDir(), "missing-binary")}, nil
	}
	e := stackEnv(t, stack{wire: wire})
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"hold"}`)
	tok := tokenOf(t, dir, "hold")
	copyDir := pwdOf(t, dir, "hold")
	// The asked turn loads the asker's copy, then fails to start.
	code, out := as(t, e, tok, "POST", "/api/v0/sessions", `{}`)
	if code != 201 {
		t.Fatalf("open asked session: %d %s", code, out)
	}
	var sc SessionCreated
	if err := json.Unmarshal([]byte(out), &sc); err != nil {
		t.Fatal(err)
	}
	code, out = as(t, e, tok, "POST", "/api/v0/turns", `{"session":"`+sc.Session+`","agent":"claude","prompt":"child"}`)
	if code != http.StatusAccepted {
		t.Fatalf("asked turn: %d %s, want it recorded", code, out)
	}
	var asked Started
	if err := json.Unmarshal([]byte(out), &asked); err != nil {
		t.Fatal(err)
	}
	// The asked turn runs after the asker's turn ends (the workspace lane), then
	// fails to start (a missing binary). Release the asker and wait for it.
	release(dir, "hold")
	waitEnded(t, e, asked.Turn)
	// The asker's copy and its marker survive the asked turn's failure.
	if b, err := os.ReadFile(filepath.Join(copyDir, "marker.txt")); err != nil || string(b) != "held" {
		t.Errorf("the asker's copy marker is %q (%v), want it intact after the asked turn failed", b, err)
	}
}

// R2, replaced by O2.3 (his call, 2026-11-25): an ask was one turn. A turn
// token now continues a session in its reach as its owner may: a second turn
// to the session it asked in queues behind the first.
func TestAskTokenContinuesTheAskedSession(t *testing.T) {
	e, dir := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"hold"}`)
	tok := tokenOf(t, dir, "hold")
	code, out := as(t, e, tok, "POST", "/api/v0/sessions", `{}`)
	if code != 201 {
		t.Fatalf("open asked session: %d %s", code, out)
	}
	var sc SessionCreated
	if err := json.Unmarshal([]byte(out), &sc); err != nil {
		t.Fatal(err)
	}
	if code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"session":"`+sc.Session+`","agent":"claude","prompt":"one"}`); code != http.StatusAccepted {
		t.Fatalf("the asked turn: %d %s", code, out)
	}
	// A second turn to the same asked session is a continuation, and queues.
	if code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"session":"`+sc.Session+`","agent":"claude","prompt":"two"}`); code != http.StatusAccepted {
		t.Errorf("second turn to the asked session: %d %s, want 202", code, out)
	}
	release(dir, "hold")
}

// R3: a turn in an asked session shares the asker's copy, so its changeset is
// what IT changed, diffed from the workspace's latest checkpoint, not base --
// the asker's earlier file changes are not re-attributed to it. (Review P1.3.)
func TestAskedTurnDiffsFromTheWorkspaceHead(t *testing.T) {
	e, dir := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	// Turn 1 writes a.txt and ends (its end checkpoint is the workspace head).
	t1 := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"a"}`)
	waitEnded(t, e, t1.Turn)
	// Turn 2 holds and asks; the asked turn writes b.txt and ends.
	startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"hold"}`)
	tok := tokenOf(t, dir, "hold")
	asked := ask(t, e, tok, `{"agent":"claude","prompt":"b"}`)
	// The asked turn runs after the asker's turn ends (the workspace lane), so
	// release "hold" and let the asked turn run to its end.
	release(dir, "hold")
	waitEnded(t, e, asked.Turn)
	// The asked turn diffs from the workspace head (the last end checkpoint), so
	// its own b.txt is a change and turn 1's ended a.txt is NOT re-attributed to
	// it.
	p := turnChangesOf(t, e, asked.Turn)
	paths := map[string]bool{}
	for _, f := range p.Changes {
		paths[f.Path] = true
	}
	if paths["a.txt"] || !paths["b.txt"] {
		t.Errorf("the asked turn's changes are %+v, want b.txt and NOT turn 1's ended a.txt", p.Changes)
	}
}

// R4: a workspace's head_turn is its latest turn that recorded a changeset,
// resolved by the workspace id now that turn.started carries it. (Review P1.4:
// it was always empty for a workspace created via POST /api/v0/workspaces.)
func TestWorkspaceHeadTurnResolvesByWorkspace(t *testing.T) {
	e, _ := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	turn := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"edit"}`)
	waitEnded(t, e, turn.Turn)
	var w store.Workspace
	if code := getJSON(t, e, "/api/v0/workspaces/"+ws, &w); code != 200 || w.HeadTurn != turn.Turn {
		t.Errorf("GET /workspaces/%s head_turn %q, want the turn %q", ws, w.HeadTurn, turn.Turn)
	}
}

// R5: changes on a sandbox session before its first turn is queued read the
// workspace's sandbox, so they answer isolated with nothing unapplied, not
// "works in your folder as it is". (Review P1.5.)
func TestChangesBeforeFirstTurnReadSandbox(t *testing.T) {
	e, _ := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	sandbox := e.makeSession(t, e.makeWorkspace(t, ref, machine, true))
	direct := e.makeSession(t, e.makeWorkspace(t, ref, machine, false))
	if c := changesOf(t, e, sandbox); !c.Isolated || c.Unapplied || len(c.Files) != 0 {
		t.Errorf("sandbox session before its first turn: %+v, want isolated with nothing unapplied", c)
	}
	if c := changesOf(t, e, direct); c.Isolated || c.Unapplied {
		t.Errorf("direct session before its first turn: %+v, want not isolated", c)
	}
}

// The copy's life belongs to the workspace, not any one session: archiving an
// asked session (a label) never removes or closes the asker's shared copy or its
// unapplied changes. (His call, 2026-11-21: sessions are labels, not tied to the
// workspace's life; archive is a view, recorded as session.archived, and touches
// no copy.)
func TestArchivingAnAskedSessionKeepsTheAskersCopy(t *testing.T) {
	e, dir := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"hold"}`)
	tok := tokenOf(t, dir, "hold")
	copyDir := pwdOf(t, dir, "hold")
	asked := ask(t, e, tok, `{"agent":"claude","prompt":"child"}`)
	// The asked turn runs in the asker's copy after the asker's turn ends (the
	// workspace lane), so release "hold" and let the asked turn run to its end.
	release(dir, "hold")
	waitEnded(t, e, asked.Turn)
	// Archive the asked session.
	if resp, out := do(t, e.req(t, "PATCH", "/api/v0/sessions/"+asked.Session, strings.NewReader(`{"archived":true}`))); resp.StatusCode != 200 || !strings.Contains(out, `"archived":true`) {
		t.Fatalf("archive the asked session: %d %s", resp.StatusCode, out)
	}
	// The asker's copy, its marker and the asked turn's changes all stay: archive
	// removed nothing.
	if b, err := os.ReadFile(filepath.Join(copyDir, "marker.txt")); err != nil || string(b) != "held" {
		t.Errorf("the asker's copy marker is %q (%v) after the ask was archived, want it intact", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(copyDir, "child.txt")); err != nil || len(b) == 0 {
		t.Errorf("the asked turn's file is gone (%v) after archive, want the shared copy kept", err)
	}
	var wsp store.Workspace
	if code := getJSON(t, e, "/api/v0/workspaces/"+ws, &wsp); code != 200 || wsp.HeadTurn != asked.Turn {
		t.Errorf("GET /workspaces/%s after archive: head_turn %q, want the asked turn %q kept", ws, wsp.HeadTurn, asked.Turn)
	}
}

// R6: an asked session opened via POST /api/v0/sessions names its asker (origin
// kind asked) in GET /api/v0/sessions before any turn starts. (Review P2.6.)
func TestAskedSessionShowsOriginBeforeTurn(t *testing.T) {
	e, dir := reviewEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)
	askerTurn := startTurn(t, e, `{"session":"`+sess+`","agent":"claude","prompt":"hold"}`)
	tok := tokenOf(t, dir, "hold")
	// The asker opens an asked session but starts no turn in it yet.
	code, out := as(t, e, tok, "POST", "/api/v0/sessions", `{}`)
	if code != 201 {
		t.Fatalf("open asked session: %d %s", code, out)
	}
	var sc SessionCreated
	if err := json.Unmarshal([]byte(out), &sc); err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if code := getJSON(t, e, "/api/v0/sessions", &snap); code != 200 {
		t.Fatalf("GET /sessions: %d", code)
	}
	var found *Session
	for i := range snap.Sessions {
		if snap.Sessions[i].ID == sc.Session {
			found = &snap.Sessions[i]
		}
	}
	if found == nil || found.Origin == nil || found.Origin.Kind != event.OriginAsked || found.Origin.Session != askerTurn.Session {
		t.Errorf("asked session before its turn: %+v, want origin asked by %s", found, askerTurn.Session)
	}
	release(dir, "hold")
}
