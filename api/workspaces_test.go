package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/staysup/agentbus/store"
)

// POST /api/v0/workspaces creates a workspace from a folder reference, with
// sandbox true by default; POST /api/v0/sessions creates a session in it; and
// the session then lists with its workspace and mode folded in. An idempotency
// key returns the same workspace, and a session in an unknown workspace is 404.
func TestCreateWorkspaceAndSession(t *testing.T) {
	e := newEnv(t)

	// A sandbox workspace (the default) by folder reference.
	resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(`{"folder_ref":"fld_0123456789abcdef"}`)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", resp.StatusCode, raw)
	}
	var ws store.Workspace
	if json.Unmarshal([]byte(raw), &ws) != nil || !strings.HasPrefix(ws.ID, "ws_") || !ws.Sandbox || ws.FolderRef != "fld_0123456789abcdef" {
		t.Fatalf("workspace: %s", raw)
	}

	// A direct workspace: sandbox false.
	resp, raw = do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(`{"folder_ref":"fld_1111111111111111","sandbox":false}`)))
	var direct store.Workspace
	if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &direct) != nil || direct.Sandbox {
		t.Fatalf("direct workspace: %d %s", resp.StatusCode, raw)
	}

	// A session in the sandbox workspace.
	resp, raw = do(t, e.req(t, "POST", "/api/v0/sessions", strings.NewReader(`{"workspace":"`+ws.ID+`","title":"Fix login"}`)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %s", resp.StatusCode, raw)
	}
	var sc SessionCreated
	if json.Unmarshal([]byte(raw), &sc) != nil || sc.Session == "" || sc.Workspace != ws.ID || sc.Title != "Fix login" {
		t.Fatalf("session: %s", raw)
	}

	// The session lists with its workspace and sandbox mode folded in, before
	// any turn has run.
	_, raw = do(t, e.req(t, "GET", "/api/v0/sessions", nil))
	var snap store.Snapshot
	if json.Unmarshal([]byte(raw), &snap) != nil {
		t.Fatalf("sessions: %s", raw)
	}
	var found *store.Session
	for i := range snap.Sessions {
		if snap.Sessions[i].ID == sc.Session {
			found = &snap.Sessions[i]
		}
	}
	if found == nil || found.Workspace != ws.ID || !found.Sandbox || found.Title != "Fix login" {
		t.Fatalf("session in list: %+v", found)
	}

	// GET /api/v0/workspaces/{id} reads it back.
	resp, raw = do(t, e.req(t, "GET", "/api/v0/workspaces/"+ws.ID, nil))
	if resp.StatusCode != http.StatusOK || !strings.Contains(raw, ws.ID) {
		t.Fatalf("get workspace: %d %s", resp.StatusCode, raw)
	}

	// A session in an unknown workspace is refused.
	resp, raw = do(t, e.req(t, "POST", "/api/v0/sessions", strings.NewReader(`{"workspace":"ws_dead0000dead0000"}`)))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("session in unknown workspace: %d %s, want 404", resp.StatusCode, raw)
	}

	// An idempotency key returns the same workspace, makes no second one.
	body := `{"folder_ref":"fld_2222222222222222","idempotency_key":"k1"}`
	_, raw = do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(body)))
	var first store.Workspace
	_ = json.Unmarshal([]byte(raw), &first)
	_, raw = do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(body)))
	var again store.Workspace
	_ = json.Unmarshal([]byte(raw), &again)
	if first.ID == "" || first.ID != again.ID {
		t.Fatalf("idempotent create: %q then %q", first.ID, again.ID)
	}
}

// An idempotency key reused with other fields is refused, not silently given
// the first object; and a session title with a line break is refused.
func TestWorkspaceSessionIdempotencyMismatchAndTitle(t *testing.T) {
	e := newEnv(t)
	first := `{"folder_ref":"fld_0123456789abcdef","idempotency_key":"m1"}`
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(first))); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: %d %s", resp.StatusCode, raw)
	}
	// Same key, different sandbox mode: refused.
	mismatch := `{"folder_ref":"fld_0123456789abcdef","sandbox":false,"idempotency_key":"m1"}`
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(mismatch))); resp.StatusCode != http.StatusConflict {
		t.Fatalf("reused key, other fields: %d %s, want 409", resp.StatusCode, raw)
	}

	// Create a workspace and reject a session title with a newline.
	_, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(`{"folder_ref":"fld_1111111111111111"}`)))
	var ws store.Workspace
	_ = json.Unmarshal([]byte(raw), &ws)
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/sessions", strings.NewReader(`{"workspace":"`+ws.ID+`","title":"line one\nline two"}`))); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("session with a multi-line title: %d %s, want 400", resp.StatusCode, raw)
	}
}

// POST /api/v0/workspaces with a typed folder needs a queue (the folder's
// machine registers it): without one it answers 404, never panics.
func TestCreateWorkspaceNoQueueFolder(t *testing.T) {
	e := newEnv(t) // no queue
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(`{"folder":"/tmp"}`))); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("typed folder with no queue: %d %s, want 404", resp.StatusCode, raw)
	}
}

// A session title is normalized one way (shared with PATCH rename): a
// whitespace-only title is refused, and surrounding spaces are trimmed.
func TestCreateSessionTitleNormalized(t *testing.T) {
	e := newEnv(t)
	_, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(`{"folder_ref":"fld_0123456789abcdef"}`)))
	var ws store.Workspace
	_ = json.Unmarshal([]byte(raw), &ws)

	// Whitespace-only: refused.
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/sessions", strings.NewReader(`{"workspace":"`+ws.ID+`","title":"   "}`))); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("whitespace-only title: %d %s, want 400", resp.StatusCode, raw)
	}
	// Surrounding spaces trimmed.
	resp, raw := do(t, e.req(t, "POST", "/api/v0/sessions", strings.NewReader(`{"workspace":"`+ws.ID+`","title":"  Fix login  "}`)))
	var sc SessionCreated
	if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &sc) != nil || sc.Title != "Fix login" {
		t.Fatalf("padded title: %d %s, want trimmed to \"Fix login\"", resp.StatusCode, raw)
	}
}

// Every store-backed create route documents the 409 the shared idempotency
// guard can return, so a returnable status is not left undocumented.
func TestCreateRoutesDocumentConflict(t *testing.T) {
	doc := loadSpec(t)
	for _, p := range []string{"/api/v0/workspaces", "/api/v0/sessions"} {
		op := doc.Paths.Find(p).Post
		if op == nil || op.Responses.Status(http.StatusConflict) == nil {
			t.Errorf("POST %s does not document 409", p)
		}
	}
}

// A workspace needs exactly one of folder or folder_ref, and a bad folder_ref
// shape is refused.
func TestCreateWorkspaceValidation(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{}`, http.StatusBadRequest},
		{`{"folder":"/tmp","folder_ref":"fld_0123456789abcdef"}`, http.StatusBadRequest},
		{`{"folder_ref":"not-a-ref"}`, http.StatusBadRequest},
		{`{"folder_ref":"fld_0123456789abcdef","machine":"not-a-con"}`, http.StatusBadRequest},
		// A base: a branch, tag or commit shape, with a sandbox; a remote
		// only with a base.
		{`{"folder_ref":"fld_0123456789abcdef","remote":"origin"}`, http.StatusBadRequest},
		{`{"folder_ref":"fld_0123456789abcdef","base":"main","sandbox":false}`, http.StatusBadRequest},
		{`{"folder_ref":"fld_0123456789abcdef","base":"main","remote":"-x"}`, http.StatusBadRequest},
		{`{"folder_ref":"fld_0123456789abcdef","base":"main","remote":"a/b"}`, http.StatusBadRequest},
		// A base names one folder's repo: a workspace with several takes none.
		{`{"folders":[{"folder_ref":"fld_0123456789abcdef"},{"folder_ref":"fld_fedcba9876543210"}],"base":"main"}`, http.StatusBadRequest},
	} {
		resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(tc.body)))
		if resp.StatusCode != tc.want {
			t.Errorf("POST /workspaces %s: %d %s, want %d", tc.body, resp.StatusCode, raw, tc.want)
		}
	}
}

// A base is refused when its shape could read as an option, a revision
// expression or a refspec, each with the one fixed sentence.
func TestCreateWorkspaceBaseShape(t *testing.T) {
	e := newEnv(t)
	for _, base := range []string{"-x", "--upload-pack=x", ".hidden", "/abs", "a..b", "a//b", "a/", "a.", "a.lock", "a/.b",
		"HEAD~1", "main^", "a:b", "a b", "@{1}", "a*", strings.Repeat("a", 256)} {
		body, _ := json.Marshal(map[string]string{"folder_ref": "fld_0123456789abcdef", "base": base})
		resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(string(body))))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(raw, badBase) {
			t.Errorf("base %q: %d %s, want 400 with the base sentence", base, resp.StatusCode, raw)
		}
	}
	for _, good := range []string{"main", "feature/login", "v1.2.3", "release-2026_11", "0123abc"} {
		if msg := checkBase(good, "", true); msg != "" {
			t.Errorf("base %q refused: %s", good, msg)
		}
	}
	if msg := checkBase("main", "origin", true); msg != "" {
		t.Errorf("remote origin refused: %s", msg)
	}
}

// POST /api/v0/folders/branches checks its body before any job: one of
// folder or folder_ref, their shapes, the machine's and the remote's.
func TestListBranchesValidation(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{`{}`, `[]`, `{"folder":"/tmp","folder_ref":"fld_0123456789abcdef"}`, `{"folder":"rel"}`,
		`{"folder_ref":"nope"}`, `{"folder_ref":"fld_0123456789abcdef","machine":"x"}`,
		`{"folder_ref":"fld_0123456789abcdef","remote":"-x"}`, `{"folder_ref":"fld_0123456789abcdef","remote":"a/b"}`,
		`{"folder_ref":"fld_0123456789abcdef","extra":1}`} {
		resp, raw := do(t, e.req(t, "POST", "/api/v0/folders/branches", strings.NewReader(body)))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /folders/branches %s: %d %s, want 400", body, resp.StatusCode, raw)
		}
	}
}
