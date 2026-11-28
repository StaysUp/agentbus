//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// The P1 that replaced the close mechanism: POST /workspaces/{id}/close used to
// answer "already closed" from the record's closed field, which turns false only
// once a turn STARTS. With a turn queued and not started, the record still said
// closed, so the close was not queued, and the turn then rebuilt the copy. The
// close now always joins the workspace lane. Even while the record says
// closed, it must queue behind the waiting turn.
func TestCloseQueuesWhileTheRecordSaysClosed(t *testing.T) {
	e := newJobsEnv(t)
	e.q.ReadAppliedWith(e.store.Applied)
	ref, machine := e.folderRef(t, "/tmp")
	ws := e.makeWorkspace(t, ref, machine, true)
	sess := e.makeSession(t, ws)

	// A runner of the folder's machine is seen, so a close can be queued for it.
	r := e.req(t, "POST", "/api/v0/runner/register", strings.NewReader(
		`{"name":"r","capacity":1,"protocol":6,"capabilities":{"connector":"`+machine+`"}}`))
	r.Header.Set("Authorization", "Bearer "+enrollTok)
	if resp, raw := do(t, r); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, raw)
	}

	// The record says closed: a workspace.closed, and no turn started since.
	if _, _, err := e.store.Append(event.Event{ID: "evt_wx_test", Kind: event.WorkspaceClosed,
		Workspace: ws, Time: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// A turn is queued; nothing has taken it.
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/turns",
		strings.NewReader(`{"session":"`+sess+`","agent":"claude","prompt":"hi"}`))); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /turns: %d %s", resp.StatusCode, raw)
	}
	var w struct {
		Closed *time.Time `json:"closed"`
	}
	if code := getJSON(t, e, "/api/v0/workspaces/"+ws, &w); code != 200 || w.Closed == nil {
		t.Fatalf("the record should call the workspace closed while the turn waits: %d closed=%v", code, w.Closed)
	}

	resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces/"+ws+"/close", nil))
	var st CloseState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("close: %d %s", resp.StatusCode, raw)
	}
	if st.Already {
		t.Fatalf("close answered already closed from the record while a turn waits: %d %s", resp.StatusCode, raw)
	}
	if resp.StatusCode != http.StatusAccepted || st.State != "queued" {
		t.Fatalf("close: %d %s, want 202 queued behind the waiting turn", resp.StatusCode, raw)
	}
}
