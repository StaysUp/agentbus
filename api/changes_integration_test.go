//go:build integration

package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

func changesOf(t *testing.T, e *env, session string) WorkspaceChangesPage {
	t.Helper()
	ws, found, err := e.store.WorkspaceOf(ids.SessionID(session))
	if err != nil || !found {
		t.Fatalf("workspace of %s: found=%v err=%v", session, found, err)
	}
	code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+string(ws)+"/changes", "")
	if code != 200 {
		t.Fatalf("changes of %s: %d %s", session, code, out)
	}
	var c WorkspaceChangesPage
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// snapshotOf is every file of dir with its bytes.
func snapshotOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return out
}

// The preview of a session's changes is the apply that follows it: the same
// files, each diff the change the apply makes, and it writes nothing. A
// conflict in the preview is the file the apply then refuses. A session
// fully applied reports nothing unapplied.
func TestSessionChangesPreview(t *testing.T) {
	e, _ := diffEnv(t, filepath.Join(t.TempDir(), settings.PolicyFile))
	folder := t.TempDir()
	for p, c := range map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n"} {
		if err := os.WriteFile(filepath.Join(folder, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first := e.firstTurn(t, folder, "claude", "edit1", true)
	waitEnded(t, e, first.Turn)

	before := snapshotOf(t, folder)
	c := changesOf(t, e, first.Session)
	if !reflect.DeepEqual(snapshotOf(t, folder), before) {
		t.Fatal("the preview wrote in the folder")
	}
	if !c.Isolated || !c.Unapplied || c.Turn != first.Turn || !slices.Equal(c.Turns, []string{first.Turn}) ||
		c.Count != 3 || len(c.Conflicts) != 0 {
		t.Fatalf("preview: %+v", c)
	}
	code, st := applyAs(t, e, token, c.Turn)
	if code != 200 {
		t.Fatalf("apply: %d %+v", code, st)
	}
	var paths []string
	for _, f := range c.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, st.Files) {
		t.Fatalf("preview files %v, the apply wrote %v", paths, st.Files)
	}
	after := snapshotOf(t, folder)
	bytesOf := func(m map[string]string, p string) []byte {
		if s, ok := m[p]; ok {
			return []byte(s)
		}
		return nil
	}
	for _, f := range c.Files {
		if f.Diff == nil {
			t.Fatalf("%s: no diff", f.Path)
		}
		if d := workspace.Unified(f.Path, bytesOf(before, f.Path), bytesOf(after, f.Path), f.Change); d != *f.Diff {
			t.Errorf("%s: preview %q, the apply's change %q", f.Path, *f.Diff, d)
		}
	}
	if c := changesOf(t, e, first.Session); c.Unapplied || len(c.Files) != 0 || c.Turn != first.Turn {
		t.Fatalf("preview of a session fully applied: %+v", c)
	}

	// The next turn changes a.txt, and so does the person.
	second := startTurn(t, e, `{"agent":"claude","session":"`+first.Session+`","prompt":"edit2"}`)
	waitEnded(t, e, second.Turn)
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("mine\n"), 0o644)
	before = snapshotOf(t, folder)
	c = changesOf(t, e, first.Session)
	if !reflect.DeepEqual(snapshotOf(t, folder), before) {
		t.Fatal("the preview wrote in the folder")
	}
	if !c.Unapplied || c.Turn != second.Turn || len(c.Conflicts) != 1 || c.Conflicts[0].Path != "a.txt" ||
		len(c.Files) != 1 || c.Files[0].Path != "a.txt" || c.Files[0].Diff == nil ||
		*c.Files[0].Diff != "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1,2 @@\n-mine\n+one\n+three\n" {
		t.Fatalf("preview over the person's edit: %+v", c)
	}
	code, st = applyAs(t, e, token, c.Turn)
	if code != 409 || len(st.Conflicts) != 1 || st.Conflicts[0].Path != c.Conflicts[0].Path || st.Conflicts[0].Why != c.Conflicts[0].Why {
		t.Fatalf("apply after the preview: %d %+v, preview %+v", code, st, c.Conflicts)
	}

	if code, out := as(t, e, token, "GET", "/api/v0/workspaces/ws_0000000000000000/changes", ""); code != 404 {
		t.Errorf("an unknown workspace: %d %s", code, out)
	}
	// Each preview was read, so none of their jobs is kept.
	if js, err := e.q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 100); err != nil || len(js) != 0 {
		t.Errorf("preview jobs kept: %d %v", len(js), err)
	}
}

// A session closed after its preview is cached does not serve the stale cache:
// a close writes no store event, so the route checks the queue and, finding
// the copy gone, re-runs and answers not isolated rather than reusing it.
func TestSessionChangesClosedSkipsCache(t *testing.T) {
	e, _ := diffEnv(t, filepath.Join(t.TempDir(), settings.PolicyFile))
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\ntwo\n"), 0o644)
	first := e.firstTurn(t, folder, "claude", "edit1", true)
	waitEnded(t, e, first.Turn)
	if c := changesOf(t, e, first.Session); !c.Isolated || !c.Unapplied || c.Count == 0 {
		t.Fatalf("preview before close: %+v", c)
	}
	ws, _, err := e.store.WorkspaceOf(ids.SessionID(first.Session))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.CloseSession(queue.DefaultTeam, ws); err != nil {
		t.Fatal(err)
	}
	if c := changesOf(t, e, first.Session); c.Isolated || c.Unapplied || c.Why != applySessionClosed || len(c.Files) != 0 {
		t.Fatalf("changes after close reused the cache: %+v", c)
	}
}

// A workspace id is not a session: the session routes (GET and PATCH
// /sessions/{id}) answer 404 for a workspace id and write nothing, so a
// workspace id cannot masquerade as a session and mint a phantom session in the
// record (his rule, 2026-11-22). The workspace's own routes (its /changes) take
// a workspace id and are the one way to reach it.
func TestWorkspaceIDIsNotASession(t *testing.T) {
	e, _ := diffEnv(t, filepath.Join(t.TempDir(), settings.PolicyFile))
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\ntwo\n"), 0o644)
	first := e.firstTurn(t, folder, "claude", "edit1", true)
	waitEnded(t, e, first.Turn)
	ws, found, err := e.store.WorkspaceOf(ids.SessionID(first.Session))
	if err != nil || !found {
		t.Fatalf("resolve workspace: found=%v err=%v", found, err)
	}
	if string(ws) == first.Session {
		t.Fatalf("workspace id equals the session id, so the test cannot tell them apart: %s", ws)
	}
	// The session routes read it as no session: 404, never a synthesized 200.
	if code, out := as(t, e, token, "GET", "/api/v0/sessions/"+string(ws), ""); code != 404 {
		t.Fatalf("GET /sessions/{workspace id}: %d %s, want 404", code, out)
	}
	// Its own changes route takes the workspace id and works -- and storing that
	// preview (keyed by the workspace id) must not add the workspace as a phantom
	// session to the session list.
	if code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+string(ws)+"/changes", ""); code != 200 {
		t.Fatalf("GET /workspaces/{workspace id}/changes: %d %s, want 200", code, out)
	}
	code, out := as(t, e, token, "GET", "/api/v0/sessions", "")
	var list struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if code != 200 || json.Unmarshal([]byte(out), &list) != nil {
		t.Fatalf("GET /sessions: %d %s", code, out)
	}
	for _, s := range list.Sessions {
		if s.ID == string(ws) {
			t.Fatalf("GET /sessions lists the workspace id as a phantom session: %s", out)
		}
	}
	// Archiving a workspace id as a session is 404 too, and writes no
	// session.archived -- SessionHasWork is session-scoped, so the workspace's
	// open jobs do not make its id pass as a session.
	if code, out := as(t, e, token, "PATCH", "/api/v0/sessions/"+string(ws), `{"archived":true}`); code != 404 {
		t.Fatalf("PATCH /sessions/{workspace id} archived: %d %s, want 404", code, out)
	}
	if known, err := e.store.Known(string(ws)); err != nil || known {
		t.Fatalf("the workspace id became a known session after archive: known=%v err=%v", known, err)
	}
}

// A preview whose session has a stored turn.applied -- here a refused apply,
// with the turn still unapplied -- is reused, not re-run on every GET. The
// apply is folded into the preview's base (the preview reflects the folder it
// refused against), so it does not read back as a change since. Round-3
// defect: base was the turn's change alone, below the later apply, so every
// GET re-ran, and answered 503 when the runner was offline instead of serving
// the stored preview. Reuse shows as the latest preview's seq not moving.
// Any apply retakes the preview, a refused one included: the apply might have
// changed the folder and the cursor counts it, so the next GET re-runs rather
// than serving the cached preview. (Re-running after a refusal also shows the
// person the folder edit that caused it.)
func TestSessionChangesRerunsAfterRefusedApply(t *testing.T) {
	e, _ := diffEnv(t, filepath.Join(t.TempDir(), settings.PolicyFile))
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\ntwo\n"), 0o644)
	first := e.firstTurn(t, folder, "claude", "edit1", true)
	waitEnded(t, e, first.Turn)
	// Cache a preview before any apply: its cursor has no applies yet. The
	// preview is keyed by the workspace it previews.
	ws, _, err := e.store.WorkspaceOf(ids.SessionID(first.Session))
	if err != nil {
		t.Fatal(err)
	}
	changesOf(t, e, first.Session)
	_, seq1, found, err := e.store.LatestPreview(string(ws))
	if err != nil || !found {
		t.Fatalf("no cached preview: %v", err)
	}
	// The person edits the file so the apply is refused; the refused apply
	// still counts, so the cursor moves off the one the preview recorded.
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("mine\n"), 0o644)
	if code, _ := applyAs(t, e, token, first.Turn); code != 409 {
		t.Fatalf("apply should be refused: %d", code)
	}
	changesOf(t, e, first.Session)
	if _, seq2, _, _ := e.store.LatestPreview(string(ws)); seq2 == seq1 {
		t.Fatalf("the preview was reused after a refused apply (seq %d) instead of re-run", seq1)
	}
}

// A session in the person's folder as it is has nothing to apply, and says
// so.
func TestSessionChangesNotIsolated(t *testing.T) {
	e, _ := diffEnv(t, "")
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\ntwo\n"), 0o644)
	// A direct workspace (sandbox false) works in the folder as it is.
	first := e.firstTurn(t, folder, "claude", "edit1", false)
	waitEnded(t, e, first.Turn)
	c := changesOf(t, e, first.Session)
	if c.Isolated || c.Unapplied || c.Why != changesNotIsolated || len(c.Files) != 0 {
		t.Fatalf("preview of a session in the folder: %+v", c)
	}
}

// A session whose first turn has not ended has nothing to apply yet, and is
// isolated as its workspace's sandbox says: a copy when sandbox is true, the
// folder as it is when false. The isolation setting only chooses the backend,
// so "none" does not turn a sandbox workspace's wall off (his rule, 2026-11-21).
func TestSessionChangesFirstTurnRunning(t *testing.T) {
	for _, tc := range []struct {
		name, policy      string
		sandbox, isolated bool
	}{
		{"a copy", "", true, true},
		{"the folder as it is", "", false, false},
		{"sandbox ignores isolation none", "isolation = \"" + settings.IsolationNone + "\"\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			perm := filepath.Join(t.TempDir(), settings.PolicyFile)
			if tc.policy != "" {
				if err := os.WriteFile(perm, []byte(tc.policy), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			e, dir := diffEnv(t, perm)
			folder := t.TempDir()
			os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\n"), 0o644)
			first := e.firstTurn(t, folder, "claude", "hold", tc.sandbox)
			tokenOf(t, dir, "hold") // the turn is running
			c := changesOf(t, e, first.Session)
			why := ""
			if !tc.isolated {
				why = changesNotIsolated
			}
			if c.Isolated != tc.isolated || c.Unapplied || c.Why != why || c.Turn != "" || len(c.Files) != 0 {
				t.Fatalf("preview while the first turn runs: %+v", c)
			}
			release(dir, "hold")
			waitEnded(t, e, first.Turn)
		})
	}
}

// changesQueue is an API over a queue the test drives itself, as the
// runner of machine con would: one isolated session whose first turn
// ended with changes. later moves the queue's clock on.
func changesQueue(t *testing.T, waits ...time.Duration) (e *env, q *queue.Queue, r queue.Runner, session, turn string, later func(time.Duration)) {
	t.Helper()
	wait := 5 * time.Second
	if len(waits) > 0 {
		wait = waits[0]
	}
	dir := t.TempDir()
	var skew atomic.Int64
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), func() time.Time { return time.Now().Add(time.Duration(skew.Load())) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port, Queue: q, TurnWait: wait})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(srv.Close)
	const con = "con_aaaaaaaaaaaaaaaa"
	r, err = q.RegisterRunner(queue.DefaultTeam, "r", nil, 4, queue.HashToken(queue.NewID("tok_")), &queue.Capabilities{Connector: con})
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", FolderRef: "fld_0123456789abcdef",
		Machine: con, Isolate: true, Prompt: "hi"}, queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	// What is appliable is the record's (store.Applied): the queue reads it
	// there, and the turn ends the way control's finisher ends it, its
	// turn.changed in the record first.
	q.ReadAppliedWith(st.Applied)
	if _, _, err := st.Append(event.Event{Kind: event.TurnStarted, Time: time.Now().UTC(), Session: j.Session, Turn: j.Turn,
		Text: "hi", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	tj, at, ok, _, err := q.Take(r.ID, 30*time.Second, queue.LaneInteractive)
	if err != nil || !ok {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := q.Start(r.ID, at.ID, at.Number, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.EndAs(st, q, r.ID, at.ID, at.Number, tj, queue.Finish{Status: queue.Finished, Session: j.Session,
		Workspace: &queue.Workspace{Kind: queue.WorkspaceCopy, Space: j.ID},
		Result:    []byte(`{"changeset":"` + strings.Repeat("1", 64) + `","changes":{"kind":"snapshot","from":"cp0","to":"cp1","files":[]}}`)}); err != nil {
		t.Fatal(err)
	}
	// The workspace changes route reads the workspace from the store, so record
	// one for this session the way the migration does: its id is its own
	// workspace id (migrated-style), sandbox true.
	sb := true
	if _, _, err := st.Append(event.Event{ID: "wc_" + j.Session, Kind: event.WorkspaceCreated, Time: time.Now().UTC(),
		Workspace: j.Session, FolderRef: "fld_0123456789abcdef", Machine: con, Sandbox: &sb}); err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, api: a, store: st, port: port, q: q}, q, r, j.Session, j.Turn, func(d time.Duration) { skew.Add(int64(d)) }
}

// endJob takes the next job on r (a folder job with folder) and ends it.
func endJob(t *testing.T, q *queue.Queue, r queue.Runner, folder bool, f queue.Finish) queue.Job {
	t.Helper()
	j, at, ok, _, err := q.Take(r.ID, 30*time.Second, laneOf(folder))
	if err != nil || !ok {
		t.Fatalf("take: %v %v", ok, err)
	}
	if _, _, err := q.Start(r.ID, at.ID, at.Number, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(r.ID, at.ID, at.Number, f); err != nil {
		t.Fatal(err)
	}
	return j
}

// The changes route's other answers: 409 while an apply of the session runs,
// 503 with the folder's machine offline, and a preview cut to fit, passed on
// as it came, its job dropped once read.
func TestSessionChangesAnswers(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		e, q, _, session, turn, _ := changesQueue(t)
		if _, _, err := q.Apply(queue.DefaultTeam, turn); err != nil {
			t.Fatal(err)
		}
		if code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes", ""); code != 409 || !strings.Contains(out, applyBusy) {
			t.Fatalf("%d %s", code, out)
		}
	})
	t.Run("offline", func(t *testing.T) {
		e, _, _, session, _, later := changesQueue(t)
		later(queue.RunnerFresh + time.Second)
		if code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes", ""); code != 503 || !strings.Contains(out, applyOffline) {
			t.Fatalf("%d %s", code, out)
		}
	})
	t.Run("cut", func(t *testing.T) {
		e, q, r, session, turn, _ := changesQueue(t)
		type answer struct {
			code int
			out  string
		}
		got := make(chan answer, 1)
		go func() {
			code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes", "")
			got <- answer{code, out}
		}()
		p := queue.PreviewResult{Files: []queue.PreviewFile{{Path: "a.txt", Change: "modified", DiffLeftOut: true}},
			Count: 40, Truncated: true, DiffsCut: true, Conflicts: []queue.ApplyConflict{{Path: "a.txt", Why: "changed"}}}
		res, _ := json.Marshal(queue.Result{Preview: &p})
		var preview queue.Job
		for deadline := time.Now().Add(5 * time.Second); preview.ID == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if js, _ := q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 1); len(js) == 1 {
				preview = endJob(t, q, r, true, queue.Finish{Status: queue.Finished, Result: res})
			}
		}
		a := <-got
		var c WorkspaceChangesPage
		if a.code != 200 || json.Unmarshal([]byte(a.out), &c) != nil {
			t.Fatalf("%d %s", a.code, a.out)
		}
		if !c.Isolated || !c.Unapplied || c.Turn != turn || c.Count != 40 || !c.Truncated || !c.DiffsCut ||
			len(c.Files) != 1 || !c.Files[0].DiffLeftOut || len(c.Conflicts) != 1 {
			t.Fatalf("a cut preview: %+v", c)
		}
		if _, err := q.Get(queue.DefaultTeam, preview.ID); !errors.Is(err, queue.ErrNotFound) {
			t.Fatalf("the read preview's job: %v", err)
		}
	})
}

// When a re-run cannot run because the runner is offline, the last stored
// preview is served instead of a 503. Here the session has not changed, so the
// preview is still current and is not marked stale.
func TestSessionChangesServesCachedWhenOffline(t *testing.T) {
	e, q, r, session, _, later := changesQueue(t)
	// Cache a preview by running the one preview job the first GET plans.
	got := make(chan int, 1)
	go func() {
		code, _ := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes", "")
		got <- code
	}()
	p := queue.PreviewResult{Files: []queue.PreviewFile{{Path: "a.txt", Change: "modified"}}, Count: 1}
	res, _ := json.Marshal(queue.Result{Preview: &p})
	cached := false
	for deadline := time.Now().Add(5 * time.Second); !cached && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if js, _ := q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 1); len(js) == 1 {
			endJob(t, q, r, true, queue.Finish{Status: queue.Finished, Result: res})
			cached = true
		}
	}
	if !cached {
		t.Fatal("the preview job was never queued")
	}
	if code := <-got; code != 200 {
		t.Fatalf("the caching GET: %d", code)
	}
	// The runner goes offline; ?fresh forces a re-run that cannot run, so the
	// stored preview is served marked stale.
	later(queue.RunnerFresh + time.Second)
	code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes?fresh=true", "")
	var c WorkspaceChangesPage
	if code != 200 || json.Unmarshal([]byte(out), &c) != nil {
		t.Fatalf("stale GET: %d %s", code, out)
	}
	// The cursor has not moved, so the stored preview is still current: it is
	// served (not a 503), and not marked stale.
	if c.Stale || c.Why != "" || c.Count != 1 || c.PreviewedAt == nil || len(c.Files) != 1 {
		t.Fatalf("expected the current preview served, not stale: %+v", c)
	}
}

// A pending apply still answers 409, even when a preview is cached: reuse must
// not serve a 200 over an apply in progress. The apply moves the cursor, so
// the cached preview is not reused and the re-run reports the apply is busy.
func TestSessionChangesBusyNotReused(t *testing.T) {
	e, q, r, session, turn, _ := changesQueue(t)
	// Cache a preview by running the one preview job the first GET plans.
	got := make(chan int, 1)
	go func() {
		code, _ := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes", "")
		got <- code
	}()
	p := queue.PreviewResult{Files: []queue.PreviewFile{{Path: "a.txt", Change: "modified"}}, Count: 1}
	res, _ := json.Marshal(queue.Result{Preview: &p})
	cached := false
	for deadline := time.Now().Add(5 * time.Second); !cached && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if js, _ := q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 1); len(js) == 1 {
			endJob(t, q, r, true, queue.Finish{Status: queue.Finished, Result: res})
			cached = true
		}
	}
	if !cached {
		t.Fatal("the preview job was never queued")
	}
	if code := <-got; code != 200 {
		t.Fatalf("the caching GET: %d", code)
	}
	// Start an apply; it is pending, so a fresh GET must report busy, not the
	// cached preview.
	if _, _, err := q.Apply(queue.DefaultTeam, turn); err != nil {
		t.Fatal(err)
	}
	if code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes", ""); code != 409 || !strings.Contains(out, applyBusy) {
		t.Fatalf("a pending apply with a cached preview: %d %s", code, out)
	}
}

// A pinned page reports the same stale status as the first: once the session
// moves off a preview (here an apply), paging it by its event id still says
// stale, not only the first page. (Round-7 P1.2: the pinned path dropped it.)
func TestSessionChangesPinnedPageReportsStale(t *testing.T) {
	e, _ := diffEnv(t, filepath.Join(t.TempDir(), settings.PolicyFile))
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\ntwo\n"), 0o644)
	first := e.firstTurn(t, folder, "claude", "edit1", true)
	waitEnded(t, e, first.Turn)
	c := changesOf(t, e, first.Session)
	if c.Event == "" {
		t.Fatalf("no cached preview event: %+v", c)
	}
	// An apply moves the cursor off the cached preview.
	if code, st := applyAs(t, e, token, first.Turn); code != 200 {
		t.Fatalf("apply: %d %+v", code, st)
	}
	// Paging that now out-of-date preview by its event id reports it stale.
	ws, _, _ := e.store.WorkspaceOf(ids.SessionID(first.Session))
	code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+string(ws)+"/changes?event="+c.Event, "")
	var p WorkspaceChangesPage
	if code != 200 || json.Unmarshal([]byte(out), &p) != nil {
		t.Fatalf("pinned GET: %d %s", code, out)
	}
	if !p.Stale || p.Why != changesStale || p.Event != c.Event {
		t.Fatalf("a pinned out-of-date preview should be stale: %+v", p)
	}
}

// Two GETs of a session's changes that arrive together run one preview, not
// two racing to store: the first holds the session's gate and runs it, the
// second waits and reuses its result. Both return the same preview.
func TestSessionChangesConcurrentGetsRunOnePreview(t *testing.T) {
	e, q, r, session, _, _ := changesQueue(t)
	type ans struct {
		code int
		out  string
	}
	got := make(chan ans, 2)
	for i := 0; i < 2; i++ {
		go func() {
			code, out := getRaw(e, token, "/api/v0/workspaces/"+session+"/changes")
			got <- ans{code, out}
		}()
	}
	// Exactly one preview job is planned while both GETs are in flight; run it.
	p := queue.PreviewResult{Files: []queue.PreviewFile{{Path: "a.txt", Change: "modified"}}, Count: 1}
	res, _ := json.Marshal(queue.Result{Preview: &p})
	ran := false
	for deadline := time.Now().Add(5 * time.Second); !ran && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		js, _ := q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 2)
		if len(js) > 1 {
			t.Fatalf("two preview jobs ran for concurrent GETs: %d", len(js))
		}
		if len(js) == 1 {
			endJob(t, q, r, true, queue.Finish{Status: queue.Finished, Result: res})
			ran = true
		}
	}
	if !ran {
		t.Fatal("no preview job appeared")
	}
	for i := 0; i < 2; i++ {
		a := <-got
		var c WorkspaceChangesPage
		if a.code != 200 || json.Unmarshal([]byte(a.out), &c) != nil || c.Count != 1 || len(c.Files) != 1 {
			t.Fatalf("concurrent GET %d: %d %s", i, a.code, a.out)
		}
	}
	// No second preview ran after the first was read.
	if js, _ := q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 2); len(js) != 0 {
		t.Fatalf("a preview job was left or a second ran: %d", len(js))
	}
}

// A preview whose session changes while it runs is served stale: the cursor is
// rechecked after the run, so a turn or apply that landed during it is not
// shown as current. (Round-8: the final serve hardcoded not-stale.)
func TestSessionChangesFreshRunStaleIfChangedWhileRunning(t *testing.T) {
	e, q, r, session, turn, _ := changesQueue(t)
	got := make(chan string, 1)
	go func() {
		_, out := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes?fresh=true", "")
		got <- out
	}()
	// While the preview runs, an apply moves the cursor off what it recorded.
	ran := false
	for deadline := time.Now().Add(5 * time.Second); !ran && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if js, _ := q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 1); len(js) == 1 {
			if _, _, err := q.Apply(queue.DefaultTeam, turn); err != nil {
				t.Fatal(err)
			}
			p := queue.PreviewResult{Files: []queue.PreviewFile{{Path: "a.txt", Change: "modified"}}, Count: 1}
			res, _ := json.Marshal(queue.Result{Preview: &p})
			endJob(t, q, r, true, queue.Finish{Status: queue.Finished, Result: res})
			ran = true
		}
	}
	if !ran {
		t.Fatal("no preview job appeared")
	}
	var c WorkspaceChangesPage
	if json.Unmarshal([]byte(<-got), &c) != nil {
		t.Fatal("decode")
	}
	if !c.Stale || c.Why != changesStale {
		t.Fatalf("a preview whose session changed while it ran should be stale: %+v", c)
	}
}

// Waiting for the preview gate is bounded by one budget with the run, not a
// fresh wait each: a second GET held behind a stuck preview answers the
// timeout rather than hanging. (Round-8: the gate wait had no deadline.)
func TestSessionChangesGateWaitBounded(t *testing.T) {
	e, q, _, session, _, _ := changesQueue(t, 300*time.Millisecond)
	// The first GET holds the gate on a preview that never finishes.
	go getRaw(e, token, "/api/v0/workspaces/"+session+"/changes?fresh=true")
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if js, _ := q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 1); len(js) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first preview never started")
		}
	}
	// The second GET waits for the gate and, within the budget, answers 504.
	start := time.Now()
	code, out := as(t, e, token, "GET", "/api/v0/workspaces/"+session+"/changes?fresh=true", "")
	if code != 504 || !strings.Contains(out, changesSlow) {
		t.Fatalf("a bounded gate wait: %d %s", code, out)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("the gate wait was not bounded: %s", waited)
	}
}
