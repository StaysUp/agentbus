package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
)

// A session whose turns ran on different agents: each turn names its own
// agent, the session keeps its first agent beside its latest, a transcript
// reads every turn's words and last reply without any earlier transcript,
// a turn that got one reads back the prompt its agent got, and a resume id
// from before a switch to another harness reads as stale.
func TestSessionAcrossAgents(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	exit0 := 0
	for _, e := range []event.Event{
		{ID: "1", Kind: event.SessionOpened, Session: "s", Turn: "t1", Hold: "h1", Harness: "claude", AgentID: "a_haiku", Folder: "/w"},
		{ID: "2", Kind: event.TurnStarted, Session: "s", Turn: "t1", Text: "remember PELICAN", Harness: "claude", AgentID: "a_haiku", Folder: "/w"},
		{ID: "3", Kind: event.ModelRequested, Session: "s", Turn: "t1", Exchange: "x-1", Model: "claude-haiku"},
		{ID: "4", Kind: event.ModelResponded, Session: "s", Turn: "t1", Exchange: "x-1", Status: 200, Text: "OK"},
		{ID: "5", Kind: event.SessionClosed, Session: "s", Turn: "t1", Hold: "h1", ExitCode: &exit0, ResumeID: "claude-sess"},
		{ID: "6", Kind: event.SessionOpened, Session: "s", Turn: "t2", Hold: "h2", Harness: "codex", AgentID: "codex", Folder: "/w"},
		{ID: "7", Kind: event.TurnStarted, Session: "s", Turn: "t2", Text: "which word?", Context: "[earlier turns]\n\n",
			Harness: "codex", AgentID: "codex", Folder: "/w", Continuity: "transcript"},
		{ID: "8", Kind: event.ModelRequested, Session: "s", Turn: "t2", Exchange: "x-2", Model: "gpt"},
		{ID: "9", Kind: event.ModelResponded, Session: "s", Turn: "t2", Exchange: "x-2", Status: 200, Text: "PELICAN"},
		{ID: "10", Kind: event.SessionClosed, Session: "s", Turn: "t2", Hold: "h2", ExitCode: &exit0},
	} {
		mustAppend(t, s, e)
	}

	list, err := s.Turns(TurnQuery{Session: "s", Limit: 10})
	if err != nil || len(list.Turns) != 2 || list.Turns[0].AgentID != "codex" || list.Turns[1].AgentID != "a_haiku" {
		t.Fatalf("turns %+v %v", list.Turns, err)
	}
	if list.Turns[0].Prompt != "which word?" {
		t.Errorf("listed prompt %q, want the person's words only", list.Turns[0].Prompt)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if x := session(t, snap, "s"); x.Harness != "codex" || x.FirstAgent != "claude" || x.FirstAgentID != "a_haiku" {
		t.Errorf("session agent %q first %q (%q), want codex and claude (a_haiku)", x.Harness, x.FirstAgent, x.FirstAgentID)
	}

	tr, err := s.Transcript("s")
	if err != nil || len(tr) != 2 {
		t.Fatalf("transcript %+v %v", tr, err)
	}
	if tr[0] != (TranscriptTurn{Turn: "t1", Agent: "claude", AgentID: "a_haiku", Model: "claude-haiku", Prompt: "remember PELICAN", Reply: "OK"}) ||
		tr[1] != (TranscriptTurn{Turn: "t2", Agent: "codex", AgentID: "codex", Model: "gpt", Prompt: "which word?", Reply: "PELICAN"}) {
		t.Errorf("transcript %+v", tr)
	}

	if p, _, tr, found, err := s.TurnStart("s", "t2"); err != nil || !found || !tr || p != "[earlier turns]\n\nwhich word?" {
		t.Errorf("turn start %q %v %v %v, want a transcript turn: the context then the words", p, tr, found, err)
	}

	c, err := s.Continuation("s")
	if err != nil || c.Harness != "codex" || c.AgentID != "codex" || c.ResumeID != "claude-sess" || !c.ResumeStale {
		t.Errorf("continuation %+v %v, want codex now and claude's resume id from before the switch", c, err)
	}
	// Replay before the switch does not count: the transcript turn started
	// fresh, so its resume id, or a later one, holds everything.
	mustAppend(t, s, event.Event{ID: "1b", Kind: event.TurnStarted, Session: "r", Turn: "u1", Text: "a", ReplayFrom: "u0"})
	mustAppend(t, s, event.Event{ID: "1c", Kind: event.TurnStarted, Session: "r", Turn: "u2", Text: "b", Continuity: "transcript"})
	if used, err := s.UsedReplay("r"); err != nil || used {
		t.Errorf("used replay %v %v, want false after a transcript turn", used, err)
	}
	mustAppend(t, s, event.Event{ID: "1d", Kind: event.TurnStarted, Session: "r", Turn: "u3", Text: "c", ReplayFrom: "u2"})
	if used, err := s.UsedReplay("r"); err != nil || !used {
		t.Errorf("used replay %v %v, want true for a replay after it", used, err)
	}
	// A turn after the switch that gives a resume id makes it current.
	mustAppend(t, s, event.Event{ID: "11", Kind: event.TurnStarted, Session: "s", Turn: "t3", Text: "more", Harness: "codex", Continuity: "replay"})
	mustAppend(t, s, event.Event{ID: "12", Kind: event.SessionClosed, Session: "s", Turn: "t3", Hold: "h3", ExitCode: &exit0, ResumeID: "codex-thread"})
	if c, err := s.Continuation("s"); err != nil || c.ResumeID != "codex-thread" || c.ResumeStale {
		t.Errorf("continuation %+v %v, want codex's id, current", c, err)
	}
}

// A transcript cuts a long text at TranscriptTextMax on a rune boundary and
// says so.
func TestTranscriptCutsLongText(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	long := strings.Repeat("é", TranscriptTextMax) // 2 bytes a rune
	mustAppend(t, s, event.Event{ID: "1", Kind: event.TurnStarted, Session: "s", Turn: "t1", Text: "x" + long})
	tr, err := s.Transcript("s")
	if err != nil || len(tr) != 1 {
		t.Fatalf("transcript %+v %v", tr, err)
	}
	if !tr[0].PromptCut || len(tr[0].Prompt) > TranscriptTextMax || !strings.HasPrefix(tr[0].Prompt, "x") ||
		strings.ContainsRune(tr[0].Prompt, '�') || tr[0].Reply != "" || tr[0].ReplyCut {
		t.Errorf("cut %d bytes, cut %v, reply %q", len(tr[0].Prompt), tr[0].PromptCut, tr[0].Reply)
	}
}
