package main

// -cases: the step 6 cases, each once, across the two machines: an
// interrupted transfer, a role restart, a repeated request, a disconnected
// connector, a file edited before apply. Each must end with a clear error or
// a clean result, never half-applied work. The tool prints what happened and
// the text the person sees, as a table.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/staysup/agentbus/queue"
)

// outcome is one case: what happened, the text the person sees, and whether
// it ended with a clear error or a clean result.
type outcome struct {
	name, happened, seen string
	ok                   bool
}

func (w *world) cases() error {
	list := []struct {
		name string
		run  func() (outcome, error)
	}{
		{"1a. transfer: connector stopped mid-upload", w.caseUploadCut},
		{"1b. transfer: runner stopped mid-fetch", w.caseFetchCut},
		{"2. role restart: control, connector, runner, mid-session", w.caseRestart},
		{"2b. runner stopped mid-turn, then started again", w.caseRunnerCut},
		{"3. repeated request: one turn key twice, one apply twice", w.caseRepeat},
		{"4. disconnected connector: apply, then a new session", w.caseNoConnector},
		{"5. file edited on the Mac before apply", w.caseEdited},
	}
	var out []outcome
	failed := 0
	for _, c := range list {
		if w.o.only != "" && !strings.HasPrefix(c.name, w.o.only) {
			continue
		}
		fmt.Printf("\ncase %s\n", c.name)
		o, err := c.run()
		if err != nil {
			o = outcome{happened: "the case could not finish: " + err.Error()}
		}
		o.name = c.name
		// A case that stopped early may leave a role stopped: start it, so
		// the next case starts as every case does.
		if err := w.rolesUp(); err != nil {
			return fmt.Errorf("after case %s: %w", c.name, err)
		}
		if !o.ok {
			failed++
		}
		out = append(out, o)
		fmt.Printf("  happened: %s\n  seen: %s\n  ok: %t\n", o.happened, o.seen, o.ok)
	}
	fmt.Printf("\nStep 6 cases\n")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "case\tok\twhat happened\tthe person sees\n")
	for _, o := range out {
		fmt.Fprintf(tw, "%s\t%t\t%s\t%s\n", o.name, o.ok, o.happened, o.seen)
	}
	tw.Flush()
	if failed > 0 {
		return fmt.Errorf("%d of %d cases did not end cleanly", failed, len(out))
	}
	return nil
}

// caseUploadCut stops the connector while it sends a new session's snapshot,
// then starts it again: the turn ends with an error, and the next turn on the
// folder builds a whole copy.
func (w *world) caseUploadCut() (outcome, error) {
	ref := w.folders["cut-upload"]
	before := len(lines(w.serverLog))
	done := w.turnAsync(map[string]any{"agent": "claude", "folder_ref": ref, "connector": w.connector, "isolate": true, "prompt": "cut"})
	if err := waitFor(2*time.Minute, "the connector's first pack", func() bool { return w.logHas(before, "path=/api/v0/runner/packs ") }); err != nil {
		return outcome{}, err
	}
	stopProc(w.conn, syscall.SIGKILL)
	fmt.Println("  connector stopped (KILL) while it sent packs")
	r := <-done
	j, err := w.ended(r)
	if err != nil {
		return outcome{}, err
	}
	seen := w.seen(r, j)
	if err := w.startConnector(); err != nil {
		return outcome{}, err
	}
	// The next turn on the folder: a new session, a whole copy.
	t, _, err := w.turn(map[string]any{"agent": "claude", "folder_ref": ref, "connector": w.connector, "isolate": true, "prompt": "again"}, "")
	if err != nil {
		return outcome{happened: "the next turn after the connector came back: " + err.Error(), seen: seen}, nil
	}
	whole, why := w.onlyStandIn(t.Turn)
	happened := fmt.Sprintf("turn job %s %s; connector started again; next session's turn finished, its diff %s", j.State, j.Reason, why)
	return outcome{happened: happened, seen: seen, ok: stoppedClearly(j) && whole}, nil
}

// caseFetchCut stops the runner while it fetches a new session's copy, then
// starts it again: the turn ends with an error; the session's next turn and
// a new session work on whole copies.
func (w *world) caseFetchCut() (outcome, error) {
	ref := w.folders["cut-fetch"]
	before := len(lines(w.serverLog))
	body := map[string]any{"agent": "claude", "folder_ref": ref, "connector": w.connector, "isolate": true, "prompt": "cut"}
	done := w.turnAsync(body)
	if err := waitFor(2*time.Minute, "the runner's first fetch", func() bool { return w.logHas(before, "path=/api/v0/runner/packs/fetch ") }); err != nil {
		return outcome{}, err
	}
	if err := w.stopRunner("KILL"); err != nil {
		return outcome{}, err
	}
	fmt.Println("  runner stopped (KILL) while it fetched packs")
	r := <-done
	j, err := w.ended(r)
	if err != nil {
		return outcome{}, err
	}
	seen := w.seen(r, j)
	if err := w.startRunner(); err != nil {
		return outcome{}, err
	}
	happened := fmt.Sprintf("turn job %s %s; runner started again", j.State, j.Reason)
	ok := stoppedClearly(j)
	// The same session again, when it has one: a clear error or a whole copy.
	if r.ID != "" {
		r2 := <-w.turnAsync(map[string]any{"session": r.ID, "prompt": "next"})
		if r2.Turn == "" {
			happened += "; the session's next turn refused: " + r2.code
			ok = ok && strings.HasPrefix(r2.code, "409") && strings.Contains(r2.raw, "start a new session")
		} else if j2, err := w.waitJob(r2.Turn, 3*time.Minute); err != nil {
			return outcome{}, err
		} else if j2.State == "finished" {
			whole, why := w.onlyStandIn(j2.Turn)
			happened += "; the session's next turn finished, its diff " + why
			ok = ok && whole
		} else {
			happened += fmt.Sprintf("; the session's next turn %s %s", j2.State, j2.Reason)
			seen += " | next turn: " + w.seen(r2, j2)
			ok = false
		}
	}
	t, _, err := w.turn(merge(body, map[string]any{"prompt": "again"}), "")
	if err != nil {
		return outcome{happened: happened + "; a new session: " + err.Error(), seen: seen}, nil
	}
	whole, why := w.onlyStandIn(t.Turn)
	happened += "; a new session's turn finished, its diff " + why
	return outcome{happened: happened, seen: seen, ok: ok && whole}, nil
}

// caseRestart restarts control while a turn runs, then the connector, then
// the runner, between turns of one session; the session goes on and its
// apply puts every turn in the folder.
func (w *world) caseRestart() (outcome, error) {
	proj := filepath.Join(w.dir, "project")
	w.resetProject()
	body := map[string]any{"agent": "claude", "folder_ref": w.ref, "connector": w.connector, "isolate": true}
	t1, _, err := w.turn(merge(body, map[string]any{"prompt": "r1"}), "")
	if err != nil {
		return outcome{}, err
	}
	// A slow turn; control restarts while it runs.
	done := w.turnAsync(map[string]any{"session": t1.ID, "prompt": "slow"})
	r := <-done
	if r.Turn == "" {
		return outcome{}, fmt.Errorf("the slow turn: %s", r.code)
	}
	time.Sleep(2 * time.Second)
	stopProc(w.server, syscall.SIGTERM)
	if err := w.startServer(); err != nil {
		return outcome{}, err
	}
	fmt.Println("  control restarted while the slow turn ran")
	j2, err := w.waitJob(r.Turn, 3*time.Minute)
	if err != nil {
		return outcome{}, err
	}
	var steps []string
	steps = append(steps, fmt.Sprintf("turn during control's restart: %s %s", j2.State, j2.Reason))
	seen := ""
	if j2.State != "finished" {
		seen = w.seen(r, j2)
	}
	stopProc(w.conn, syscall.SIGTERM)
	if err := w.startConnector(); err != nil {
		return outcome{}, err
	}
	if err := w.stopRunner("TERM"); err != nil {
		return outcome{}, err
	}
	if err := w.startRunner(); err != nil {
		return outcome{}, err
	}
	fmt.Println("  connector and runner restarted")
	t3, _, err := w.turn(map[string]any{"session": t1.ID, "prompt": "r3"}, "")
	if err != nil {
		steps = append(steps, "turn after the connector and runner restarted: "+err.Error())
		return outcome{happened: strings.Join(steps, "; "), seen: seen}, nil
	}
	steps = append(steps, "turn after the connector and runner restarted: finished")
	code, a := w.api.apply(t3.Turn)
	want := "one\nr1\n"
	if j2.State == "finished" {
		want += "slow\n"
	}
	want += "r3\n"
	got := text(filepath.Join(proj, "a.txt"))
	steps = append(steps, fmt.Sprintf("apply up to the last turn: %d %s, a.txt %q", code, a.State, got))
	if seen == "" {
		seen = "nothing: every turn finished; apply " + a.State
	}
	return outcome{happened: strings.Join(steps, "; "), seen: seen, ok: j2.State == "finished" && code == 200 && a.State == "applied" && got == want}, nil
}

// caseRunnerCut stops the runner (KILL) while a turn of a session runs, and
// starts it again: the turn ends with an error, in its job and in the
// record; the session's next turn runs, and its apply holds what finished.
func (w *world) caseRunnerCut() (outcome, error) {
	proj := filepath.Join(w.dir, "project")
	w.resetProject()
	body := map[string]any{"agent": "claude", "folder_ref": w.ref, "connector": w.connector, "isolate": true}
	t1, _, err := w.turn(merge(body, map[string]any{"prompt": "k1"}), "")
	if err != nil {
		return outcome{}, err
	}
	r := <-w.turnAsync(map[string]any{"session": t1.ID, "prompt": "slow"})
	if r.Turn == "" {
		return outcome{}, fmt.Errorf("the slow turn: %s", r.code)
	}
	time.Sleep(2 * time.Second)
	if err := w.stopRunner("KILL"); err != nil {
		return outcome{}, err
	}
	fmt.Println("  runner stopped (KILL) while the slow turn ran")
	j, err := w.waitJob(r.Turn, 3*time.Minute)
	if err != nil {
		return outcome{}, err
	}
	seen := w.seen(r, j)
	// Control closes a lost turn in its record on its next sweep, within a
	// second or so of the job's end.
	var one struct{ Turn struct{ State, Error string } }
	if err := waitFor(5*time.Second, "the record", func() bool {
		w.api.call("GET", "/api/v0/turns/"+r.Turn, nil, &one)
		return one.Turn.State == "failed"
	}); err != nil {
		steps := fmt.Sprintf("slow turn's job %s %s; its record %q, not failed", j.State, j.Reason, one.Turn.State)
		w.startRunner()
		return outcome{happened: steps, seen: seen}, nil
	}
	steps := []string{fmt.Sprintf("slow turn's job %s %s; its record %s", j.State, j.Reason, one.Turn.State)}
	if err := w.startRunner(); err != nil {
		return outcome{}, err
	}
	t3, _, err := w.turn(map[string]any{"session": t1.ID, "prompt": "k3"}, "")
	if err != nil {
		steps = append(steps, "the session's next turn: "+err.Error())
		return outcome{happened: strings.Join(steps, "; "), seen: seen}, nil
	}
	// The slow turn's agent may outlive its runner and write after all:
	// then its write must be in the next turn's diff, not applied unseen:
	// new.txt, which it removed and the next turn added again, shows as
	// modified from the first turn's end, not as added.
	_, why := w.onlyStandIn(t3.Turn)
	code, a := w.api.apply(t3.Turn)
	got := text(filepath.Join(proj, "a.txt"))
	steps = append(steps, fmt.Sprintf("runner started again; the session's next turn finished, its diff %s; apply %d %s, a.txt %q", why, code, a.State, got))
	ok := j.State == "failed" && code == 200 && a.State == "applied" &&
		(got == "one\nk1\nk3\n" || got == "one\nk1\nslow\nk3\n" && strings.Contains(why, "new.txt:modified"))
	return outcome{happened: strings.Join(steps, "; "), seen: seen, ok: ok}, nil
}

// caseRepeat sends one turn request with an idempotency key twice at once,
// then one apply twice at once: one turn, one apply.
func (w *world) caseRepeat() (outcome, error) {
	w.resetProject()
	key := fmt.Sprintf("step6-%d", time.Now().UnixNano())
	body := map[string]any{"agent": "claude", "folder_ref": w.ref, "connector": w.connector, "isolate": true, "prompt": "twice", "idempotency_key": key}
	a, b := w.turnAsync(body), w.turnAsync(body)
	ra, rb := <-a, <-b
	if ra.Turn == "" || rb.Turn == "" {
		return outcome{happened: "a request was refused: " + ra.code + " / " + rb.code}, nil
	}
	j, err := w.waitJob(ra.Turn, 3*time.Minute)
	if err != nil {
		return outcome{}, err
	}
	var turns struct{ Turns []struct{ ID string } }
	w.api.call("GET", "/api/v0/turns?session="+ra.ID, nil, &turns)
	steps := []string{fmt.Sprintf("turn requests: turns %s and %s, existing %t/%t, session turns %d, job %s",
		ra.Turn, rb.Turn, ra.Existing, rb.Existing, len(turns.Turns), j.State)}
	ok := ra.Turn == rb.Turn && ra.ID == rb.ID && ra.Existing != rb.Existing && len(turns.Turns) == 1 && j.State == "finished"
	// The same apply twice at once.
	var wg sync.WaitGroup
	var codes [2]int
	var answers [2]applyAnswer
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); codes[i], answers[i] = w.api.apply(j.Turn) }()
	}
	wg.Wait()
	var applies struct{ Jobs []queue.Job }
	if q, err := w.api.queue(); err == nil {
		applies.Jobs, _ = q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindApply, Session: ra.ID}, 100)
	}
	got := text(filepath.Join(w.dir, "project", "a.txt"))
	steps = append(steps, fmt.Sprintf("applies: %d %s and %d %s, apply jobs %d, a.txt %q",
		codes[0], answers[0].State, codes[1], answers[1].State, len(applies.Jobs), got))
	ok = ok && codes[0] == 200 && codes[1] == 200 && answers[0].State == "applied" && answers[1].State == "applied" &&
		len(applies.Jobs) == 1 && got == "one\ntwice\n"
	return outcome{happened: strings.Join(steps, "; "), seen: "one turn and one apply; the second answer says existing (turn) or applied again (apply)", ok: ok}, nil
}

// caseNoConnector stops the connector and, once control has not seen it for
// a minute (queue.RunnerFresh), asks for an apply and for a new session on
// its folder; then starts it again and asks for the apply again.
func (w *world) caseNoConnector() (outcome, error) {
	w.resetProject()
	body := map[string]any{"agent": "claude", "folder_ref": w.ref, "connector": w.connector, "isolate": true}
	t, _, err := w.turn(merge(body, map[string]any{"prompt": "gone"}), "")
	if err != nil {
		return outcome{}, err
	}
	stopProc(w.conn, syscall.SIGTERM)
	fmt.Println("  connector stopped; waiting 65 s")
	time.Sleep(65 * time.Second)
	var steps, seen []string
	t0 := time.Now()
	var a applyAnswer
	code, raw := w.api.call("POST", "/api/v0/turns/"+t.Turn+"/apply", nil, &a)
	steps = append(steps, fmt.Sprintf("apply: %d after %s, a.txt %q", code, time.Since(t0).Round(100*time.Millisecond), text(filepath.Join(w.dir, "project", "a.txt"))))
	seen = append(seen, "apply: "+short(raw))
	ok := code == 503 && strings.Contains(raw, "connector is not connected") && text(filepath.Join(w.dir, "project", "a.txt")) == "one\n"
	t0 = time.Now()
	r := <-w.turnAsync(merge(body, map[string]any{"prompt": "new"}))
	steps = append(steps, fmt.Sprintf("new session: %s after %s", strings.SplitN(r.code, " ", 2)[0], time.Since(t0).Round(100*time.Millisecond)))
	seen = append(seen, "new session: "+short(r.raw))
	j, err := w.ended(r)
	if err != nil {
		return outcome{}, err
	}
	steps = append(steps, fmt.Sprintf("its job %s %s", j.State, j.Reason))
	// Refused before the turn started: 503 with the reason, and no job.
	ok = ok && r.Turn == "" && strings.HasPrefix(r.code, "503") && strings.Contains(r.raw, "connector is not connected")
	if err := w.startConnector(); err != nil {
		return outcome{}, err
	}
	var after applyAnswer
	acode := 0
	if err := waitFor(time.Minute, "the apply", func() bool {
		acode, after = w.api.apply(t.Turn)
		return acode != 503
	}); err != nil {
		steps = append(steps, "connector back: the apply still answered 503 a minute later")
		return outcome{happened: strings.Join(steps, "; "), seen: strings.Join(seen, " | ")}, nil
	}
	got := text(filepath.Join(w.dir, "project", "a.txt"))
	steps = append(steps, fmt.Sprintf("connector back, asked again: apply %d %s, a.txt %q", acode, after.State, got))
	ok = ok && after.State == "applied" && got == "one\ngone\n"
	return outcome{happened: strings.Join(steps, "; "), seen: strings.Join(seen, " | "), ok: ok}, nil
}

// caseEdited edits files on the Mac before an apply: one the turn changed
// (refused, nothing written), one it did not (applied, the edit kept).
func (w *world) caseEdited() (outcome, error) {
	proj := filepath.Join(w.dir, "project")
	w.resetProject()
	body := map[string]any{"agent": "claude", "folder_ref": w.ref, "connector": w.connector, "isolate": true, "prompt": "e1"}
	t, _, err := w.turn(body, "")
	if err != nil {
		return outcome{}, err
	}
	os.WriteFile(filepath.Join(proj, "a.txt"), []byte("mine\n"), 0o644)
	var a applyAnswer
	code, raw := w.api.call("POST", "/api/v0/turns/"+t.Turn+"/apply", nil, &a)
	refusedOK := code == 409 && a.State == "refused" && len(a.Conflicts) == 1 && a.Conflicts[0].Path == "a.txt" &&
		text(filepath.Join(proj, "a.txt")) == "mine\n" && text(filepath.Join(proj, "new.txt")) == "<missing>"
	steps := []string{fmt.Sprintf("a.txt edited: %d %s, folder untouched %t", code, a.State, refusedOK)}
	seen := "a.txt edited: " + short(raw)
	// Put a.txt back, edit b.txt, which the turn did not touch.
	os.WriteFile(filepath.Join(proj, "a.txt"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "b.txt"), []byte("mine too\n"), 0o644)
	code, a = w.api.apply(t.Turn)
	got := text(filepath.Join(proj, "a.txt")) + "|" + text(filepath.Join(proj, "b.txt")) + "|" + text(filepath.Join(proj, "new.txt"))
	steps = append(steps, fmt.Sprintf("a.txt put back, b.txt edited, asked again: %d %s, files %q", code, a.State, got))
	os.WriteFile(filepath.Join(proj, "b.txt"), []byte("keep\n"), 0o644)
	ok := refusedOK && code == 200 && a.State == "applied" && got == "one\ne1\n|mine too\n|e1\n"
	return outcome{happened: strings.Join(steps, "; "), seen: seen, ok: ok}, nil
}

// resetProject puts the project folder back as setup made it.
func (w *world) resetProject() {
	proj := filepath.Join(w.dir, "project")
	os.WriteFile(filepath.Join(proj, "a.txt"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "b.txt"), []byte("keep\n"), 0o644)
	os.Remove(filepath.Join(proj, "new.txt"))
}

// turnReply is the answer to POST /api/v0/turns, sent in the background. ID
// is the session.
type turnReply struct {
	ID        string `json:"session"`
	Turn      string
	Existing  bool
	code, raw string
}

func (w *world) turnAsync(body map[string]any) <-chan turnReply {
	c := make(chan turnReply, 1)
	go func() {
		var r turnReply
		code, raw := w.api.call("POST", "/api/v0/turns", body, &r)
		r.code, r.raw = strconv.Itoa(code)+" "+short(raw), raw
		c <- r
	}()
	return c
}

// fullJob is a job with its result's refusal and error.
type fullJob struct {
	job
	refused, err string
}

// getJob is the job of turn, from the server's queue.
func (w *world) getJob(turn string) fullJob {
	var j job
	w.api.jobOfTurn(turn, &j)
	var res struct{ Refused, Error string }
	if len(j.Result) > 0 {
		_ = json.Unmarshal(j.Result, &res)
	}
	return fullJob{j, res.Refused, res.Error}
}

// ended is the job of a turn request once it has ended. A request refused
// before its turn started answers an error and no job: then the job is
// empty, and seen reports the answer.
func (w *world) ended(r turnReply) (fullJob, error) {
	if r.Turn == "" {
		if strings.HasPrefix(r.code, "2") {
			return fullJob{}, fmt.Errorf("POST /api/v0/turns answered %s without a turn", r.code)
		}
		return fullJob{job: job{State: "refused"}}, nil
	}
	return w.waitJob(r.Turn, 3*time.Minute)
}

// waitJob waits for the job of turn to end.
func (w *world) waitJob(turn string, limit time.Duration) (fullJob, error) {
	var j fullJob
	err := waitFor(limit, "turn "+turn, func() bool { j = w.getJob(turn); return j.Ended != nil })
	return j, err
}

// seen is the text the person sees for a turn that did not finish: the
// answer to the request when it was an error, and the job's reason and
// refusal, which the cockpit shows.
func (w *world) seen(r turnReply, j fullJob) string {
	var parts []string
	if !strings.HasPrefix(r.code, "2") {
		parts = append(parts, "POST /turns "+r.code)
	}
	s := "job " + j.State
	if j.Reason != "" {
		s += " (" + j.Reason + ")"
	}
	if j.refused != "" {
		s += ": " + j.refused
	}
	if j.err != "" {
		s += ": " + j.err
	}
	return strings.Join(append(parts, s), "; ")
}

// stoppedClearly: the turn ended with a reason, as a failed job or as a
// request refused before it started (ended).
func stoppedClearly(j fullJob) bool { return j.State == "failed" || j.State == "refused" }

// onlyStandIn says whether the turn's diff holds only what the stand-in did
// (a.txt modified, new.txt added): a copy built in part would show the
// files it lacks as deleted.
func (w *world) onlyStandIn(turn string) (bool, string) {
	var d struct {
		Files *struct {
			Changes []struct{ Path, Change string }
		}
	}
	code, raw := w.api.call("GET", "/api/v0/turns/"+turn+"/diff", nil, &d)
	if code != 200 || d.Files == nil {
		return false, "not read: " + short(raw)
	}
	got := map[string]string{}
	for _, c := range d.Files.Changes {
		got[c.Path] = c.Change
	}
	if len(got) == 2 && got["a.txt"] == "modified" && got["new.txt"] == "added" {
		return true, "a.txt modified, new.txt added (whole copy)"
	}
	return false, fmt.Sprintf("%d changes, e.g. %v", len(got), firstN(got, 3))
}

func firstN(m map[string]string, n int) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if len(out) == n {
			break
		}
		out[k] = v
	}
	return out
}

func (w *world) logHas(from int, s string) bool {
	l := lines(w.serverLog)
	for _, x := range l[min(from, len(l)):] {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// startServer starts control on this Mac, on its data folder, answering the
// VM by host.lima.internal.
func (w *world) startServer() error {
	home := filepath.Join(w.dir, "server")
	addr := fmt.Sprintf("127.0.0.1:%d", w.o.port)
	w.serverLog = filepath.Join(home, "logs", "service.log")
	var err error
	if w.server, err = start(w.bin, home, filepath.Join(w.dir, "server.out"), "server", "-listen", addr, "-host", "host.lima.internal", "-replay=false"); err != nil {
		return err
	}
	w.api, err = waitService("http://"+addr, home, w.server)
	return err
}

// startConnector starts the connector on this Mac, owning the folders.
func (w *world) startConnector() error {
	addr := fmt.Sprintf("http://127.0.0.1:%d", w.o.port)
	enroll := filepath.Join(w.dir, "server", "enroll_token")
	var err error
	w.conn, err = start(w.bin, filepath.Join(w.dir, "connector"), filepath.Join(w.dir, "connector.out"),
		append([]string{"connector", "-server", addr, "-enroll-token-file", enroll, "-name", "mac"}, w.folderArgs...)...)
	return err
}

// stopProc stops a process start started, and its group with KILL.
func stopProc(p *proc, sig syscall.Signal) {
	if p == nil {
		return
	}
	if sig == syscall.SIGKILL {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	} else {
		p.cmd.Process.Signal(sig)
	}
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// startRunner starts the runner in the VM, by env -i, and waits until it
// takes jobs. Only the runner goes to the background, with every output to
// its file: what holds the shell's output open holds the ssh call open.
func (w *world) startRunner() error {
	d := w.o.vmDir
	before := w.runnerStarts()
	runCmd := "cd " + d + " && chmod 755 bin/agentbus bin/claude && chmod 600 home/enroll_token || exit 1; " +
		"env -i HOME=" + d + "/home PATH=" + d + "/bin:/usr/bin:/bin AGENTBUS_HOME=" + d + "/home " +
		"setsid nohup bin/agentbus runner -server http://host.lima.internal:" + strconv.Itoa(w.o.port) +
		" -enroll-token-file home/enroll_token -name vm -replay=false >> runner.out 2>&1 < /dev/null & echo $! > runner.pid"
	if out, err := lima(w.o.vm, runCmd); err != nil {
		return fmt.Errorf("the runner did not start in the VM: %s", out)
	}
	if err := waitFor(60*time.Second, "the VM runner", func() bool { return w.runnerStarts() > before }); err != nil {
		out, _ := lima(w.o.vm, "tail -20 "+d+"/runner.out")
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// rolesUp starts control, the connector and the VM runner again when any
// is stopped.
func (w *world) rolesUp() error {
	select {
	case <-w.server.done:
		if err := w.startServer(); err != nil {
			return err
		}
	default:
	}
	select {
	case <-w.conn.done:
		if err := w.startConnector(); err != nil {
			return err
		}
	default:
	}
	out, _ := lima(w.o.vm, "kill -0 -- -$(cat "+w.o.vmDir+"/runner.pid) 2>/dev/null && echo up")
	if !strings.Contains(out, "up") {
		return w.startRunner()
	}
	return nil
}

func (w *world) runnerStarts() int {
	out, _ := lima(w.o.vm, "grep -c 'runner taking jobs' "+w.o.vmDir+"/runner.out 2>/dev/null")
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// stopRunner sends sig (TERM or KILL) to the runner's process group in the
// VM and waits until it has ended.
func (w *world) stopRunner(sig string) error {
	d := w.o.vmDir
	// TERM waits up to 30 s, then KILL.
	out, _ := lima(w.o.vm, "test -s "+d+"/runner.pid || { echo no pid; exit 0; }; p=$(cat "+d+"/runner.pid); kill -"+sig+" -- -$p 2>/dev/null; "+
		"for i in $(seq 1 300); do kill -0 -- -$p 2>/dev/null || exit 0; sleep 0.1; done; kill -KILL -- -$p 2>/dev/null; "+
		"for i in $(seq 1 50); do kill -0 -- -$p 2>/dev/null || exit 0; sleep 0.1; done; echo still running")
	switch {
	case strings.Contains(out, "no pid"):
		return fmt.Errorf("the runner in the VM has no pid file")
	case strings.Contains(out, "still running"):
		return fmt.Errorf("the runner in the VM did not stop on %s, nor on KILL", sig)
	}
	return nil
}
