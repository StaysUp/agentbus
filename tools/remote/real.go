package main

// -real: after the checks, a real model turn on a runner that holds no
// secret. A second runner in the VM runs the VM's own claude and codex,
// started with env -i (no key, no sign-in), and takes only jobs labelled
// real. Its model calls go through its own gateway to the server's relay,
// which adds the credential: the OpenRouter key from the server's
// environment, read by the variable's name (-key-env), and the person's own
// subscriptions, connected on this scratch server (-connect for claude,
// -connect-codex for codex's ChatGPT sign-in). The record reaches the server
// while the turn runs; the tool says when it first saw the turn's model call
// there.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// realPrompt asks for a short answer and no tools.
const realPrompt = "Reply with the single word pong, and nothing else. Do not use any tools."

func (w *world) real() error {
	d := w.o.vmDir
	claude, err := lima(w.o.vm, "sh -lc 'command -v claude'")
	claude = strings.TrimSpace(claude)
	if err != nil || claude == "" {
		return fmt.Errorf("no claude on the VM's PATH: %s", claude)
	}
	// codex too, when the VM has it: an npm install, run by node.
	codex, _ := lima(w.o.vm, "sh -lc 'command -v codex'")
	codex = strings.TrimSpace(codex)
	prep := "mkdir -p " + d + "/realbin " + d + "/home2 && ln -sf " + claude + " " + d + "/realbin/claude && cp " + d + "/bin/agentbus " + d + "/realbin/agentbus && " +
		"cp " + d + "/home/enroll_token " + d + "/home2/ && chmod 600 " + d + "/home2/enroll_token"
	codexDir := ""
	if codex != "" {
		prep += " && ln -sf " + codex + " " + d + "/realbin/codex"
		codexDir = ":$(dirname " + codex + ")"
	}
	if out, err := lima(w.o.vm, prep); err != nil {
		return fmt.Errorf("the real runner's folder could not be made in the VM: %s", out)
	}
	// As the first runner: only the runner goes to the background, with
	// every output to its file, or the ssh call stays open.
	// claude's own folder and /usr/local/bin on PATH too: an npm install of
	// claude needs node, which lives there.
	run := "cd " + d + " || exit 1; env -i HOME=" + d + "/home2 PATH=" + d + "/realbin:$(dirname " + claude + ")" + codexDir + ":/usr/local/bin:/usr/bin:/bin AGENTBUS_HOME=" + d + "/home2 " +
		"setsid nohup realbin/agentbus runner -server http://host.lima.internal:" + strconv.Itoa(w.o.port) +
		" -enroll-token-file home2/enroll_token -name vm-real -labels real > runner2.out 2>&1 < /dev/null & echo $! > runner2.pid"
	if out, err := lima(w.o.vm, run); err != nil {
		return fmt.Errorf("the real runner did not start in the VM: %s", out)
	}
	if err := waitFor(60*time.Second, "the real runner", func() bool {
		out, _ := lima(w.o.vm, "grep -c 'runner taking jobs' "+d+"/runner2.out")
		return strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0"
	}); err != nil {
		out, _ := lima(w.o.vm, "cat "+d+"/runner2.out")
		return fmt.Errorf("%w: %s", err, out)
	}
	fmt.Printf("\nreal claude in the VM: %s, real codex: %s, env -i (no key, no sign-in)\n", claude, codex)

	// An OpenRouter agent: the server reads the key by its variable's name.
	keySet := os.Getenv(w.o.keyEnv) != "" // presence only; the value is never read here
	var ag struct{ ID string }
	if code, out := w.api.call("POST", "/api/v0/agents", map[string]any{"name": "remote-openrouter", "harness": "claude",
		"access": "openrouter", "model": w.o.model, "key_env": w.o.keyEnv}, &ag); code != 201 {
		return fmt.Errorf("POST /api/v0/agents: %d %s", code, out)
	}
	fmt.Printf("\nOpenRouter agent, model %s, key from %s on the server (set: %t)\n", w.o.model, w.o.keyEnv, keySet)
	if err := w.realTurn(ag.ID); err != nil {
		return err
	}

	// The person's own subscription, through the server.
	if err := w.subscriptionsOn(); err != nil {
		return err
	}
	if w.o.connect {
		w.connect("claude")
	}
	fmt.Printf("\nclaude subscription through the server (connected: %t)\n", w.connected("claude"))
	if err := w.realTurn("claude"); err != nil {
		return err
	}
	if codex == "" {
		fmt.Println("\nno codex on the VM's PATH: the codex turn is left out")
		return nil
	}
	if w.o.connectCodex {
		w.connect("codex")
	}
	fmt.Printf("\ncodex ChatGPT sign-in through the server (connected: %t)\n", w.connected("codex"))
	return w.realTurn("codex")
}

// connect runs agentbus connect harness here, on the scratch server's data
// folder, in this terminal.
func (w *world) connect(harness string) {
	fmt.Printf("\nConnecting your %s subscription on this scratch server (agentbus connect %s):\n", harness, harness)
	cmd := exec.Command(filepath.Join(w.dir, "agentbus"), "connect", harness)
	cmd.Env = append(os.Environ(), "AGENTBUS_HOME="+filepath.Join(w.dir, "server"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Println("the connect did not finish:", err)
	}
}

// connected says the scratch server keeps harness's subscription.
func (w *world) connected(harness string) bool {
	var subs struct {
		Subscriptions []struct {
			Harness   string
			Connected bool
		}
	}
	w.api.call("GET", "/api/v0/subscriptions", nil, &subs)
	for _, s := range subs.Subscriptions {
		if s.Harness == harness && s.Connected {
			return true
		}
	}
	return false
}

// subscriptionsOn sets subscriptions_on_runners = "own" in the scratch
// server's policy.
func (w *world) subscriptionsOn() error {
	var p map[string]any
	if code, out := w.api.call("GET", "/api/v0/settings/permissions", nil, &p); code != 200 {
		return fmt.Errorf("GET permissions: %d %s", code, out)
	}
	p["subscriptions_on_runners"] = "own"
	if code, out := w.api.callMatch("PUT", "/api/v0/settings/permissions", p, "*"); code != 200 {
		return fmt.Errorf("PUT permissions: %d %s", code, out)
	}
	return nil
}

// realTurn runs one turn on agent on the real runner, and reports what the
// server saw while it ran and how it ended.
func (w *world) realTurn(agent string) error {
	before := len(lines(w.serverLog))
	t0 := time.Now()
	// Labels pick the real runner; the client API's POST /turns takes none,
	// so the turn's job is made in the server's queue.
	created, err := w.api.queueTurn(map[string]any{"agent": agent,
		"folder_ref": w.ref, "connector": w.connector, "isolate": true, "labels": []string{"real"}, "prompt": realPrompt})
	if err != nil {
		return err
	}
	type turnView struct {
		Turn struct {
			State      string
			ModelCalls int `json:"model_calls"`
			Reply      string
			Error      string
		}
	}
	var j job
	var t turnView
	live := time.Duration(-1) // when the server first held a model call of the turn while its job ran
	if err := waitFor(5*time.Minute, "the real turn", func() bool {
		j = job{}
		w.api.jobByID(created.ID, &j)
		if j.Attempt != "" {
			t = turnView{}
			w.api.call("GET", "/api/v0/turns/"+j.Turn, nil, &t)
			if live < 0 && j.Ended == nil && t.Turn.ModelCalls > 0 {
				live = time.Since(t0)
			}
		}
		return j.Ended != nil
	}); err != nil {
		return err
	}
	total := time.Since(t0)
	// The turn as the server's record holds it once the job ended.
	t = turnView{}
	w.api.call("GET", "/api/v0/turns/"+j.Turn, nil, &t)
	relay := map[string]int{}
	for _, l := range lines(w.serverLog)[before:] {
		if i := strings.Index(l, "path=/relay/"); i >= 0 {
			f := strings.Fields(l[i:])
			status := ""
			for _, x := range f {
				if v, ok := strings.CutPrefix(x, "status="); ok {
					status = v
				}
			}
			relay[status]++
		}
	}
	liveText := "not seen while it ran"
	if live >= 0 {
		liveText = fmt.Sprintf("seen at %s while it ran", live.Round(time.Millisecond))
	}
	fmt.Printf("  job %s after %s; turn %s, %d model calls, model call on the server %s\n", j.State, total.Round(time.Millisecond), t.Turn.State, t.Turn.ModelCalls, liveText)
	fmt.Printf("  relay requests by status: %v\n", relay)
	if t.Turn.Reply != "" {
		fmt.Printf("  reply: %q\n", clip(t.Turn.Reply, 300))
	}
	if t.Turn.Error != "" {
		fmt.Printf("  error: %q\n", clip(t.Turn.Error, 300))
	}
	// What the closed-off agent tried to reach and was refused, by host.
	var r struct {
		Events []struct{ Kind, Host string }
	}
	w.api.call("GET", "/api/v0/sessions/"+j.Session+"/events?turn="+j.Turn+"&limit=1000", nil, &r)
	refused := map[string]int{}
	for _, e := range r.Events {
		if e.Kind == "network.refused" {
			refused[e.Host]++
		}
	}
	fmt.Printf("  network.refused by host: %v\n", refused)
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
