package api

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/staysup/agentbus/toolreg"
)

// checkIdempotencyKey is the one validator every key-accepting route shares. It
// accepts an empty key and the caller shape, and refuses any '/', which is what
// keeps the internal delivery/ result/ apply/ namespaces unreachable by a
// caller (his call, 2026-11-19).
func TestCheckIdempotencyKey(t *testing.T) {
	for _, ok := range []string{"", "k", "a.b_c:d-1", strings.Repeat("x", 128)} {
		if msg := checkIdempotencyKey(ok); msg != "" {
			t.Errorf("checkIdempotencyKey(%q) = %q, want accepted", ok, msg)
		}
	}
	for _, bad := range []string{"delivery/k", "result/x", "apply/y", "a/b", "has space", strings.Repeat("x", 129)} {
		if checkIdempotencyKey(bad) == "" {
			t.Errorf("checkIdempotencyKey(%q) accepted, want refused", bad)
		}
	}
}

// askApp is a valid ask_agent-shaped app: start a turn, then register the on
// that delivers its reply to the caller's session.
func askApp() toolreg.Tool {
	return toolreg.Tool{
		Name:        "ask_agent",
		Description: "Ask another agent.",
		Inputs: map[string]any{"type": "object", "properties": map[string]any{
			"agent": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"}}},
		Where: toolreg.WhereControl,
		Run: toolreg.Run{Kind: toolreg.RunApp, App: &toolreg.App{
			Steps: []toolreg.Step{
				{StepID: "start", OperationID: "startTurn",
					RequestBody: map[string]any{"agent": "$inputs.agent", "prompt": "$inputs.prompt"},
					Outputs:     map[string]string{"turn": "$response.body#/turn"}},
				{StepID: "deliver", OperationID: "registerOn",
					RequestBody: map[string]any{"turn": "$steps.start.outputs.turn", "session": "$caller.session"}},
			},
			Outputs: map[string]string{"turn": "$steps.start.outputs.turn"},
		}},
	}
}

func TestCheckAppSpecValid(t *testing.T) {
	a := &API{byOp: opIndex()}
	tool := askApp()
	if why := toolreg.Check(&tool); why != "" {
		t.Fatalf("structure refused: %s", why)
	}
	if why := a.checkApp(tool); why != "" {
		t.Fatalf("a valid app was refused by the spec check: %s", why)
	}
}

func TestCheckAppSpecInvalid(t *testing.T) {
	a := &API{byOp: opIndex()}
	cases := []struct {
		name string
		mut  func(*toolreg.Tool)
	}{
		{"unknown operation", func(t *toolreg.Tool) { t.Run.App.Steps[0].OperationID = "flyTurn" }},
		{"operation with an unfillable path param", func(t *toolreg.Tool) {
			// callContext's path has {call}, which the app runtime cannot fill.
			t.Run.App.Steps[0].OperationID = "callContext"
			t.Run.App.Steps[0].RequestBody = nil
			t.Run.App.Steps[0].Outputs = nil
		}},
		{"unknown body field", func(t *toolreg.Tool) { t.Run.App.Steps[0].RequestBody["nope"] = "x" }},
		{"output names no reply field", func(t *toolreg.Tool) {
			t.Run.App.Steps[0].Outputs["turn"] = "$response.body#/nope"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := askApp()
			c.mut(&tool)
			if why := a.checkApp(tool); why == "" {
				t.Fatalf("case %q accepted; expected a refusal", c.name)
			}
		})
	}
}

// A main-token-only operation cannot be an app step: an app runs with the
// caller's own token.
func TestCheckAppRefusesMainOnly(t *testing.T) {
	a := &API{byOp: opIndex()}
	var mainOp string
	for op, rt := range a.byOp {
		if rt.turn == mainOnly && !rt.public && !rt.self {
			mainOp = op
			break
		}
	}
	if mainOp == "" {
		t.Skip("no main-only operation to test with")
	}
	tool := askApp()
	tool.Run.App.Steps[0].OperationID = mainOp
	tool.Run.App.Steps[0].RequestBody = map[string]any{}
	tool.Run.App.Steps[0].Outputs = nil
	if why := a.checkApp(tool); why == "" {
		t.Fatalf("a main-only operation %q was accepted as an app step", mainOp)
	}
}

// An id that fills a path segment is refused unless it is an id shape, so it
// cannot add path segments or a fragment and change which operation runs (found
// in review 2026-11-19: "t/apply#" turned a stop into an apply).
func TestRunStepValidatesID(t *testing.T) {
	a := &API{byOp: opIndex()}
	rt := a.byOp["stopTurn"] // POST /api/v0/turns/{id}/stop
	if rt == nil {
		t.Fatal("no stopTurn operation")
	}
	r := httptest.NewRequest("POST", "/api/v0/mcp", nil)
	for _, bad := range []string{"t_x/apply#", "t_x/apply", "../agents", "t x"} {
		if code, _, why := a.runStep(r, rt, nil, map[string]any{"id": bad}); why == "" || code != 0 {
			t.Fatalf("a path-injecting id %q was accepted: code=%d why=%q", bad, code, why)
		}
	}
}

func TestResolveValue(t *testing.T) {
	args := map[string]any{"agent": "codex", "turn": "t_1"}
	steps := map[string]map[string]any{"start": {"turn": "t_9"}}
	caller := "s_me"
	cases := []struct {
		in   any
		want any
	}{
		{"$inputs.agent", "codex"},
		{"$steps.start.outputs.turn", "t_9"},
		{"$caller.session", "s_me"},
		{"literal", "literal"},
		{"Review turn {$inputs.turn} now", "Review turn t_1 now"},
		{42.0, 42.0},
	}
	for _, c := range cases {
		got, why := resolveValue(c.in, args, caller, steps)
		if why != "" {
			t.Fatalf("resolve %v: %s", c.in, why)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("resolve %v = %v, want %v", c.in, got, c.want)
		}
	}
	// One pass: a value that itself contains {$...} is not re-scanned as another
	// input's placeholder (found in review 2026-11-19). With a="{$inputs.b}" and
	// b="value", the template keeps a's literal and fills b once.
	twoArg := map[string]any{"a": "{$inputs.b}", "b": "value"}
	if out, why := resolveValue("A={$inputs.a}; B={$inputs.b}", twoArg, caller, steps); why != "" || out != "A={$inputs.b}; B=value" {
		t.Fatalf("interpolation re-scanned a substituted value: %q %q", out, why)
	}

	// A conditional value: "then" when if_present resolves to a non-empty
	// string, "else" otherwise, each branch interpolated. review_turn uses it so
	// one app answers a call with a turn and one without.
	cond := map[string]any{
		"if_present": "$inputs.turn",
		"then":       "turn {$inputs.turn}",
		"else":       "workspace {$steps.start.outputs.turn}",
	}
	if out, why := resolveValue(cond, args, caller, steps); why != "" || out != "turn t_1" {
		t.Fatalf("conditional with a present input: %q %q, want %q", out, why, "turn t_1")
	}
	if out, why := resolveValue(cond, map[string]any{"turn": ""}, caller, steps); why != "" || out != "workspace t_9" {
		t.Fatalf("conditional with an empty input: %q %q, want %q", out, why, "workspace t_9")
	}
	if out, why := resolveValue(cond, map[string]any{}, caller, steps); why != "" || out != "workspace t_9" {
		t.Fatalf("conditional with an absent input: %q %q, want %q", out, why, "workspace t_9")
	}
	// A plain object with no if_present key stays a literal, not a conditional.
	lit := map[string]any{"k": "v"}
	if out, why := resolveValue(lit, args, caller, steps); why != "" || !reflect.DeepEqual(out, lit) {
		t.Fatalf("a plain object was not kept as a literal: %v %q", out, why)
	}
}

// The review_turn app's prompt adapts to its input: a named turn points the
// reviewer at that turn's diff; no turn points it at the workspace's whole
// unapplied set, named by the session step's workspace output.
func TestReviewTurnAppPrompt(t *testing.T) {
	a := &API{}
	var start toolreg.Step
	for _, s := range reviewTurnApp().Run.App.Steps {
		if s.StepID == "start" {
			start = s
		}
	}
	if start.StepID == "" {
		t.Fatal("review_turn has no start step")
	}
	// The session step hands on the asker's workspace, which the no-turn prompt names.
	stepOut := map[string]map[string]any{"session": {"session": "s_rev", "workspace": "ws_abc"}}

	withTurn, _, why := a.resolveStep(start, map[string]any{"turn": "t_7", "agent": "codex"}, "s_caller", stepOut)
	if why != "" {
		t.Fatalf("resolve with a turn: %s", why)
	}
	if p, _ := withTurn["prompt"].(string); !strings.Contains(p, "GET /api/v0/turns/t_7/diff") || strings.Contains(p, "/workspaces/") {
		t.Fatalf("with a turn, the prompt should read the turn diff, not the workspace: %q", p)
	}

	noTurn, _, why := a.resolveStep(start, map[string]any{"agent": "codex"}, "s_caller", stepOut)
	if why != "" {
		t.Fatalf("resolve with no turn: %s", why)
	}
	if p, _ := noTurn["prompt"].(string); !strings.Contains(p, "GET /api/v0/workspaces/ws_abc/changes") || strings.Contains(p, "/api/v0/turns/") {
		t.Fatalf("with no turn, the prompt should read the workspace's unapplied set, not a turn: %q", p)
	}
}

func TestJSONPointer(t *testing.T) {
	doc := map[string]any{
		"turn":    "t_1",
		"changes": map[string]any{"count": 2.0},
		"agents":  []any{map[string]any{"id": "a_0"}, map[string]any{"id": "a_1"}},
	}
	if v, ok := jsonPointer(doc, "/turn"); !ok || v != "t_1" {
		t.Fatalf("/turn = %v %v", v, ok)
	}
	if v, ok := jsonPointer(doc, "/changes/count"); !ok || v != 2.0 {
		t.Fatalf("/changes/count = %v %v", v, ok)
	}
	// An array index: a pointer into a list element (the review finding).
	if v, ok := jsonPointer(doc, "/agents/1/id"); !ok || v != "a_1" {
		t.Fatalf("/agents/1/id = %v %v", v, ok)
	}
	for _, bad := range []string{"/missing", "/agents/2/id", "/agents/x", "/agents/01", "/agents/-1"} {
		if _, ok := jsonPointer(doc, bad); ok {
			t.Fatalf("%s resolved; want miss", bad)
		}
	}
}

// deriveKey is stable for the same call and differs per step; a key fits the
// turn idempotency_key pattern (letters, digits, '-').
func TestDeriveKey(t *testing.T) {
	args := map[string]any{"agent": "codex", "prompt": "do it"}
	// Same call id = a retry: the same key, so the write is a no-op.
	k1 := deriveKey(nil, "ask_agent", "1", args, "start")
	k2 := deriveKey(nil, "ask_agent", "1", args, "start")
	if k1 != k2 {
		t.Fatalf("a retry (same call id) gave two keys: %s %s", k1, k2)
	}
	// A different step of the same call gets its own key.
	if k3 := deriveKey(nil, "ask_agent", "1", args, "deliver"); k1 == k3 {
		t.Fatalf("two steps shared a key: %s", k1)
	}
	// A DIFFERENT call id = a fresh call: a different key even when the
	// arguments match, so two separate asks are two turns (found in review).
	if k4 := deriveKey(nil, "ask_agent", "2", args, "start"); k1 == k4 {
		t.Fatalf("a fresh call with the same args reused the key: %s", k1)
	}
	if !idempotencyKey.MatchString(k1) {
		t.Fatalf("derived key %q does not fit the idempotency key pattern", k1)
	}
}
