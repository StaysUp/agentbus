package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/jobs"
)

// GET /api/v0/runners lists each runner and connector for a person: its
// role, labels, harnesses and whether it is online. It never holds a token,
// and only the main token reads it.
func TestListRunners(t *testing.T) {
	e := newJobsEnv(t)
	register := func(body string) jobs.Enrolled {
		t.Helper()
		r := e.req(t, "POST", "/api/v0/runner/register", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+enrollTok)
		resp, raw := do(t, r)
		var en jobs.Enrolled
		if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &en) != nil || en.Token == "" {
			t.Fatalf("register: %d %s", resp.StatusCode, raw)
		}
		return en
	}
	run := register(`{"name":"mac","capacity":4,"labels":["gpu"],"protocol":2,"capabilities":{"os":"darwin","arch":"arm64",` +
		`"harnesses":[{"name":"claude","found":true,"version":"2.1.319","signed_in":true},{"name":"codex","found":false}]}}`)
	conn := register(`{"name":"mac","capacity":1,"protocol":2,"capabilities":{"role":"connector","connector":"con_0123456789abcdef"}}`)

	resp, raw := do(t, e.req(t, "GET", "/api/v0/runners", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v0/runners: %d %s", resp.StatusCode, raw)
	}
	noToken(t, resp, raw)
	for _, tok := range []string{run.Token, conn.Token, enrollTok} {
		if strings.Contains(raw, tok) {
			t.Fatalf("a token in the answer: %s", raw)
		}
	}
	var list RunnerList
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list.Runners) != 2 {
		t.Fatalf("answer %s, %v; want two runners", raw, err)
	}
	by := map[string]RunnerInfo{}
	for _, rn := range list.Runners {
		by[rn.ID] = rn
	}
	r, c := by[run.Runner.ID], by[conn.Runner.ID]
	if r.Name != "mac" || r.Role != "runner" || !r.Online || r.Capacity != 4 || len(r.Labels) != 1 || r.Labels[0] != "gpu" ||
		r.OS != "darwin" || r.Arch != "arm64" || r.LastSeen.IsZero() || r.Enrolled.IsZero() {
		t.Errorf("runner %+v", r)
	}
	if len(r.Harnesses) != 2 || r.Harnesses[0].Name != "claude" || !r.Harnesses[0].Found || r.Harnesses[0].Version != "2.1.319" ||
		r.Harnesses[0].SignedIn == nil || !*r.Harnesses[0].SignedIn || r.Harnesses[1].Name != "codex" || r.Harnesses[1].Found {
		t.Errorf("runner harnesses %+v", r.Harnesses)
	}
	if c.Role != "connector" || !c.Online || c.Labels == nil || c.Harnesses == nil || len(c.Harnesses) != 0 {
		t.Errorf("connector %+v, want role connector with empty lists", c)
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
