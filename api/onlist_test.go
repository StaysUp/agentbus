package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
)

// GET /api/v0/ons lists the ons waiting to react, the ones the caller may
// remove (O3, 2026-11-26): every turn's on for the main token; for a turn
// token, those its own turn registered and those that deliver to a session in
// its reach. Finding the ons that deliver to a session took a read of jobs.db.
// Control's own on over the record is listed to no one.
func TestListOns(t *testing.T) {
	grants := map[string]run.Grant{"abr_copy": {Session: "s_asker", Turn: "t_copy", Root: "s_asker"}}
	e, _ := rulesEnv(t, grants)
	seedReach(t, e.store)
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	e.api.cfg.Queue = q
	register := func(subject, session string, maker queue.Maker) string {
		t.Helper()
		on, err := q.RegisterOn(queue.DefaultTeam, queue.OnTurnCompleted, subject, session, "", maker)
		if err != nil {
			t.Fatal(err)
		}
		return on.ID
	}
	outside := register("t_x", "s_other", queue.Maker{})                               // the owner's, outside the agent's reach
	inReach := register("t_y", "s_mine", queue.Maker{})                                // the owner's, in its reach
	own := register("t_z", "s_other", queue.Maker{Session: "s_asker", Turn: "t_copy"}) // the agent's own, delivering outside
	gone := register("t_w", "s_mine", queue.Maker{})                                   // removed: not listed
	if err := q.RemoveOn(queue.DefaultTeam, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RegisterRecordOn(queue.DefaultTeam, "workspace.applied", queue.Action{Queue: queue.KindPutAway}, "builtin/put_away/workspace.applied", 0); err != nil {
		t.Fatal(err)
	}
	list := func(tok, query string, want ...string) {
		t.Helper()
		code, out := callAs(t, e, tok, "GET", "/api/v0/ons"+query, "")
		var l OnList
		if code != http.StatusOK || json.Unmarshal([]byte(out), &l) != nil {
			t.Fatalf("%s GET /api/v0/ons%s: %d %s", tok, query, code, out)
		}
		var got []string
		for _, on := range l.Ons {
			got = append(got, on.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s GET /api/v0/ons%s: %v, want %v (%s)", tok, query, got, want, out)
		}
	}
	list(token, "", outside, inReach, own)
	list(token, "?session=s_mine", inReach)
	list("abr_copy", "", inReach, own)
	list("abr_copy", "?session=s_other", own)
	list("abr_copy", "?session=s_e")
	// Who registered it is shown; the owner's names no turn.
	code, out := callAs(t, e, "abr_copy", "GET", "/api/v0/ons?session=s_other", "")
	var l OnList
	if code != http.StatusOK || json.Unmarshal([]byte(out), &l) != nil || len(l.Ons) != 1 || l.Ons[0].By == nil ||
		l.Ons[0].By.Turn != "t_copy" || l.Ons[0].By.Session != "s_asker" || l.Ons[0].Turn != "t_z" {
		t.Errorf("the agent's own on: %d %s", code, out)
	}
	for _, query := range []string{"?session=", "?session=s_mine&session=s_other"} {
		if code, out := callAs(t, e, "abr_copy", "GET", "/api/v0/ons"+query, ""); code != http.StatusBadRequest {
			t.Errorf("GET /api/v0/ons%s: %d %s, want 400", query, code, out)
		}
	}
	// The list is what DELETE allows: an on delivering in reach goes, one
	// outside it, another's, does not.
	if code, out := callAs(t, e, "abr_copy", "DELETE", "/api/v0/ons/"+outside, ""); code != http.StatusForbidden {
		t.Errorf("remove an on outside the reach: %d %s, want 403", code, out)
	}
	if code, out := callAs(t, e, "abr_copy", "DELETE", "/api/v0/ons/"+inReach, ""); code != http.StatusNoContent {
		t.Errorf("remove an on delivering in reach: %d %s, want 204", code, out)
	}
	list("abr_copy", "", own)
}
