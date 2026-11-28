package api

import (
	"encoding/json"
	"github.com/staysup/agentbus/event"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
)

// POST /api/v0/commands (H1): only an agent's turn runs a command, under its
// turn's command rules, and not out of a copy's wall. What it accepts is an
// exec job in the turn's workspace whose delivery on is listed for the turn's
// session; the command is read and stopped by its id.
func TestRunCommandRoute(t *testing.T) {
	grants := map[string]run.Grant{}
	e, db := rulesEnv(t, grants)
	seedReach(t, e.store)
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	e.api.cfg.Queue = q
	// Each turn's turn.started says what a command of it may do, as its runner
	// recorded it at launch (harness.CommandLimit): limit "" may run, "old"
	// records nothing (an older runner), else the reason it may not.
	turnIn := func(tok, session, workspace, harness, limit string) {
		t.Helper()
		j, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Session: ids.SessionID(session),
			Workspace: ids.WorkspaceID(workspace), Harness: harness, Prompt: "hi"}, queue.Links{})
		if err != nil {
			t.Fatal(err)
		}
		grants[tok] = run.Grant{Session: session, Turn: j.Turn, Root: session}
		ev := event.Event{ID: "ev_" + tok, Kind: event.TurnStarted, Time: time.Now().UTC(), Session: session, Turn: j.Turn, Harness: harness}
		switch limit {
		case "":
			ev.CommandsRun = true
		case "old":
		default:
			ev.CommandLimit = limit
		}
		if _, _, err := e.store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	turnIn("abr_direct", "s_d", "ws_d", "claude", "")
	turnIn("abr_copy", "s_asker", "ws_a", "claude", "Claude Code runs your commands in its own sandbox in this copy")
	turnIn("abr_read_only", "s_mine", "ws_d", "codex", "your turn may not edit its folder (edits are forbidden, or it runs read-only), and a command could write")
	turnIn("abr_old", "s_sib", "ws_d", "claude", "old")

	post := func(tok, body string) (int, string) {
		t.Helper()
		return callAs(t, e, tok, "POST", "/api/v0/commands", body)
	}
	// The main token has no turn to run it for.
	if code, out := post(token, `{"command":"make gate"}`); code != http.StatusForbidden || !strings.Contains(out, "only an agent's turn") {
		t.Errorf("main token: %d %s", code, out)
	}
	for _, body := range []string{`{}`, `{"command":"  "}`, `{"command":"x","timeout_minutes":241}`, `{"command":"x","other":1}`} {
		if code, out := post("abr_direct", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", body, code, out)
		}
	}
	// The route decides by what the runner recorded: a turn limited in a way
	// the command would escape is refused, saying why (review P1, 2026-11-27:
	// a read-only codex turn's command was accepted), and so is a turn whose
	// record says nothing.
	for tok, want := range map[string]string{"abr_copy": "own sandbox", "abr_read_only": "may not edit", "abr_old": "from before commands"} {
		if code, out := post(tok, `{"command":"echo x > /tmp/outside"}`); code != http.StatusForbidden || !strings.Contains(out, want) {
			t.Errorf("%s: %d %s, want 403 saying %q", tok, code, out, want)
		}
	}
	if n, err := q.List(queue.DefaultTeam, queue.Filter{Kind: queue.KindExec}, 10); err != nil || len(n) != 0 {
		t.Fatalf("refused commands made jobs: %d %v", len(n), err)
	}
	// The turn's command rules: forbid refuses, ask refuses (no one answers).
	p, _, err := db.Policy()
	if err != nil {
		t.Fatal(err)
	}
	p.Forbid = append(p.Forbid, "git push")
	p.Ask = append(p.Ask, "make release")
	if _, _, err := db.SetPolicy(p, settings.Any); err != nil {
		t.Fatal(err)
	}
	if code, out := post("abr_direct", `{"command":"make test && git push"}`); code != http.StatusForbidden || !strings.Contains(out, `refused by the rule \"git push\"`) {
		t.Errorf("a forbidden command: %d %s", code, out)
	}
	if code, out := post("abr_direct", `{"command":"make release"}`); code != http.StatusForbidden || !strings.Contains(out, "person") {
		t.Errorf("an ask command: %d %s", code, out)
	}
	// Allowed: an exec job in the turn's workspace, its on listed for the session.
	code, out := post("abr_direct", `{"command":"make gate","timeout_minutes":20,"idempotency_key":"k1"}`)
	var st CommandStarted
	if code != http.StatusAccepted || json.Unmarshal([]byte(out), &st) != nil || !strings.HasPrefix(st.Command, "c_") || st.State != "queued" {
		t.Fatalf("an allowed command: %d %s", code, out)
	}
	if code, again := post("abr_direct", `{"command":"make gate","timeout_minutes":20,"idempotency_key":"k1"}`); code != http.StatusAccepted || !strings.Contains(again, st.Command) || !strings.Contains(again, `"existing":true`) {
		t.Errorf("a retry with the key: %d %s", code, again)
	}
	j, err := q.JobByCommand(queue.DefaultTeam, st.Command)
	if err != nil || j.Kind != queue.KindExec || j.Spec.Workspace != "ws_d" || j.Spec.Command != "make gate" || j.Spec.CommandTimeout != 1200 ||
		j.Links.Chain == nil || j.Links.Chain.By != grants["abr_direct"].Turn {
		t.Fatalf("the command's job: %+v %v", j, err)
	}
	code, out = callAs(t, e, "abr_direct", "GET", "/api/v0/ons?session=s_d", "")
	if code != http.StatusOK || !strings.Contains(out, st.Command) || !strings.Contains(out, `"event":"command.ended"`) {
		t.Errorf("the command's on: %d %s", code, out)
	}
	// Read and stopped by its id: by the asking turn, and by the main token. A
	// turn token of a session outside the asker's reach may do neither.
	var v CommandView
	if code, out := callAs(t, e, "abr_direct", "GET", "/api/v0/commands/"+st.Command, ""); code != http.StatusOK ||
		json.Unmarshal([]byte(out), &v) != nil || v.State != "queued" || v.Line != "make gate" || v.Session != "s_d" || v.Timeout != 20 {
		t.Errorf("GET the command: %d %s", code, out)
	}
	if code, out := callAs(t, e, "abr_copy", "GET", "/api/v0/commands/"+st.Command, ""); code != http.StatusForbidden {
		t.Errorf("GET by a turn outside its reach: %d %s", code, out)
	}
	if code, out := callAs(t, e, "abr_copy", "POST", "/api/v0/commands/"+st.Command+"/stop", ""); code != http.StatusForbidden {
		t.Errorf("stop by a turn outside its reach: %d %s", code, out)
	}
	if code, out := callAs(t, e, token, "GET", "/api/v0/commands/c_0000000000000000", ""); code != http.StatusNotFound {
		t.Errorf("GET of no command: %d %s", code, out)
	}
	// A command is not a turn: the turn routes do not find it.
	if code, out := callAs(t, e, token, "POST", "/api/v0/turns/"+st.Command+"/stop", ""); code != http.StatusNotFound {
		t.Errorf("stopTurn of a command: %d %s", code, out)
	}
	if code, out := callAs(t, e, "abr_direct", "POST", "/api/v0/commands/"+st.Command+"/stop", ""); code != http.StatusAccepted {
		t.Fatalf("stop: %d %s", code, out)
	}
	if code, out := callAs(t, e, token, "GET", "/api/v0/commands/"+st.Command, ""); code != http.StatusOK ||
		json.Unmarshal([]byte(out), &v) != nil || v.State != "stopped" {
		t.Errorf("GET after a stop while queued: %d %s", code, out)
	}
	if code, out := callAs(t, e, token, "POST", "/api/v0/commands/"+st.Command+"/stop", ""); code != http.StatusConflict {
		t.Errorf("a stop of an ended command: %d %s", code, out)
	}
}
