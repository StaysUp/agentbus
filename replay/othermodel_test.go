package replay

import (
	"strings"
	"testing"

	"github.com/staysup/agentbus/store"
)

// A request to another model than the history's, as after a switch of agent
// on Claude Code: the history keeps the conversation and loses what belongs
// to the model or process that made it. Thinking blocks go (their
// signatures are that model's), and so do a system message's tool_addition
// blocks and its output_config. A system message left with text only becomes
// a plain string; one left with nothing, and an assistant message of
// thinking only, go.
func TestSpliceForAnotherModel(t *testing.T) {
	const (
		sysAnnounce = `{"role":"system","content":[{"type":"text","text":"` + sys1 + `"},{"type":"tool_addition","tool":{"name":"mcp_x"}}],"output_config":{"effort":"high"}}`
		toolsOnly   = `{"role":"system","content":[{"type":"tool_removal","name":"mcp_x"}]}`
		thinkOnly   = `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"U0lH"}]}`
	)
	last := `{"model":"claude-x","messages":[` +
		`{"role":"user","content":[` + reminder + `,{"type":"text","text":"remember the word kiwi"}]},` +
		sysAnnounce + `,` + thinkOnly + `,` +
		`{"role":"user","content":[{"type":"text","text":"go on"}]},` + toolsOnly +
		`],` + top + `,"stream":true}`
	src := newSource()
	src.addTurn("t1", "remember the word kiwi", "")
	src.addCall(t, "t1", "x-1", last, t1Reply, "end_turn")
	src.addTurn("t2", "which word?", "t1")
	r := New(src)

	other := strings.Replace(t2Loop, `"model":"claude-x"`, `"model":"deepseek/deepseek-v4.1-flash"`, 1)
	rw := r.Rewrite("s", "t2", []byte(other))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("refused or unchanged: %+v", rw.Events)
	}
	loop := mustElements(t, field([]byte(t2Loop), "messages"))
	want := `[{"role":"user","content":[` + reminder + `,{"type":"text","text":"remember the word kiwi"}]},` +
		`{"role":"system","content":"` + sys1 + `"},` +
		`{"role":"user","content":[{"type":"text","text":"go on"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"Noted: kiwi."}]},` +
		`{"role":"user","content":"which word?"},` +
		`{"role":"system","content":"` + cut2 + `"},` +
		string(loop[2]) + `,` + string(loop[3]) + `]`
	if got := field2(t, rw.Body, "messages"); got != want {
		t.Errorf("messages\n got %s\nwant %s", got, want)
	}
	if rw.Replayed != 4 {
		t.Errorf("replayed %d, want 4", rw.Replayed)
	}

	// The same model keeps the history as it was.
	rw = r.Rewrite("s", "t2", []byte(t2Loop))
	for _, s := range []string{`"signature":"U0lHLXR3bw=="`, `"tool_addition"`, `"output_config"`, `"tool_removal"`} {
		if !strings.Contains(string(rw.Body), s) {
			t.Errorf("same model: the request lacks %s", s)
		}
	}
}

// codex: a request to another model drops the history's reasoning items,
// whose encrypted content only the model that made them can read.
func TestCodexSpliceForAnotherModel(t *testing.T) {
	_, r := codexTwoTurns(t)
	fresh := strings.Replace(codexReq(thread2, append(context("b", "read-only"), prompt("b", p2))...),
		`"model":"gpt-x"`, `"model":"gpt-y"`, 1)
	rw := r.Rewrite("s", "t2", []byte(fresh))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("not spliced: %+v", rw.Events)
	}
	want := append(append(context("a", "read-only"), prompt("a", p1), answer1), prompt("b", p2))
	eqItems(t, "input", inputOf(t, rw.Body), want)
	if rw.Replayed != 6 {
		t.Errorf("replayed %d, want 6", rw.Replayed)
	}
}

// A turn on another harness got the earlier turns as text, then failed
// before any model call. The next turn's history is that turn's prompt and
// the note, never the turn before it, which is the other harness's.
func TestNotReachedTranscriptTurnStandsAlone(t *testing.T) {
	src, _ := twoTurns(t)
	const withContext = "[earlier turns as text]\n\nwhich word?"
	src.turns["t2"] = fakeTurn{prompt: withContext, transcript: true}
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(1)})
	src.addTurn("t3", "again?", "t2")
	r := New(src)
	rw := r.Rewrite("s", "t3", []byte(t2First("again?")))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("refused or unchanged: %+v", rw.Events)
	}
	msgs := field2(t, rw.Body, "messages")
	if strings.Contains(msgs, "kiwi") || !strings.Contains(msgs, jsonQuote(withContext)) || !strings.Contains(msgs, "not acted on") {
		t.Errorf("messages %s", msgs)
	}
}

// A turn that failed before any model call carries the history before it,
// and that history's model: a later request to another model still lays it
// out for that model.
func TestNotReachedKeepsTheHistoryModel(t *testing.T) {
	src, _ := twoTurns(t)
	src.setEnd("t2", store.TurnEnd{Closed: true, ExitCode: intp(1)})
	src.addTurn("t3", "again?", "t2")
	r := New(src)
	other := strings.Replace(t2First("again?"), `"model":"claude-x"`, `"model":"deepseek/deepseek-v4.1-flash"`, 1)
	rw := r.Rewrite("s", "t3", []byte(other))
	if rw.Refuse != nil || rw.Body == nil {
		t.Fatalf("refused or unchanged: %+v", rw.Events)
	}
	if b := string(rw.Body); strings.Contains(b, `"signature"`) || !strings.Contains(b, "kiwi") {
		t.Errorf("body %s", b)
	}
}
