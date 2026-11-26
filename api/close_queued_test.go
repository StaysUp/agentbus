package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Close answers at once with the queued close, as POST /api/v0/turns does
// (0.6.7). It waited up to TurnWait (30 seconds by default) for the close to
// run, so a caller closing a dozen workspaces waited minutes.
func TestCloseAnswersQueuedAtOnce(t *testing.T) {
	e := newJobsEnv(t)
	e.api.cfg.TurnWait = time.Hour // a wait would hold the reply past the deadline below
	e.q.ReadAppliedWith(e.store.Applied)
	ref, machine := e.folderRef(t, "/tmp")
	ws := e.makeWorkspace(t, ref, machine, true)
	// A runner of the folder's machine is seen, so a close can be queued for
	// it. Nothing takes the close.
	r := e.req(t, "POST", "/api/v0/runner/register", strings.NewReader(
		`{"name":"r","capacity":1,"protocol":5,"capabilities":{"connector":"`+machine+`"}}`))
	r.Header.Set("Authorization", "Bearer "+enrollTok)
	if resp, raw := do(t, r); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(e.req(t, "POST", "/api/v0/workspaces/"+ws+"/close", nil).WithContext(ctx))
	if err != nil {
		t.Fatalf("close: %v, want an answer at once", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var st CloseState
	if err := json.Unmarshal(raw, &st); err != nil || resp.StatusCode != http.StatusAccepted || st.State != "queued" {
		t.Fatalf("close: %d %s, want 202 queued", resp.StatusCode, raw)
	}
	// A second close while the first waits is refused, as before.
	if resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces/"+ws+"/close", nil)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second close: %d %s, want 409", resp.StatusCode, raw)
	}
}
