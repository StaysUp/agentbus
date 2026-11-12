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

// A machine that ran agentbus login is listed as pending, with no code and no
// token, until the person types its code to approve it, or refuses it. Only
// the main token decides; a turns token or the machine's own is refused.
func TestPendingMachines(t *testing.T) {
	e := newJobsEnv(t)
	ask := func() jobs.LoginStarted {
		t.Helper()
		r := e.req(t, "POST", "/api/v0/runner/login", strings.NewReader(`{"name":"vm","capacity":2,"protocol":4,"capabilities":{"os":"linux","arch":"arm64",`+
			`"sandbox":{"can":false,"why":"AppArmor refuses user namespaces"}}}`))
		r.Header.Del("Authorization")
		resp, raw := do(t, r)
		var l jobs.LoginStarted
		if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &l) != nil {
			t.Fatalf("login: %d %s", resp.StatusCode, raw)
		}
		return l
	}
	a, b := ask(), ask()
	resp, raw := do(t, e.req(t, "GET", "/api/v0/runners", nil))
	var list RunnerList
	if resp.StatusCode != 200 || json.Unmarshal([]byte(raw), &list) != nil || len(list.Runners) != 0 || len(list.Pending) != 2 {
		t.Fatalf("list: %d %s", resp.StatusCode, raw)
	}
	for _, secret := range []string{a.Code, b.Code, strings.ReplaceAll(a.Code, "-", ""), a.Token, b.Token} {
		if strings.Contains(raw, secret) {
			t.Fatalf("a code or token is in the list: %s", raw)
		}
	}
	p := list.Pending[0]
	if p.ID != a.Login || p.Name != "vm" || p.OS != "linux" || p.Sandbox == nil || p.Sandbox.Can || p.Sandbox.Why != "AppArmor refuses user namespaces" || p.Expires.IsZero() {
		t.Fatalf("pending %+v", p)
	}
	post := func(path, body, tok string) (int, string) {
		t.Helper()
		var r *http.Request
		if body == "" {
			r = e.req(t, "POST", path, nil)
		} else {
			r = e.req(t, "POST", path, strings.NewReader(body))
		}
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, raw := do(t, r)
		return resp.StatusCode, raw
	}
	approve := "/api/v0/runners/pending/" + a.Login + "/approve"
	if code, raw := post(approve, `{"code":"`+a.Code+`"}`, turnsTok); code != http.StatusForbidden {
		t.Fatalf("turns token approves: %d %s", code, raw)
	}
	if code, raw := post(approve, `{"code":"`+a.Code+`"}`, a.Token); code != http.StatusUnauthorized {
		t.Fatalf("the machine approves itself: %d %s", code, raw)
	}
	if code, _ := post(approve, `{"code":"BBBB-CCCC"}`, ""); code != http.StatusBadRequest && a.Code != "BBBB-CCCC" {
		t.Fatalf("a wrong code: %d", code)
	}
	if code, _ := post(approve, `{}`, ""); code != http.StatusBadRequest {
		t.Fatalf("no code: %d", code)
	}
	if code, _ := post("/api/v0/runners/pending/lgn_0000000000000000/approve", `{"code":"`+a.Code+`"}`, ""); code != http.StatusNotFound {
		t.Fatalf("no such machine: %d", code)
	}
	code, raw := post(approve, `{"code":"`+strings.ToLower(a.Code)+`"}`, "")
	var rn RunnerInfo
	if code != 200 || json.Unmarshal([]byte(raw), &rn) != nil || rn.Name != "vm" || rn.Sandbox == nil || rn.Sandbox.Why == "" {
		t.Fatalf("approve: %d %s", code, raw)
	}
	if code, _ := post(approve, `{"code":"`+a.Code+`"}`, ""); code != http.StatusConflict {
		t.Fatalf("approved twice: %d", code)
	}
	refuse := "/api/v0/runners/pending/" + b.Login + "/refuse"
	if code, _ := post(refuse, "", turnsTok); code != http.StatusForbidden {
		t.Fatalf("turns token refuses: %d", code)
	}
	if code, _ := post(refuse, "", ""); code != http.StatusNoContent {
		t.Fatalf("refuse: %d", code)
	}
	_, raw = do(t, e.req(t, "GET", "/api/v0/runners", nil))
	if json.Unmarshal([]byte(raw), &list) != nil || len(list.Runners) != 1 || len(list.Pending) != 0 || list.Runners[0].ID != rn.ID {
		t.Fatalf("after: %s", raw)
	}

	// Remove: main token only, and an online runner is refused.
	remove := "/api/v0/runners/" + rn.ID + "/remove"
	if code, _ := post(remove, "", turnsTok); code != http.StatusForbidden {
		t.Fatalf("turns token removes: %d", code)
	}
	if code, raw := post(remove, "", ""); code != http.StatusConflict || !strings.Contains(raw, "online") {
		t.Fatalf("remove online: %d %s", code, raw)
	}
	if code, _ := post("/api/v0/runners/run_0000000000000000/remove", "", ""); code != http.StatusNotFound {
		t.Fatalf("remove none: %d", code)
	}
}
