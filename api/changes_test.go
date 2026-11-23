package api

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// A preview over the metadata bound is reported plainly, not silently: the
// route reuses the stored changes.previewed event, serves count as the whole
// number of files while files holds only a page, says truncated, and pages
// the rest pinned to the same event.
func TestSessionChangesReportsTruncatedAndPagesPinned(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	// A stored preview where more files exist than it kept (too many to send).
	files := make([]event.PreviewedFile, 0, 3)
	for i := 0; i < 3; i++ {
		files = append(files, event.PreviewedFile{Path: fmt.Sprintf("f%02d.txt", i), Change: "added"})
	}
	sb := true
	wc := event.Event{ID: "wc_ws1", Kind: event.WorkspaceCreated, Time: now, Workspace: "ws1",
		FolderRef: "fld_0123456789abcdef", Machine: "con_aaaaaaaaaaaaaaaa", Sandbox: &sb}
	if _, _, err := e.store.Append(wc); err != nil {
		t.Fatal(err)
	}
	ev := event.Event{ID: "evt_p", Kind: event.ChangesPreviewed, Time: now, Workspace: "ws1",
		Previewed: &event.Previewed{Turn: "t_1", Count: 5000, Truncated: true, Files: files}}
	if _, _, err := e.store.Append(ev); err != nil {
		t.Fatal(err)
	}

	// The route reuses the stored preview (no fresh) and reports it plainly.
	var p WorkspaceChangesPage
	if getJSON(t, e, "/api/v0/workspaces/ws1/changes?limit=2", &p) != http.StatusOK {
		t.Fatal("changes")
	}
	if p.Count != 5000 || !p.Truncated || len(p.Files) != 2 || p.Event != "evt_p" || p.PreviewedAt == nil || p.NextBefore == nil {
		t.Fatalf("first page: %+v", p)
	}
	// count minus the files a reader has seen is how many are left out: plain.
	seen := len(p.Files)

	// Page the rest, pinned to the same event so the snapshot is stable.
	var p2 WorkspaceChangesPage
	path := "/api/v0/workspaces/ws1/changes?limit=2&event=evt_p&before=" + strconv.FormatInt(*p.NextBefore, 10)
	if getJSON(t, e, path, &p2) != http.StatusOK {
		t.Fatal("second page")
	}
	if p2.Count != 5000 || !p2.Truncated || p2.Event != "evt_p" || p2.NextBefore != nil {
		t.Fatalf("second page: %+v", p2)
	}
	seen += len(p2.Files)
	if seen != 3 {
		t.Fatalf("paged %d of the %d kept files", seen, len(files))
	}
}
