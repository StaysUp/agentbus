//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"github.com/staysup/agentbus/folders"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
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
		`case "$p" in "[AgentBus]"*) printf '%s' "$p" > "$D/delivered-$$"; exit 0;; quick|"Review a change"*) exit 0;; esac; ` +
		`printf '%s\n' "$@" > "$D/args-$p"; env > "$D/env-$p.tmp"; pwd > "$D/pwd-$p"; mv "$D/env-$p.tmp" "$D/env-$p"; ` +
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
		if _, err := e.q.Deliver(queue.DefaultTeam, nil, nil); err != nil {
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
		// The asked turn never writes to its asker's session, nor its own.
		{ctok, "POST", "/api/v0/turns", `{"agent":"claude","session":"` + p.Session + `","prompt":"x"}`, 403},
		{ctok, "POST", "/api/v0/turns", `{"agent":"claude","session":"` + c1.Session + `","prompt":"x"}`, 403},
		// It may not stop the person's work.
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
		// Each copy is named after its session.
		if tn.Origin.Session != c.Session || tn.Origin.TurnKind != "turn" ||
			!strings.HasPrefix(tn.Prompt, "[AgentBus]") || !strings.Contains(tn.Prompt, "Turn: "+c.Turn+", finished") ||
			strings.Contains(tn.Prompt, "sessions/") || !strings.HasSuffix(w, "sessions/"+c.Session) {
			t.Errorf("result of %s in %s: %+v\n%s", c.Turn, w, tn.Origin, tn.Prompt)
		}
		// On the parent's own agent.
		if tn.Harness != "claude" {
			t.Errorf("result of %s ran on %s", c.Turn, tn.Harness)
		}
	}

	// The parent's token is gone; the result turn's own token asks for a
	// review of a child's work, and that comes back too.
	j1, err := e.q.JobByTurn(queue.DefaultTeam, c1.Turn)
	if err != nil || j1.Ended == nil {
		t.Fatalf("child job: %v %+v", err, j1)
	}
	p2 := startTurn(t, e, `{"agent":"claude","session":"`+p.Session+`","prompt":"again"}`)
	atok := tokenOf(t, dir, "again")
	code, out := as(t, e, atok, "POST", "/api/v0/turns/"+c1.Turn+"/review", `{"agent":"codex"}`)
	if code != 202 {
		t.Fatalf("review: %d %s", code, out)
	}
	var rs Started
	_ = json.Unmarshal([]byte(out), &rs)
	rj, err := e.q.JobByTurn(queue.DefaultTeam, rs.Turn)
	if err != nil || rj.Links.Asker == nil || rj.Links.Asker.Turn != p2.Turn || rj.Links.Chain == nil || rj.Links.Chain.Root != p.Session {
		t.Errorf("review %+v: %v %+v", rs, err, rj.Links)
	}
	// A stop of asked work in the tree is allowed.
	c3 := ask(t, e, atok, `{"agent":"claude","prompt":"three"}`)
	if code, out := as(t, e, atok, "POST", "/api/v0/turns/"+c3.Turn+"/stop", ""); code != 202 {
		t.Errorf("stop asked work: %d %s", code, out)
	}
	release(dir, "again")
	release(dir, "three")
	got = deliverUntil(t, e, p.Session, 4)
	review := false
	for _, tn := range got {
		if tn.Origin.Turn == rs.Turn {
			review = tn.Origin.TurnKind == "review" && strings.Contains(tn.Prompt, "the review you asked for") && strings.Contains(tn.Prompt, "Verdict:")
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
	root := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"root"}`)
	tok := tokenOf(t, dir, "root")
	ask(t, e, tok, `{"agent":"claude","prompt":"d1"}`)
	ask(t, e, tokenOf(t, dir, "d1"), `{"agent":"claude","prompt":"d2"}`)
	code, out := as(t, e, tokenOf(t, dir, "d2"), "POST", "/api/v0/turns", `{"agent":"claude","prompt":"d3"}`)
	if code != 403 || !strings.Contains(out, "levels") || !strings.Contains(out, "[limits]") {
		t.Errorf("depth 3: %d %s", code, out)
	}
	for i := 1; i < settings.DefaultMaxJobsPerTurn; i++ {
		ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)
	}
	code, out = as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, fmt.Sprint(settings.DefaultMaxJobsPerTurn)) {
		t.Errorf("past the per-turn cap: %d %s", code, out)
	}

	startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"ro","read_only":true}`)
	code, out = as(t, e, tokenOf(t, dir, "ro"), "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x"}`)
	if code != 403 || !strings.Contains(out, "read-only") {
		t.Errorf("read-only turn asking: %d %s", code, out)
	}
	code, out = as(t, e, tokenOf(t, dir, "ro"), "PATCH", "/api/v0/sessions/"+root.Session, `{"title":"x"}`)
	if code != 403 || !strings.Contains(out, "read-only") {
		t.Errorf("read-only turn renaming: %d %s", code, out)
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
	root := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"root"}`)
	tok := tokenOf(t, dir, "root")
	ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)
	ask(t, e, tok, `{"agent":"claude","prompt":"child"}`)
	code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"quick"}`)
	if code != 403 || !strings.Contains(out, "at most 2 turns or reviews") || !strings.Contains(out, "max_jobs_per_turn") {
		t.Fatalf("past max_jobs_per_turn = 2: %d %s", code, out)
	}
	// The file is read again for the next request.
	write("[limits]\nmax_jobs_per_turn = 3\n")
	ask(t, e, tok, `{"agent":"claude","prompt":"quick"}`)

	child := tokenOf(t, dir, "child")
	// Another tree: a session the person started on their own.
	other := startTurn(t, e, `{"agent":"claude","folder":"`+t.TempDir()+`","prompt":"other"}`)
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
	if code, out := as(t, e, child, "POST", "/api/v0/turns/"+root.Turn+"/review", `{"agent":"claude"}`); code == 403 {
		t.Errorf("review a turn in its tree: %d %s", code, out)
	}
	// Outside its tree, nothing: not to read, rename, review or stop.
	for _, tc := range []struct {
		name, method, path, body, want string
	}{
		{"read another tree", "GET", "/api/v0/sessions/" + other.Session + "/search?q=x", "", "only its own session"},
		{"read another tree by query", "GET", "/api/v0/turns?session=" + other.Session, "", "only its own session"},
		{"rename another tree's session", "PATCH", "/api/v0/sessions/" + other.Session, `{"title":"x"}`, "only its own session"},
		{"review another tree's turn", "POST", "/api/v0/turns/" + other.Turn + "/review", `{"agent":"claude"}`, "only its own session"},
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
	if code != 403 || !strings.Contains(out, "max_cost_usd_per_tree") || !strings.Contains(out, "$0.30") {
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

// A turn may start a session in the folder it runs in (its copy) or one
// inside it, as well as in its session's own folder; never elsewhere. The
// connector says whether a path is inside, where the folders are.
func TestAskInOwnCopy(t *testing.T) {
	e, dir := askEnv(t)
	folder := t.TempDir()
	os.MkdirAll(filepath.Join(folder, "sub"), 0o755)
	os.WriteFile(filepath.Join(folder, "sub", "x.txt"), []byte("x"), 0o644)
	startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"root"}`)
	tok := tokenOf(t, dir, "root")
	copyDir := pwdOf(t, dir, "root")
	if copyDir == folder {
		t.Fatalf("the root turn ran in the folder itself")
	}
	ask(t, e, tok, `{"agent":"claude","prompt":"in-copy","folder":"`+copyDir+`"}`)
	ask(t, e, tok, `{"agent":"claude","prompt":"in-copy-sub","folder":"`+filepath.Join(copyDir, "sub")+`"}`)
	ask(t, e, tok, `{"agent":"claude","prompt":"in-folder-sub","folder":"`+filepath.Join(folder, "sub")+`"}`)
	if code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","folder":"`+t.TempDir()+`"}`); code != 403 {
		t.Errorf("a folder outside: %d %s", code, out)
	}
	// A folder inside, checked here, never goes to another connector.
	if code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"agent":"claude","prompt":"x","folder":"`+filepath.Join(folder, "sub")+
		`","connector":"con_0123456789abcdef"}`); code != 403 {
		t.Errorf("a folder inside, on another connector: %d %s", code, out)
	}
	for _, p := range []string{"root", "in-copy", "in-copy-sub", "in-folder-sub"} {
		release(dir, p)
	}
}

// A worktree's session asks with no folder: the new session works on the
// worktree as it is (its base and its edits), not on the repo.
func TestAskFromAWorktree(t *testing.T) {
	e, dir := askEnv(t)
	repo := gitRepo(t)
	// No route takes a repo and base any more; the queue still does.
	spec := queue.Spec{Harness: "claude", Repo: repo, Base: "HEAD", Prompt: "root"}
	if msg := queue.ResolveFolders(&spec, e.api.cfg.Folders); msg != "" {
		t.Fatal(msg)
	}
	if _, _, err := e.q.Create(queue.DefaultTeam, queue.KindTurn, "", spec, queue.Links{}); err != nil {
		t.Fatal(err)
	}
	tok := tokenOf(t, dir, "root")
	child := ask(t, e, tok, `{"agent":"claude","prompt":"child"}`)
	j, err := e.q.JobByTurn(queue.DefaultTeam, child.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if j.Spec.FolderRef == "" || j.Spec.FolderRef == folders.Ref(repo) {
		t.Fatalf("the child's folder is %q, the repo's is %q", j.Spec.FolderRef, folders.Ref(repo))
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
	root := startTurn(t, e, `{"agent":"claude","folder":"`+folder+`","prompt":"root"}`)
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
	// With the tools, on by default, the line names the tool and the turn.
	if want := "call turn_diff with turn " + root.Turn + " and session " + root.Session; !strings.Contains(string(args), want) {
		t.Errorf("the child's instructions do not name %s: %s", want, args)
	}
	if got, _ := os.ReadFile(filepath.Join(pwdOf(t, dir, "child"), "a.txt")); string(got) != "one\n" {
		t.Errorf("the child's copy holds %q, want the person's folder", got)
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
	path := "/api/v0/turns/" + root.Turn + "/diff?session=" + root.Session
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		code, out := as(t, e, childTok, "GET", path, "")
		if code != 200 {
			t.Fatalf("the child reads the root's diff: %d %s", code, out)
		}
		var d struct {
			Files *struct {
				Changes []struct {
					Path, Diff string
				} `json:"changes"`
			} `json:"files"`
		}
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatal(err)
		}
		if d.Files != nil {
			if len(d.Files.Changes) != 1 || d.Files.Changes[0].Path != "a.txt" || !strings.Contains(d.Files.Changes[0].Diff, "+two") {
				t.Fatalf("the root's diff: %s", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the root's diff has no files: %s", out)
		}
	}
	release(dir, "child")
}
