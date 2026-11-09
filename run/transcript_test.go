package run

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

func transcriptEngine(t *testing.T, names map[string]string) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e, err := New(Config{Store: st, AgentName: func(id string) string { return names[id] }})
	if err != nil {
		t.Fatal(err)
	}
	return e, st
}

func turnWith(t *testing.T, st *store.Store, n int, agentID, agent, model, prompt, reply string) {
	t.Helper()
	id := "t" + string(rune('0'+n))
	evs := []event.Event{{ID: id + "a", Kind: event.TurnStarted, Session: "s", Turn: id, Text: prompt, Harness: agent, AgentID: agentID}}
	if model != "" {
		evs = append(evs, event.Event{ID: id + "b", Kind: event.ModelRequested, Session: "s", Turn: id, Exchange: "x-" + id, Model: model},
			event.Event{ID: id + "c", Kind: event.ModelResponded, Session: "s", Turn: id, Exchange: "x-" + id, Status: 200, Text: reply})
	}
	for _, e := range evs {
		if _, _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

// The transcript names each turn's agent by its saved name and model, or by
// its harness, gives the person's words and the final reply, oldest first,
// says when a turn had no reply, and sits between a head and a foot that
// say what it is.
func TestTranscript(t *testing.T) {
	e, st := transcriptEngine(t, map[string]string{"a_1": "Claude Code · haiku"})
	turnWith(t, st, 1, "a_1", "claude", "claude-haiku", "remember OSPREY-9", "OK")
	turnWith(t, st, 2, "", "codex", "gpt-x", "which word?", "OSPREY-9")
	turnWith(t, st, 3, "a_gone", "claude", "", "stopped early", "")
	got, err := e.transcript("s", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := transcriptHead +
		"--- Turn 1, the person to Claude Code · haiku (model claude-haiku) ---\nremember OSPREY-9\n\n" +
		"--- Turn 1, Claude Code · haiku (model claude-haiku) replied ---\nOK\n\n" +
		"--- Turn 2, the person to Codex (model gpt-x) ---\nwhich word?\n\n" +
		"--- Turn 2, Codex (model gpt-x) replied ---\nOSPREY-9\n\n" +
		"--- Turn 3, the person to Claude Code ---\nstopped early\n\n" +
		"--- Turn 3, Claude Code replied ---\n(no reply: the turn ended without one)\n\n" +
		transcriptFoot
	if got != want {
		t.Errorf("transcript\n got %q\nwant %q", got, want)
	}
}

// Past its budget the transcript keeps the newest turns and says how many
// it left out; a long reply is cut and marked; and a prompt near MaxPrompt
// leaves no room, so there is no transcript.
func TestTranscriptBudget(t *testing.T) {
	e, st := transcriptEngine(t, nil)
	big := strings.Repeat("r", transcriptReplyMax+100)
	for i := 1; i <= 9; i++ {
		turnWith(t, st, i, "", "claude", "m", "q"+string(rune('0'+i)), big)
	}
	got, err := e.transcript("s", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > TranscriptMax {
		t.Errorf("%d bytes, over %d", len(got), TranscriptMax)
	}
	if !strings.Contains(got, "--- Turn 9, the person") || strings.Contains(got, "--- Turn 1, the person") ||
		!strings.Contains(got, " earlier turns are left out: they do not fit.]") || !strings.Contains(got, " [… cut]") {
		t.Errorf("transcript kept the wrong turns or marks:\n%.600s", got)
	}
	if got, _ := e.transcript("s", MaxPrompt-100); got != "" {
		t.Errorf("a prompt near MaxPrompt still got %d bytes of transcript", len(got))
	}
}
