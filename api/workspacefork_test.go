package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/staysup/agentbus/store"
)

// POST /api/v0/workspaces with from records a workspace made from another's
// copy: the source's folder and machine, a copy, and where it comes from. Its
// changes are never applied: apply, update and the preview refuse it.
func TestCreateWorkspaceFrom(t *testing.T) {
	e := newJobsEnv(t)
	post := func(body string) (int, string) {
		t.Helper()
		resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(body)))
		return resp.StatusCode, raw
	}
	const machine = "con_0123456789abcdef"
	code, raw := post(`{"folder_ref":"fld_0123456789abcdef","machine":"` + machine + `"}`)
	var src store.Workspace
	if code != http.StatusCreated || json.Unmarshal([]byte(raw), &src) != nil {
		t.Fatalf("source: %d %s", code, raw)
	}
	code, raw = post(`{"folder_ref":"fld_1111111111111111","machine":"` + machine + `","sandbox":false}`)
	var direct store.Workspace
	if code != http.StatusCreated || json.Unmarshal([]byte(raw), &direct) != nil {
		t.Fatalf("direct: %d %s", code, raw)
	}

	body := `{"from":{"workspace":"` + src.ID + `","checkpoint":"base"},"idempotency_key":"f1"}`
	code, raw = post(body)
	var fork store.Workspace
	if code != http.StatusCreated || json.Unmarshal([]byte(raw), &fork) != nil {
		t.Fatalf("fork: %d %s", code, raw)
	}
	if fork.ID == src.ID || !fork.Sandbox || fork.FolderRef != src.FolderRef || fork.Machine != machine ||
		fork.From == nil || fork.From.Workspace != src.ID || fork.From.Checkpoint != "base" {
		t.Fatalf("fork record: %s", raw)
	}
	// The same key returns the same workspace; another checkpoint under it is refused.
	if code, raw = post(body); code != http.StatusCreated || !strings.Contains(raw, fork.ID) {
		t.Fatalf("retry: %d %s", code, raw)
	}
	if code, _ = post(`{"from":{"workspace":"` + src.ID + `","checkpoint":"t_other"},"idempotency_key":"f1"}`); code != http.StatusConflict {
		t.Fatalf("a reused key with other fields: %d, want 409", code)
	}
	// GET reads it back with from.
	resp, raw := do(t, e.req(t, "GET", "/api/v0/workspaces/"+fork.ID, nil))
	if resp.StatusCode != http.StatusOK || !strings.Contains(raw, `"from":{"workspace":"`+src.ID+`","checkpoint":"base"}`) {
		t.Fatalf("get fork: %d %s", resp.StatusCode, raw)
	}

	for name, c := range map[string]struct {
		body string
		want int
	}{
		"with a folder":       {`{"from":{"workspace":"` + src.ID + `","checkpoint":"base"},"folder_ref":"fld_0123456789abcdef"}`, 400},
		"bad checkpoint":      {`{"from":{"workspace":"` + src.ID + `","checkpoint":"../x"}}`, 400},
		"lock checkpoint":     {`{"from":{"workspace":"` + src.ID + `","checkpoint":"t1.lock"}}`, 400},
		"no checkpoint":       {`{"from":{"workspace":"` + src.ID + `"}}`, 400},
		"direct":              {`{"from":{"workspace":"` + src.ID + `","checkpoint":"base"},"sandbox":false}`, 400},
		"unknown workspace":   {`{"from":{"workspace":"ws_dead0000dead0000","checkpoint":"base"}}`, 404},
		"direct source":       {`{"from":{"workspace":"` + direct.ID + `","checkpoint":"base"}}`, 400},
		"another machine":     {`{"from":{"workspace":"` + src.ID + `","checkpoint":"base"},"machine":"con_ffffffffffffffff"}`, 400},
		"from a fork is fine": {`{"from":{"workspace":"` + fork.ID + `","checkpoint":"base"}}`, 201},
	} {
		if code, raw := post(c.body); code != c.want {
			t.Errorf("%s: %d %s, want %d", name, code, raw, c.want)
		}
	}

	for _, path := range []string{"/apply", "/update"} {
		resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces/"+fork.ID+path, nil))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(raw, forkNotApplied) {
			t.Errorf("%s of a fork: %d %s, want 400", path, resp.StatusCode, raw)
		}
	}
	resp, raw = do(t, e.req(t, "GET", "/api/v0/workspaces/"+fork.ID+"/changes", nil))
	var page WorkspaceChangesPage
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(raw), &page) != nil || page.Why != forkNotApplied || len(page.Files) != 0 {
		t.Fatalf("preview of a fork: %d %s", resp.StatusCode, raw)
	}
}
