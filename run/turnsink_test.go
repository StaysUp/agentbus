package run

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

type nopSink struct{}

func (nopSink) Emit(event.Event) {}

func turnSinkStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// An API or asked session already owns its workspace (POST /api/v0/sessions
// wrote its session.created naming it). Its agent's model calls reach the
// TurnSink as a turn.started carrying no workspace, and that must NOT mint a
// second, phantom direct workspace keyed on the session id: the one-workspace-
// per-session invariant would otherwise be a lie the moment an API turn runs.
func TestTurnSinkNoPhantomWorkspaceForPlacedSession(t *testing.T) {
	st := turnSinkStore(t)
	now := time.Now().UTC()
	sb := true
	if _, _, err := st.Append(event.Event{ID: "wc_ws_1", Kind: event.WorkspaceCreated, Time: now,
		Workspace: "ws_1", Folder: "/src/app", FolderRef: "fld_1111111111111111", Sandbox: &sb}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Append(event.Event{ID: "sc_s1", Kind: event.SessionCreated, Time: now,
		Session: "s1", Workspace: "ws_1"}); err != nil {
		t.Fatal(err)
	}

	sink := TurnSinkMachine(st, nopSink{}, func() string { return "con_2222222222222222" })
	// The agent's model call reaches the sink as a turn.started with no workspace
	// (the gateway does not know it).
	sink.Emit(event.Event{ID: "t1", Kind: event.TurnStarted, Session: "s1", Turn: "t1", Time: now})

	ws, err := st.Workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].ID != "ws_1" || !ws[0].Sandbox {
		t.Fatalf("a placed session's gateway turn minted a phantom workspace: %+v", ws)
	}
}

// A hook or gateway session has no session.created, so it has no workspace of
// its own: the TurnSink is where its direct workspace is born, carrying the
// folder's machine (rule 3: one source for where a turn runs).
func TestTurnSinkCreatesDirectWorkspaceForHookSession(t *testing.T) {
	st := turnSinkStore(t)
	now := time.Now().UTC()
	sink := TurnSinkMachine(st, nopSink{}, func() string { return "con_2222222222222222" })
	sink.Emit(event.Event{ID: "h1", Kind: event.TurnStarted, Session: "s_hook", Turn: "h1", Time: now})

	ws, err := st.Workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].ID != "s_hook" || ws[0].Sandbox {
		t.Fatalf("hook session got no direct workspace, or the wrong one: %+v", ws)
	}
	if ws[0].Machine != "con_2222222222222222" {
		t.Fatalf("direct workspace machine %q, want the folder's machine", ws[0].Machine)
	}
}
