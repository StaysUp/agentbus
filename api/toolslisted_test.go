package api

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

// toolsListedOf reads the store's tools.listed events of session.
func toolsListedOf(t *testing.T, e *env, session string) []event.Event {
	t.Helper()
	f := store.Filter{Session: session, Only: session != ""}
	recs, err := e.store.After(0, f, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Event
	for _, r := range recs {
		if r.Kind != event.ToolsListed {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal(r.Data, &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// A turn's first tools/list stores one tools.listed: the server, the names it
// served, and the harness's own tools hidden, with why. Later lists of the
// same turn store nothing more; another turn gets its own.
func TestToolsListedOncePerTurn(t *testing.T) {
	const claudeTok, otherTok = "turn-tok-claude-0123456789abcdef", "turn-tok-other-0123456789abcdef"
	e := mcpEnvGrants(t, true, map[string]run.Grant{
		claudeTok: {Session: "s1", Turn: "t_1", Root: "s1", Harness: "claude"},
		otherTok:  {Session: "s1", Turn: "t_2", Root: "s1", Harness: "claude", ReadOnly: true},
	})
	served := toolNames(t, e, claudeTok)
	toolNames(t, e, claudeTok)
	toolNames(t, e, claudeTok)
	got := toolsListedOf(t, e, "s1")
	if len(got) != 1 {
		t.Fatalf("tools.listed after three lists: %d, want 1: %+v", len(got), got)
	}
	ev := got[0]
	if ev.Turn != "t_1" || ev.Server != "agentbus" || !slices.Equal(ev.Tools, served) {
		t.Errorf("tools.listed = turn %q server %q tools %v, want t_1, agentbus, %v", ev.Turn, ev.Server, ev.Tools, served)
	}
	var want []event.HiddenTool
	for _, n := range []string{"Task", "Agent", "ListAgents", "SendMessage"} {
		want = append(want, event.HiddenTool{Name: n, Why: "replaced by AgentBus tools"})
	}
	if !slices.Equal(ev.ToolsHidden, want) {
		t.Errorf("tools_hidden = %v, want %v", ev.ToolsHidden, want)
	}
	if ev.Vendor != "" || ev.Exchange != "" || ev.ID == "" || ev.Time.IsZero() {
		t.Errorf("tools.listed is the API's own event: %+v", ev)
	}

	// Another turn's list is its own event, with its own (read-only) tools.
	ro := toolNames(t, e, otherTok)
	got = toolsListedOf(t, e, "s1")
	if len(got) != 2 || got[1].Turn != "t_2" || !slices.Equal(got[1].Tools, ro) {
		t.Fatalf("second turn: %+v, want t_2 with %v", got, ro)
	}
}

// The main token lists tools for no turn, so nothing is stored.
func TestToolsListedNotForTheMainToken(t *testing.T) {
	e := mcpEnv(t)
	toolNames(t, e, token)
	if got := toolsListedOf(t, e, ""); len(got) != 0 {
		t.Errorf("main token stored %+v", got)
	}
}

// A next turn that names no agent has no harness on its grant: the session's
// latest agent is its harness. codex hides none of its own tools.
func TestToolsListedHarnessFromTheSession(t *testing.T) {
	const tok = "turn-tok-next-0123456789abcdef"
	e := mcpEnvGrants(t, true, map[string]run.Grant{tok: {Session: "s2", Turn: "t_9", Root: "s2"}})
	if _, _, err := e.store.Append(event.Event{ID: "evt_open", Kind: event.SessionOpened, Time: time.Now().UTC(),
		Session: "s2", Turn: "t_8", Hold: "h1", Harness: "codex"}); err != nil {
		t.Fatal(err)
	}
	toolNames(t, e, tok)
	got := toolsListedOf(t, e, "s2")
	if len(got) != 1 || got[0].Turn != "t_9" || len(got[0].ToolsHidden) != 0 || len(got[0].Tools) == 0 {
		t.Fatalf("codex turn: %+v, want one tools.listed with tools and none hidden", got)
	}

	// The same session on claude, by the latest agent: its tools hidden.
	const tok2 = "turn-tok-next2-0123456789abcdef"
	e2 := mcpEnvGrants(t, true, map[string]run.Grant{tok2: {Session: "s3", Turn: "t_2", Root: "s3"}})
	for i, h := range []string{"codex", "claude"} {
		if _, _, err := e2.store.Append(event.Event{ID: "evt_o" + h, Kind: event.SessionOpened, Time: time.Now().UTC(),
			Session: "s3", Turn: "t_" + string(rune('a'+i)), Hold: "h" + h, Harness: h}); err != nil {
			t.Fatal(err)
		}
	}
	toolNames(t, e2, tok2)
	if got := toolsListedOf(t, e2, "s3"); len(got) != 1 || len(got[0].ToolsHidden) != 4 {
		t.Fatalf("claude by the latest agent: %+v, want 4 hidden", got)
	}
}
