package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
)

// GET /api/v0/runners lists each runner for a person, one per machine: its
// machine, role, labels, harnesses and whether it is online. A connector of
// a build before 0.5 is listed only while no runner reports its machine,
// and none can register now. It never holds a token, and only the main
// token reads it.
func TestListRunners(t *testing.T) {
	e := newJobsEnv(t)
	register := func(body string) (int, string) {
		t.Helper()
		r := e.req(t, "POST", "/api/v0/runner/register", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+enrollTok)
		resp, raw := do(t, r)
		return resp.StatusCode, raw
	}
	const machine, gone = "con_0123456789abcdef", "con_fedcba9876543210"
	code, raw := register(`{"name":"mac","capacity":4,"labels":["gpu"],"protocol":4,"capabilities":{"os":"darwin","arch":"arm64","connector":"` + machine + `",` +
		`"harnesses":[{"name":"claude","found":true,"version":"2.1.319","signed_in":true},{"name":"codex","found":false}]}}`)
	var run jobs.Enrolled
	if code != http.StatusCreated || json.Unmarshal([]byte(raw), &run) != nil || run.Token == "" {
		t.Fatalf("register: %d %s", code, raw)
	}
	if code, raw := register(`{"name":"mac","capacity":1,"protocol":4,"capabilities":{"role":"connector","connector":"` + machine + `"}}`); code != http.StatusBadRequest ||
		!strings.Contains(raw, "agentbus connector was retired") {
		t.Fatalf("a connector registered: %d %s", code, raw)
	}
	// Connectors a 0.4 control enrolled: one on the runner's machine, one on
	// a machine with no runner yet.
	for _, c := range []string{machine, gone} {
		if _, err := e.q.RegisterRunner(queue.DefaultTeam, "old", nil, 4, queue.HashToken(queue.NewID("tok_")),
			&queue.Capabilities{Role: queue.RoleConnector, Connector: c}); err != nil {
			t.Fatal(err)
		}
	}

	resp, raw := do(t, e.req(t, "GET", "/api/v0/runners", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v0/runners: %d %s", resp.StatusCode, raw)
	}
	noToken(t, resp, raw)
	for _, tok := range []string{run.Token, enrollTok} {
		if strings.Contains(raw, tok) {
			t.Fatalf("a token in the answer: %s", raw)
		}
	}
	var list RunnerList
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list.Runners) != 2 {
		t.Fatalf("answer %s, %v; want the runner and the connector of a machine with no runner", raw, err)
	}
	var r, c RunnerInfo
	for _, rn := range list.Runners {
		if rn.ID == run.Runner.ID {
			r = rn
		} else {
			c = rn
		}
	}
	if r.Name != "mac" || r.Role != "runner" || r.Connector != machine || !r.Online || r.Capacity != 4 || len(r.Labels) != 1 || r.Labels[0] != "gpu" ||
		r.OS != "darwin" || r.Arch != "arm64" || r.LastSeen.IsZero() || r.Enrolled.IsZero() {
		t.Errorf("runner %+v", r)
	}
	if len(r.Harnesses) != 2 || r.Harnesses[0].Name != "claude" || !r.Harnesses[0].Found || r.Harnesses[0].Version != "2.1.319" ||
		r.Harnesses[0].SignedIn == nil || !*r.Harnesses[0].SignedIn || r.Harnesses[1].Name != "codex" || r.Harnesses[1].Found {
		t.Errorf("runner harnesses %+v", r.Harnesses)
	}
	if c.Role != "connector" || c.Connector != gone || c.Labels == nil || c.Harnesses == nil || len(c.Harnesses) != 0 {
		t.Errorf("connector %+v, want role connector of the machine with no runner, with empty lists", c)
	}

	// A minute and more without a word: offline, still listed.
	later := time.Now().Add(2 * time.Minute)
	e.api.cfg.Now = func() time.Time { return later }
	_, raw = do(t, e.req(t, "GET", "/api/v0/runners", nil))
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list.Runners) != 2 || list.Runners[0].Online || list.Runners[1].Online {
		t.Errorf("two minutes on: %s, want both listed offline", raw)
	}

	r2 := e.req(t, "GET", "/api/v0/runners", nil)
	r2.Header.Set("Authorization", "Bearer "+turnsTok)
	if resp, raw := do(t, r2); resp.StatusCode != http.StatusForbidden {
		t.Errorf("with the turns token: %d %s, want 403", resp.StatusCode, raw)
	}
	r3 := e.req(t, "GET", "/api/v0/runners", nil)
	r3.Header.Set("Authorization", "Bearer "+run.Token)
	if resp, raw := do(t, r3); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("with a runner's token: %d %s, want 401", resp.StatusCode, raw)
	}
}
