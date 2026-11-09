//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// diffEnv starts an API whose turns run a shell that edits its folder by
// prompt: edit1 and edit2 change files, hold writes its environment to
// <dir>/env-hold and waits for <dir>/release-hold. Every turn prints a
// resume id, so the session continues.
func diffEnv(t *testing.T, perm string) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	script := `for a; do p="$a"; done; echo '{"type":"result","session_id":"sid-'$$'"}'; ` +
		`case "$p" in ` +
		`edit1) printf 'one\nTWO\n' > a.txt; echo new > b.txt; rm -f gone.txt;; ` +
		`edit2) printf 'one\nthree\n' > a.txt;; ` +
		`hold) env > "$D/env-hold.tmp"; mv "$D/env-hold.tmp" "$D/env-hold"; ` +
		`i=0; while [ ! -e "$D/release-hold" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done;; ` +
		`esac`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	return stackEnv(t, stack{wire: wire, permissions: perm}), dir
}

// waitEnded waits for the job to end.
func waitEnded(t *testing.T, e *env, job string) queue.Job {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		j, err := e.q.Get(queue.DefaultTeam, job)
		if err != nil {
			t.Fatal(err)
		}
		if j.Ended != nil {
			return j
		}
	}
	t.Fatalf("job %s did not end", job)
	return queue.Job{}
}

func applyAs(t *testing.T, e *env, tok, turn string) (int, ApplyState) {
	t.Helper()
	code, out := as(t, e, tok, "POST", "/api/v0/turns/"+turn+"/apply", "")
	var st ApplyState
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("apply %s: %d %s", turn, code, out)
	}
	return code, st
}

func diffOf(t *testing.T, e *env, turn string) TurnDiff {
	t.Helper()
	code, out := as(t, e, token, "GET", "/api/v0/turns/"+turn+"/diff", "")
	if code != 200 {
		t.Fatalf("diff %s: %d %s", turn, code, out)
	}
	var d TurnDiff
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// A turn in an isolated copy has a diff with each file's own unified diff,
// and applying it writes the files into the person's folder. The same apply
// again is a no-op. A file the person changed since refuses the next apply
// with 409, naming it, and writes nothing; once it is resolved the apply
// goes through, here from another turn's own token.
func TestTurnDiffAndApply(t *testing.T) {
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	e, dir := diffEnv(t, perm)
	folder := t.TempDir()
	fixture := map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n"}
	for p, c := range fixture {
		if err := os.WriteFile(filepath.Join(folder, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(folder, p))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}

	first := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"edit1"}`)
	waitEnded(t, e, first.Job)
	if read("a.txt") != "one\ntwo\n" || read("b.txt") != "<missing>" {
		t.Fatal("the turn's edits reached the person's folder before an apply")
	}
	d := diffOf(t, e, first.Turn)
	if d.Files == nil || !d.Files.Isolated || d.Files.From != "base" || d.Files.To != first.Turn || d.Files.Count != 3 {
		t.Fatalf("files: %+v", d.Files)
	}
	diffs := map[string]string{}
	for _, f := range d.Files.Changes {
		if f.Diff != nil {
			diffs[f.Path] = f.Change + "\n" + *f.Diff
		}
	}
	want := map[string]string{
		"a.txt":    "modified\n--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n",
		"b.txt":    "added\n--- /dev/null\n+++ b/b.txt\n@@ -0,0 +1 @@\n+new\n",
		"gone.txt": "deleted\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n",
	}
	for p, w := range want {
		if diffs[p] != w {
			t.Errorf("%s: %q, want %q", p, diffs[p], w)
		}
	}
	if d.Agent != "claude" || d.State != "finished" || d.DurationMS == nil || d.Job != first.Job || d.Apply.State != "none" {
		t.Errorf("diff header: agent %q state %q duration %v job %q apply %+v", d.Agent, d.State, d.DurationMS, d.Job, d.Apply)
	}

	code, st := applyAs(t, e, token, first.Turn)
	if code != 200 || st.State != "applied" || st.Already || !slices.Equal(st.Files, []string{"a.txt", "b.txt", "gone.txt"}) {
		t.Fatalf("apply: %d %+v", code, st)
	}
	if read("a.txt") != "one\nTWO\n" || read("b.txt") != "new\n" || read("gone.txt") != "<missing>" {
		t.Fatalf("after the apply: a %q b %q gone %q", read("a.txt"), read("b.txt"), read("gone.txt"))
	}
	again, st2 := applyAs(t, e, token, first.Turn)
	if again != 200 || st2.State != "applied" || !st2.Already || st2.Job != st.Job {
		t.Fatalf("second apply: %d %+v", again, st2)
	}
	if d := diffOf(t, e, first.Turn); d.Apply.State != "applied" || d.Apply.Job != st.Job {
		t.Errorf("diff after the apply: %+v", d.Apply)
	}

	// The next turn changes a.txt, and so does the person.
	second := startTurn(t, e, `{"session":"`+first.ID+`","prompt":"edit2"}`)
	waitEnded(t, e, second.Job)
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("mine\n"), 0o644)
	code, st = applyAs(t, e, token, second.Turn)
	if code != 409 || st.State != "refused" || len(st.Conflicts) != 1 || st.Conflicts[0].Path != "a.txt" ||
		st.Conflicts[0].Why != "changed in your folder since the checkpoint" || !strings.Contains(st.Error, "a.txt") {
		t.Fatalf("apply over the person's edit: %d %+v", code, st)
	}
	if read("a.txt") != "mine\n" {
		t.Fatalf("a refused apply wrote a.txt: %q", read("a.txt"))
	}

	// Resolved, and applied by a turn of the session with its own token.
	// With apply off in [agent_api], the token is refused first.
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("one\nTWO\n"), 0o644)
	third := startTurn(t, e, `{"session":"`+first.ID+`","prompt":"hold"}`)
	tok := tokenOf(t, dir, "hold")
	os.WriteFile(perm, []byte("[agent_api]\napply = false\n"), 0o600)
	if code, out := as(t, e, tok, "POST", "/api/v0/turns/"+second.Turn+"/apply", ""); code != 403 || !strings.Contains(out, "apply is false") {
		t.Fatalf("apply switched off: %d %s", code, out)
	}
	os.WriteFile(perm, nil, 0o600)
	code, st = applyAs(t, e, tok, second.Turn)
	if code != 200 || st.State != "applied" || !slices.Equal(st.Files, []string{"a.txt"}) || read("a.txt") != "one\nthree\n" {
		t.Fatalf("apply by a turn token: %d %+v, a.txt %q", code, st, read("a.txt"))
	}
	// A running turn cannot be applied.
	if code, out := as(t, e, token, "POST", "/api/v0/turns/"+third.Turn+"/apply", ""); code != 400 {
		t.Errorf("apply of a running turn: %d %s", code, out)
	}
	release(dir, "hold")
	waitEnded(t, e, third.Job)
	if code, out := as(t, e, token, "POST", "/api/v0/turns/t_0000000000000000/apply", ""); code != 404 {
		t.Errorf("apply of an unknown turn: %d %s", code, out)
	}
}
