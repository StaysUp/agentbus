//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
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
		`case "$p" in "[AgentBus]"*) printf '%s' "$p" > "$D/delivered-$$"; exit 0;; quick|"Review a change"*) exit 0;; esac; ` +
		`env > "$D/env-$p.tmp"; pwd > "$D/pwd-$p"; mv "$D/env-$p.tmp" "$D/env-$p"; ` +
		`i=0; while [ ! -e "$D/release-$p" ] && [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"},
			Env: []string{"PATH=/usr/bin:/bin", "D=" + dir, "ANTHROPIC_BASE_URL=" + baseURL}}, nil
	}
	return stackEnv(t, stack{wire: wire, maxRuns: 16, permissions: perm}), dir
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
	b, err := os.ReadFile(filepath.Join(dir, "pwd-"+prompt))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
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
func deliverUntil(t *testing.T, e *env, session string, want int) []store.Turn {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := e.q.Deliver(queue.DefaultTeam, nil); err != nil {
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

func ask(t *testing.T, e *env, tok, body string) Started {
	t.Helper()
	code, out := as(t, e, tok, "POST", "/api/v0/turns", body)
	if code != 202 {
		t.Fatalf("ask %s: %d %s", body, code, out)
	}
	var s Started
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A turn asks two agents with its token, in the person's folder: each runs in
// a worktree of its own, says who asked it, and its result comes back to the
// asking session as a later turn of its own, naming its worktree. A review it
// asks for comes back too. The asked turns read the tree, never write to
// their asker's session, and cancel only asked work.
func TestAskFanOutAndDeliver(t *testing.T) {
	e, dir := askEnv(t)
	repo := gitRepo(t)
	p := startTurn(t, e, `{"agent":"claude","folder":"`+repo+`","prompt":"parent"}`)
	ptok := tokenOf(t, dir, "parent")

	c1 := ask(t, e, ptok, `{"agent":"claude","prompt":"one"}`)
	c2 := ask(t, e, ptok, `{"agent":"codex","prompt":"two"}`)
	ctok := tokenOf(t, dir, "one")
	tokenOf(t, dir, "two")
	w0, w1, w2 := pwdOf(t, dir, "parent"), pwdOf(t, dir, "one"), pwdOf(t, dir, "two")
	if w0 == w1 || w1 == w2 || w0 == w2 || w1 == repo || w2 == repo {
		t.Fatalf("folders not each their own: parent %s, one %s, two %s, repo %s", w0, w1, w2, repo)
	}
	// The asked turns say who asked.
	for _, c := range []Started{c1, c2} {
		list, err := e.store.Turns(store.TurnQuery{Session: c.ID, Limit: 5})
		if err != nil || len(list.Turns) != 1 {
			t.Fatalf("child %s: %v %+v", c.ID, err, list)
		}
		if o := list.Turns[0].Origin; o == nil || o.Kind != event.OriginAsked || o.Session != p.ID || o.Turn != p.Turn {
			t.Errorf("child %s origin %+v", c.ID, o)
		}
	}
	// Reads across the tree work both ways; nothing outside it.
	for _, tc := range []struct {
		tok, method, path, body string
		want                    int
	}{
		{ptok, "GET", "/api/v0/sessions/" + c1.ID + "/events", "", 200},
		{ptok, "GET", "/api/v0/turns?session=" + c2.ID, "", 200},
		{ptok, "GET", "/api/v0/jobs/" + c1.Job, "", 200},
		{ptok, "GET", "/api/v0/jobs?session=" + c1.ID, "", 200},
		{ctok, "GET", "/api/v0/sessions/" + p.ID + "/events", "", 200},
		{ctok, "GET", "/api/v0/sessions/" + c2.ID + "/events", "", 200},
		// The asked turn never writes to its asker's session, nor its own.
		{ctok, "POST", "/api/v0/turns", `{"session":"` + p.ID + `","prompt":"x"}`, 403},
		{ctok, "POST", "/api/v0/turns", `{"session":"` + c1.ID + `","prompt":"x"}`, 403},
		// It may not cancel or stop the person's work, nor its own.
		{ctok, "POST", "/api/v0/jobs/" + p.Job + "/cancel", "", 403},
		{ctok, "POST", "/api/v0/turns/" + p.Turn + "/stop", "", 403},
		{ptok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","isolate":false}`, 403},
		{ptok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","folder":"` + filepath.Dir(repo) + `"}`, 403},
	} {
		if code, out := as(t, e, tc.tok, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s %s %s: %d %s, want %d", tc.tok[:8], tc.method, tc.path, code, out, tc.want)
		}
	}

	release(dir, "parent")
	release(dir, "one")
	release(dir, "two")
	got := deliverUntil(t, e, p.ID, 2)
	byJob := map[string]store.Turn{}
	for _, tn := range got {
		byJob[tn.Origin.Job] = tn
	}
	for c, w := range map[Started]string{c1: w1, c2: w2} {
		tn, ok := byJob[c.Job]
		if !ok {
			t.Fatalf("no result of %s delivered; got %+v", c.Job, got)
		}
		if tn.Origin.Session != c.ID || tn.Origin.Turn != c.Turn || tn.Origin.JobKind != "turn" ||
			!strings.HasPrefix(tn.Prompt, "[AgentBus]") || !strings.Contains(tn.Prompt, "worktrees/"+c.Job+"\n") ||
			!strings.HasSuffix(w, "worktrees/"+c.Job) {
			t.Errorf("result of %s: %+v\n%s", c.Job, tn.Origin, tn.Prompt)
		}
		// On the parent's own agent.
		if tn.Harness != "claude" {
			t.Errorf("result of %s ran on %s", c.Job, tn.Harness)
		}
	}

	// The parent's token is gone; the result turn's own token asks for a
	// review of a child's work, and that comes back too.
	j1, err := e.q.Get(queue.DefaultTeam, c1.Job)
	if err != nil || j1.Ended == nil {
		t.Fatalf("child job: %v %+v", err, j1)
	}
	p2 := startTurn(t, e, `{"session":"`+p.ID+`","prompt":"again"}`)
	atok := tokenOf(t, dir, "again")
	code, out := as(t, e, atok, "POST", "/api/v0/jobs/"+c1.Job+"/review", `{"agent":"codex"}`)
	if code != 201 {
		t.Fatalf("review: %d %s", code, out)
	}
	var rj queue.Job
	_ = json.Unmarshal([]byte(out), &rj)
	if rj.Links.Asker == nil || rj.Links.Asker.Turn != p2.Turn || rj.Links.Chain == nil || rj.Links.Chain.Root != p.ID {
		t.Errorf("review links: %+v", rj.Links)
	}
	// A cancel of asked work in the tree is allowed.
	c3 := ask(t, e, atok, `{"agent":"claude","prompt":"three"}`)
	if code, out := as(t, e, atok, "POST", "/api/v0/jobs/"+c3.Job+"/cancel", ""); code != 202 {
		t.Errorf("cancel asked work: %d %s", code, out)
	}
	release(dir, "again")
	release(dir, "three")
	got = deliverUntil(t, e, p.ID, 4)
	review := false
	for _, tn := range got {
		if tn.Origin.Job == rj.ID {
			review = tn.Origin.JobKind == "review" && strings.Contains(tn.Prompt, "the review you asked for") && strings.Contains(tn.Prompt, "Verdict:")
		}
	}
	if !review {
		t.Errorf("no review result delivered: %+v", got)
	}
}

// A chain of asks stops at the depth cap, a read-only turn's token cannot
// ask, and one turn's asks stop at the per-turn cap. Each refusal says why.
func TestAskCaps(t *testing.T) {
	e, dir := askEnv(t)
	folder := t.TempDir()
	startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"root"}`)
	tok := tokenOf(t, dir, "root")
	ask(t, e, tok, `{"agent":"claude","prompt":"d1"}`)
	ask(t, e, tokenOf(t, dir, "d1"), `{"agent":"claude","prompt":"d2"}`)
	code, out := as(t, e, tokenOf(t, dir, "d2"), "POST", "/api/v0/turns", `{"agent":"claude","prompt":"d3"}`)
	if code != 403 || !strings.Contains(out, "levels") || !strings.Contains(out, "[limits]") {
		t.Errorf("depth 3: %d %s", code, out)
	}
	for i := 1; i < run.DefaultMaxJobsPerTurn; i++ {
		ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)
	}
	code, out = as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, fmt.Sprint(run.DefaultMaxJobsPerTurn)) {
		t.Errorf("past the per-turn cap: %d %s", code, out)
	}

	startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"ro","read_only":true}`)
	code, out = as(t, e, tokenOf(t, dir, "ro"), "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x"}`)
	if code != 403 || !strings.Contains(out, "read-only") {
		t.Errorf("read-only turn asking: %d %s", code, out)
	}
	for _, p := range []string{"root", "d1", "d2", "ro"} {
		release(dir, p)
	}
}

// permissions.toml decides what a turn token may do, read for each request:
// a cap changed in the file applies to the next request, each [agent_api]
// switch refuses its kind of call and says which, scope "session" keeps the
// token in its own session, a cost cap stops asking once the tree has spent
// it, and a broken file refuses every turn token's request. No setting opens
// the routes that change agents or tokens.
func TestAskPolicyFromPermissions(t *testing.T) {
	perm := filepath.Join(t.TempDir(), run.PermissionsFile)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(perm, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[limits]\nmax_jobs_per_turn = 2\n")
	e, dir := askEnvWith(t, perm)
	folder := t.TempDir()
	root := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"root"}`)
	tok := tokenOf(t, dir, "root")
	ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)
	ask(t, e, tok, `{"agent":"claude","prompt":"child"}`)
	code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, "at most 2 jobs") || !strings.Contains(out, "max_jobs_per_turn") {
		t.Fatalf("past max_jobs_per_turn = 2: %d %s", code, out)
	}
	// The file is read again for the next request.
	write("[limits]\nmax_jobs_per_turn = 3\n")
	ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)

	child := tokenOf(t, dir, "child")
	for _, tc := range []struct {
		name, file, method, path, body, want string
		tok                                  string
	}{
		{"start off", "[agent_api]\nstart = false\n", "POST", "/api/v0/turns", `{"agent":"claude","prompt":"quick"}`, "start is false", tok},
		{"read off", "[agent_api]\nread = false\n", "GET", "/api/v0/sessions/" + root.ID + "/search?q=x", "", "read is false", tok},
		{"list_agents off", "[agent_api]\nlist_agents = false\n", "GET", "/api/v0/harnesses", "", "list_agents is false", tok},
		{"cancel off", "[agent_api]\ncancel = false\n", "POST", "/api/v0/jobs/job_x/cancel", "", "cancel is false", tok},
		{"review off", "[agent_api]\nreview = false\n", "POST", "/api/v0/jobs/job_x/review", `{}`, "review is false", tok},
		// scope = "session": the child's token reads its own session only,
		// not the person's.
		{"scope session", "[agent_api]\nscope = \"session\"\n", "GET", "/api/v0/sessions/" + root.ID + "/search?q=x", "", "only its own session", child},
		{"scope session, by query", "[agent_api]\nscope = \"session\"\n", "GET", "/api/v0/turns?session=" + root.ID, "", "only its own session", child},
		{"scope session, events by query", "[agent_api]\nscope = \"session\"\n", "GET", "/api/v0/turns/" + root.Turn + "/events?session=" + root.ID, "", "only its own session", child},
		{"broken file", "[agent_api]\nscope = all\n", "GET", "/api/v0/harnesses", "", "cannot be used", tok},
	} {
		write(tc.file)
		code, out := as(t, e, tc.tok, tc.method, tc.path, tc.body)
		if (code != 403 && code != 503) || !strings.Contains(out, tc.want) {
			t.Errorf("%s: %d %s, want a refusal with %q", tc.name, code, out, tc.want)
		}
	}
	// Every switch on, the routes that change agents stay closed to a turn
	// token: they take the main token, always.
	write("")
	for _, r := range [][3]string{
		{"POST", "/api/v0/agents", `{"name":"x","harness":"claude","access":"subscription"}`},
		{"PUT", "/api/v0/agents/a_x", `{"name":"x","harness":"claude","access":"subscription"}`},
		{"DELETE", "/api/v0/agents/a_x", ""},
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
		Session: root.ID, Turn: root.Turn, Exchange: "x-cost", Status: 200, CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	write("[limits]\nmax_cost_usd_per_tree = 0.25\n")
	code, out = as(t, e, child, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, "max_cost_usd_per_tree") || !strings.Contains(out, "$0.30") {
		t.Errorf("past the cost cap: %d %s", code, out)
	}
	if again := ask(t, e, child, keyed); !again.Existing || again.Job != first.Job {
		t.Errorf("a retry past the cost cap: %+v, want job %s again", again, first.Job)
	}
	write("[limits]\nmax_cost_usd_per_tree = 0\n")
	ask(t, e, child, `{"agent":"claude","prompt":"quick"}`)
	for _, p := range []string{"root", "child"} {
		release(dir, p)
	}
}
