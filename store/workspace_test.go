package store

import (
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
)

func ptrBool(b bool) *bool { return &b }

// started appends a turn.started with a real folder and reference, the way an
// existing (pre-workspace) session's events look.
func wsStarted(t *testing.T, s *Store, session, turn, folder, ref string, at time.Time) {
	t.Helper()
	mustAppend(t, s, event.Event{ID: turn, Kind: event.TurnStarted, Session: session, Turn: turn,
		Text: "hi", Harness: "claude", Folder: folder, FolderRef: ref, Time: at})
}

func sessionIn(t *testing.T, s *Store, id string) *Session {
	t.Helper()
	snap, err := s.Sessions(-1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range snap.Sessions {
		if snap.Sessions[i].ID == id {
			return &snap.Sessions[i]
		}
	}
	return nil
}

// MigrateWorkspaces writes one workspace.created per existing session, with the
// session's own id as its workspace id, the sandbox mode from the queue (a copy
// or worktree is a sandbox, a folder is direct, no queue row is direct), and
// the folder from the session's own events. It is idempotent.
func TestMigrateWorkspacesModeFolderAndIdempotent(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	wsStarted(t, s, "s_copy", "t_copy", "/src/app", "fld_1111111111111111", base)
	wsStarted(t, s, "s_folder", "t_folder", "/src/lib", "fld_2222222222222222", base.Add(time.Minute))
	wsStarted(t, s, "s_hook", "t_hook", "/src/web", "fld_3333333333333333", base.Add(2*time.Minute))

	// s_copy has a copy (sandbox), s_folder a direct folder, s_hook no queue
	// row at all (hook or gateway traffic).
	n, err := s.MigrateWorkspaces([]WorkspaceSeed{
		{Session: "s_copy", Sandbox: true},
		{Session: "s_folder", Sandbox: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("migrated %d, want 3 (every session, seeded or not)", n)
	}

	want := []struct {
		id      string
		sandbox bool
		folder  string
		ref     string
	}{
		{"s_copy", true, "/src/app", "fld_1111111111111111"},
		{"s_folder", false, "/src/lib", "fld_2222222222222222"},
		{"s_hook", false, "/src/web", "fld_3333333333333333"},
	}
	for _, w := range want {
		got, found, err := s.Workspace(ids.WorkspaceID(w.id))
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("%s: no workspace after migration", w.id)
		}
		if got.ID != w.id {
			t.Fatalf("%s: workspace id %q, want the session's own id", w.id, got.ID)
		}
		if got.Sandbox != w.sandbox {
			t.Fatalf("%s: sandbox %v, want %v", w.id, got.Sandbox, w.sandbox)
		}
		if got.Folder != w.folder || got.FolderRef != w.ref {
			t.Fatalf("%s: folder %q ref %q, want %q %q", w.id, got.Folder, got.FolderRef, w.folder, w.ref)
		}
	}

	// A re-run on the next start changes nothing.
	n, err = s.MigrateWorkspaces([]WorkspaceSeed{
		{Session: "s_copy", Sandbox: true},
		{Session: "s_folder", Sandbox: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("re-run migrated %d, want 0 (idempotent)", n)
	}
	ws, err := s.Workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 3 {
		t.Fatalf("Workspaces() returned %d, want 3", len(ws))
	}
}

// EnsureDirectWorkspace creates a session's direct workspace carrying the
// folder's machine, so a turn's place (folder AND machine) is the one
// workspace record's -- a multi-runner apply of a gateway or hook session's
// direct workspace routes to the runner that holds it, not an empty machine
// (his rule, 2026-11-21, finishing rule 3). It is idempotent on wc_<session>,
// so the first machine wins and a later turn changes nothing.
func TestEnsureDirectWorkspaceCarriesMachine(t *testing.T) {
	s := newStore(t)
	if err := s.EnsureDirectWorkspace("s_gw", "/src/app", "fld_1111111111111111", "con_2222222222222222"); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Workspace(ids.WorkspaceID("s_gw"))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no workspace after EnsureDirectWorkspace")
	}
	if got.Machine != "con_2222222222222222" {
		t.Fatalf("machine %q, want the folder's machine", got.Machine)
	}
	if got.Sandbox || got.Folder != "/src/app" || got.FolderRef != "fld_1111111111111111" {
		t.Fatalf("direct workspace folder %q ref %q sandbox %v, want /src/app fld_1111111111111111 false",
			got.Folder, got.FolderRef, got.Sandbox)
	}
	// A later turn of the same session keeps the first machine (idempotent): an
	// empty machine does not erase it.
	if err := s.EnsureDirectWorkspace("s_gw", "/src/app", "fld_1111111111111111", ""); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.Workspace(ids.WorkspaceID("s_gw"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Machine != "con_2222222222222222" {
		t.Fatalf("after re-run machine %q, want the first machine kept", got.Machine)
	}
	// An empty session is a no-op, not an error.
	if err := s.EnsureDirectWorkspace("", "/x", "", ""); err != nil {
		t.Fatalf("empty session: %v", err)
	}
}

// Store.Workspace reads head_turn from the workspace's turn.changed, applied_turn
// from its latest successful apply, and sets discarded_at when the latest action
// was a discard. An unknown id is not found.
func TestWorkspaceReaderState(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	wsStarted(t, s, "s1", "t1", "/src/app", "fld_aaaaaaaaaaaaaaaa", base)
	if _, err := s.MigrateWorkspaces([]WorkspaceSeed{{Session: "s1", Sandbox: true}}); err != nil {
		t.Fatal(err)
	}

	if _, found, err := s.Workspace("ws_nope"); err != nil || found {
		t.Fatalf("unknown workspace: found=%v err=%v", found, err)
	}

	// A turn with a changeset makes it the head.
	mustAppend(t, s, event.Event{ID: "c1", Kind: event.TurnChanged, Session: "s1", Turn: "t1",
		Changes: &event.TurnChanges{Isolated: true, Count: 1}, Changeset: "cs_t1", Time: base.Add(time.Minute)})
	if w, _, _ := s.Workspace("s1"); w.HeadTurn != "t1" {
		t.Fatalf("head_turn %q, want t1", w.HeadTurn)
	}
	// A turn whose changeset could not be kept (turn.changed with none) is not
	// appliable, so it does not become the head: the head is the turn an apply
	// goes up to, and the queue reads this same fold (store.Applied). Before
	// 0.6.6 the store said head and the queue said not appliable.
	wsStarted(t, s, "s1", "t1b", "/src/app", "fld_aaaaaaaaaaaaaaaa", base.Add(time.Minute))
	mustAppend(t, s, event.Event{ID: "c1b", Kind: event.TurnChanged, Session: "s1", Turn: "t1b",
		Changes: &event.TurnChanges{Isolated: true, Count: 1}, Time: base.Add(time.Minute)})
	if w, _, _ := s.Workspace("s1"); w.HeadTurn != "t1" {
		t.Fatalf("head_turn %q after a turn with no changeset, want t1", w.HeadTurn)
	}
	if w, _, _ := s.Workspace("s1"); w.AppliedTurn != "" || w.DiscardedAt != nil {
		t.Fatalf("before apply: applied %q discarded %v", w.AppliedTurn, w.DiscardedAt)
	}

	// An apply up to t1.
	mustAppend(t, s, event.Event{ID: "a1", Kind: event.WorkspaceApplied, Session: "s1", Turn: "t1",
		Workspace: "s1", Applied: &event.Applied{Turns: []string{"t1"}, Result: "applied"}, Time: base.Add(2 * time.Minute)})
	if w, _, _ := s.Workspace("s1"); w.AppliedTurn != "t1" || w.DiscardedAt != nil {
		t.Fatalf("after apply: applied %q discarded %v", w.AppliedTurn, w.DiscardedAt)
	}

	// A later discard drops the newer changes and sets discarded_at.
	wsStarted(t, s, "s1", "t2", "/src/app", "fld_aaaaaaaaaaaaaaaa", base.Add(3*time.Minute))
	mustAppend(t, s, event.Event{ID: "c2", Kind: event.TurnChanged, Session: "s1", Turn: "t2",
		Changes: &event.TurnChanges{Isolated: true, Count: 1}, Changeset: "cs_t2", Time: base.Add(4 * time.Minute)})
	mustAppend(t, s, event.Event{ID: "d1", Kind: event.WorkspaceDiscarded, Session: "s1", Workspace: "s1",
		Discarded: &event.Discarded{ResetTo: "cs_applied", DroppedTurns: []string{"t2"}}, Time: base.Add(5 * time.Minute)})
	w, _, _ := s.Workspace("s1")
	if w.AppliedTurn != "t1" {
		t.Fatalf("after discard: applied_turn %q, want t1 (the last successful apply)", w.AppliedTurn)
	}
	if w.DiscardedAt == nil || !w.DiscardedAt.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("after discard: discarded_at %v, want the discard time", w.DiscardedAt)
	}
}

// The snapshot folds each session's workspace and its sandbox mode. A migrated
// session's workspace is its own id; a session whose turn.started names a
// workspace (an explicitly created one, the shape of a turn from W2 on) reports
// that workspace's mode, and it is reported while the turn is only queued or
// running, before any changeset.
func TestSnapshotFoldsWorkspaceAndSandbox(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	wsStarted(t, s, "s_sb", "t_sb", "/src/app", "fld_1111111111111111", base)
	wsStarted(t, s, "s_dir", "t_dir", "/src/lib", "fld_2222222222222222", base.Add(time.Minute))
	if _, err := s.MigrateWorkspaces([]WorkspaceSeed{{Session: "s_sb", Sandbox: true}, {Session: "s_dir", Sandbox: false}}); err != nil {
		t.Fatal(err)
	}

	if x := sessionIn(t, s, "s_sb"); x == nil || x.Workspace != "s_sb" || !x.Sandbox {
		t.Fatalf("sandbox session: %+v", x)
	}
	if x := sessionIn(t, s, "s_dir"); x == nil || x.Workspace != "s_dir" || x.Sandbox {
		t.Fatalf("direct session: %+v", x)
	}

	// An explicitly created workspace (W2 shape): workspace.created first, then
	// a turn.started that names it. No turn.changed yet -- the turn is still
	// running -- and the session already reports the workspace's sandbox mode.
	ok, err := s.AppendOnce(event.Event{ID: "wc_ws_explicit", Kind: event.WorkspaceCreated, Workspace: "ws_explicit",
		Folder: "/src/new", FolderRef: "fld_4444444444444444", Sandbox: ptrBool(true), Time: base.Add(2 * time.Minute)})
	if err != nil || !ok {
		t.Fatalf("workspace.created: ok=%v err=%v", ok, err)
	}
	mustAppend(t, s, event.Event{ID: "t_new", Kind: event.TurnStarted, Session: "s_new", Turn: "t_new",
		Text: "hi", Harness: "claude", Folder: "/src/new", FolderRef: "fld_4444444444444444",
		Workspace: "ws_explicit", Time: base.Add(3 * time.Minute)})
	if x := sessionIn(t, s, "s_new"); x == nil || x.Workspace != "ws_explicit" || !x.Sandbox {
		t.Fatalf("explicit-workspace session while running: %+v", x)
	}
}

// A session that already names a workspace of its own (an explicitly created
// one, whose workspace.created is keyed on its ws_ id, not wc_<session>) is not
// migrated: the migration must not write a second, phantom workspace.created
// keyed on the session's id, which the append-only log would then keep forever.
func TestMigrateSkipsSessionWithExplicitWorkspace(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 21, 16, 0, 0, 0, time.UTC)
	// A W2-shape session: workspace.created under its own id, then a turn.
	ok, err := s.AppendOnce(event.Event{ID: "wc_ws_exp", Kind: event.WorkspaceCreated, Workspace: "ws_exp",
		Folder: "/src/app", FolderRef: "fld_5555555555555555", Sandbox: ptrBool(true), Time: base})
	if err != nil || !ok {
		t.Fatalf("workspace.created: ok=%v err=%v", ok, err)
	}
	mustAppend(t, s, event.Event{ID: "t_exp", Kind: event.TurnStarted, Session: "s_exp", Turn: "t_exp",
		Text: "hi", Harness: "claude", Folder: "/src/app", FolderRef: "fld_5555555555555555",
		Workspace: "ws_exp", Time: base.Add(time.Minute)})

	// The migration runs with a (stale) queue seed for the session too.
	n, err := s.MigrateWorkspaces([]WorkspaceSeed{{Session: "s_exp", Sandbox: true}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("migrated %d, want 0 (the session already has a workspace)", n)
	}
	if _, found, _ := s.Workspace("s_exp"); found {
		t.Fatal("a phantom workspace keyed on the session id was created")
	}
	ws, err := s.Workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].ID != "ws_exp" {
		t.Fatalf("workspaces %+v, want just ws_exp", ws)
	}
}

// WorkspaceOf resolves a session's workspace: the one its session.created or
// turn.started names, or the session's own id for a migrated session, and not
// found for an unknown session.
func TestWorkspaceOf(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)
	// A session created in an explicit workspace.
	if _, err := s.AppendOnce(event.Event{ID: "sc_s_a", Kind: event.SessionCreated, Session: "s_a",
		Workspace: "ws_a", Time: base}); err != nil {
		t.Fatal(err)
	}
	// A migrated session: a turn with no workspace field.
	wsStarted(t, s, "s_b", "t_b", "/src/app", "fld_8888888888888888", base.Add(time.Minute))

	for _, c := range []struct {
		session, want string
		found         bool
	}{
		{"s_a", "ws_a", true},    // from session.created
		{"s_b", "s_b", true},     // migrated: its own id
		{"s_unknown", "", false}, // not in the record
	} {
		got, found, err := s.WorkspaceOf(ids.SessionID(c.session))
		if err != nil {
			t.Fatal(err)
		}
		if found != c.found || string(got) != c.want {
			t.Errorf("WorkspaceOf(%q) = %q,%v, want %q,%v", c.session, got, found, c.want, c.found)
		}
	}
}

// A session created by POST /api/v0/sessions (a session.created naming a
// workspace) is left alone by the migration even before its first turn, so a
// restart writes no phantom workspace keyed on the session's id.
func TestMigrateSkipsSessionCreatedBeforeFirstTurn(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 21, 18, 30, 0, 0, time.UTC)
	ok, err := s.AppendOnce(event.Event{ID: "wc_ws_exp2", Kind: event.WorkspaceCreated, Workspace: "ws_exp2",
		FolderRef: "fld_7777777777777777", Sandbox: ptrBool(true), Time: base})
	if err != nil || !ok {
		t.Fatalf("workspace.created: %v %v", ok, err)
	}
	// A session created in it, with no turn yet.
	ok, err = s.AppendOnce(event.Event{ID: "sc_s_exp2", Kind: event.SessionCreated, Session: "s_exp2",
		Workspace: "ws_exp2", Title: "Fix it", Time: base.Add(time.Minute)})
	if err != nil || !ok {
		t.Fatalf("session.created: %v %v", ok, err)
	}
	if n, err := s.MigrateWorkspaces(nil); err != nil || n != 0 {
		t.Fatalf("migrate: n=%d err=%v, want 0 (the session already has a workspace)", n, err)
	}
	if _, found, _ := s.Workspace("s_exp2"); found {
		t.Fatal("a phantom workspace keyed on the session id was created")
	}
}

// Renaming a session that was created with a title records that title as the
// old one, not an empty string or the first message.
func TestRenameKeepsCreatedTitleAsOld(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 21, 19, 0, 0, 0, time.UTC)
	if _, err := s.AppendOnce(event.Event{ID: "sc_s_t", Kind: event.SessionCreated, Session: "s_t",
		Workspace: "ws_t", Title: "First name", Time: base}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Rename(event.Event{ID: "r1", Session: "s_t", Title: "Second name", Time: base.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if out.OldTitle != "First name" {
		t.Fatalf("old_title %q, want the session.created title", out.OldTitle)
	}
}

// The migration records the authoritative folder, reference and machine from
// the seed (the queue and the folder registry), never the session's event
// folder -- which for an isolated session is the runner's scratch copy path,
// not the person's folder.
func TestMigratePopulatesMachineAndRealFolder(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 21, 17, 0, 0, 0, time.UTC)
	// An isolated session: its event folder is the runner's scratch copy.
	wsStarted(t, s, "s_iso", "t_iso", "/home/.agentbus/sessions/s_iso", "fld_6666666666666666", base)
	if _, err := s.MigrateWorkspaces([]WorkspaceSeed{{
		Session: "s_iso", Sandbox: true, Machine: "con_6666666666666666",
		FolderRef: "fld_6666666666666666", Folder: "/Users/alice/src/app",
	}}); err != nil {
		t.Fatal(err)
	}
	w, found, err := s.Workspace("s_iso")
	if err != nil || !found {
		t.Fatalf("workspace: found=%v err=%v", found, err)
	}
	if w.Folder != "/Users/alice/src/app" {
		t.Fatalf("folder %q, want the person's folder, not the scratch copy path", w.Folder)
	}
	if w.Machine != "con_6666666666666666" {
		t.Fatalf("machine %q, want the seed's connector", w.Machine)
	}
	if w.FolderRef != "fld_6666666666666666" {
		t.Fatalf("folder_ref %q", w.FolderRef)
	}
}
