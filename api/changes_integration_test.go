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

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
	"github.com/staysup/agentbus/workspace"
)

func changesOf(t *testing.T, e *env, session string) SessionChanges {
	t.Helper()
	code, out := as(t, e, token, "GET", "/api/v0/sessions/"+session+"/changes", "")
	if code != 200 {
		t.Fatalf("changes of %s: %d %s", session, code, out)
	}
	var c SessionChanges
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
	first := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"edit1"}`)
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
	if code != 409 || len(st.Conflicts) != 1 || st.Conflicts[0] != c.Conflicts[0] {
		t.Fatalf("apply after the preview: %d %+v, preview %+v", code, st, c.Conflicts)
	}

	if code, out := as(t, e, token, "GET", "/api/v0/sessions/s_0000000000000000/changes", ""); code != 404 {
		t.Errorf("an unknown session: %d %s", code, out)
	}
	// Each preview was read, so none of their jobs is kept.
	if js, err := e.q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindPreview}, 100); err != nil || len(js) != 0 {
		t.Errorf("preview jobs kept: %d %v", len(js), err)
	}
}

// A session in the person's folder as it is has nothing to apply, and says
// so.
func TestSessionChangesNotIsolated(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("isolation = \""+settings.IsolationNone+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := diffEnv(t, perm)
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\ntwo\n"), 0o644)
	first := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"edit1"}`)
	waitEnded(t, e, first.Turn)
	c := changesOf(t, e, first.Session)
	if c.Isolated || c.Unapplied || c.Why != changesNotIsolated || len(c.Files) != 0 {
		t.Fatalf("preview of a session in the folder: %+v", c)
	}
}

// A session whose first turn has not ended has nothing to apply yet, and is
// isolated as that turn asked: a copy, unless the isolation setting is none
// or the turn asked for the folder as it is (isolate false).
func TestSessionChangesFirstTurnRunning(t *testing.T) {
	for _, tc := range []struct {
		name, policy, isolate string
		isolated              bool
	}{
		{"a copy", "", "", true},
		{"isolation none", "isolation = \"" + settings.IsolationNone + "\"\n", "", false},
		{"isolate false", "", `"isolate":false,`, false},
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
			first := startTurn(t, e, `{"agent":"claude",`+tc.isolate+`"folder":"`+folder+`","prompt":"hold"}`)
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
func changesQueue(t *testing.T) (e *env, q *queue.Queue, r queue.Runner, session, turn string, later func(time.Duration)) {
	t.Helper()
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
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port, Queue: q, TurnWait: 5 * time.Second})
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
		Connector: con, Isolate: true, Prompt: "hi"}, queue.Links{})
	if err != nil {
		t.Fatal(err)
	}
	endJob(t, q, r, false, queue.Finish{Status: queue.Finished, Session: j.Session,
		Workspace: &queue.Workspace{Kind: queue.WorkspaceCopy, Space: j.ID}, Result: []byte(`{"changeset":"` + strings.Repeat("1", 64) + `"}`)})
	return &env{srv: srv, api: a, store: st, port: port, q: q}, q, r, j.Session, j.Turn, func(d time.Duration) { skew.Add(int64(d)) }
}

// endJob takes the next job on r (a folder job with folder) and ends it.
func endJob(t *testing.T, q *queue.Queue, r queue.Runner, folder bool, f queue.Finish) queue.Job {
	t.Helper()
	j, at, ok, _, err := q.Take(r.ID, 30*time.Second, folder)
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
		if code, out := as(t, e, token, "GET", "/api/v0/sessions/"+session+"/changes", ""); code != 409 || !strings.Contains(out, applyBusy) {
			t.Fatalf("%d %s", code, out)
		}
	})
	t.Run("offline", func(t *testing.T) {
		e, _, _, session, _, later := changesQueue(t)
		later(queue.RunnerFresh + time.Second)
		if code, out := as(t, e, token, "GET", "/api/v0/sessions/"+session+"/changes", ""); code != 503 || !strings.Contains(out, applyOffline) {
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
			code, out := as(t, e, token, "GET", "/api/v0/sessions/"+session+"/changes", "")
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
		var c SessionChanges
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
