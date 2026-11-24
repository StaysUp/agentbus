package toolreg

import "testing"

// objInputs is a small object schema with the named string properties, all
// optional, for building app tools in tests.
func objInputs(names ...string) map[string]any {
	props := map[string]any{}
	for _, n := range names {
		props[n] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": props}
}

// askAgentApp is the canonical valid app used across the cases.
func askAgentApp() Tool {
	return Tool{
		Name:        "ask_agent",
		Description: "Ask another agent to do a piece of work.",
		Inputs:      objInputs("agent", "prompt"),
		Run: Run{Kind: RunApp, App: &App{
			Steps: []Step{
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

func TestCheckAppValid(t *testing.T) {
	tool := askAgentApp()
	if why := Check(&tool); why != "" {
		t.Fatalf("a valid app was refused: %s", why)
	}
	if tool.Where != WhereControl {
		t.Fatalf("where not filled: %q", tool.Where)
	}
}

func TestCheckAppInterpolatedPromptValid(t *testing.T) {
	tool := askAgentApp()
	tool.Name = "review_turn"
	tool.Inputs = objInputs("turn", "agent")
	tool.Run.App.Steps[0].RequestBody = map[string]any{
		"agent":  "$inputs.agent",
		"prompt": "Review turn {$inputs.turn}; read its diff via the API.",
	}
	if why := Check(&tool); why != "" {
		t.Fatalf("an interpolated prompt was refused: %s", why)
	}
}

// A conditional value (if_present / then / else) is valid when every branch's
// references are in scope, and each branch may embed its own {$...}.
func TestCheckAppConditionalValid(t *testing.T) {
	tool := askAgentApp()
	tool.Name = "review_turn"
	tool.Inputs = objInputs("turn", "agent")
	tool.Run.App.Steps[0].RequestBody["prompt"] = map[string]any{
		"if_present": "$inputs.turn",
		"then":       "Review turn {$inputs.turn}.",
		"else":       "Review {$inputs.agent}'s whole workspace.",
	}
	if why := Check(&tool); why != "" {
		t.Fatalf("a valid conditional prompt was refused: %s", why)
	}
}

func TestCheckAppInvalid(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Tool)
	}{
		{"no steps", func(t *Tool) { t.Run.App.Steps = nil }},
		{"nil app", func(t *Tool) { t.Run.App = nil }},
		{"api field set on app", func(t *Tool) { t.Run.Method = "POST" }},
		{"duplicate stepId", func(t *Tool) { t.Run.App.Steps[1].StepID = "start" }},
		{"empty stepId", func(t *Tool) { t.Run.App.Steps[0].StepID = "" }},
		{"empty operationId", func(t *Tool) { t.Run.App.Steps[0].OperationID = "" }},
		{"unknown input", func(t *Tool) { t.Run.App.Steps[0].RequestBody["agent"] = "$inputs.missing" }},
		{"caller used wrong", func(t *Tool) { t.Run.App.Steps[0].RequestBody["agent"] = "$caller.turn" }},
		{"response in request", func(t *Tool) { t.Run.App.Steps[0].RequestBody["agent"] = "$response.body#/x" }},
		{"self output read", func(t *Tool) {
			t.Run.App.Steps[0].RequestBody["agent"] = "$steps.start.outputs.turn"
		}},
		{"forward output read", func(t *Tool) {
			// deliver reads an output that start never declares
			t.Run.App.Steps[1].RequestBody["turn"] = "$steps.deliver.outputs.turn"
		}},
		{"undeclared step output", func(t *Tool) {
			t.Run.App.Steps[1].RequestBody["turn"] = "$steps.start.outputs.nope"
		}},
		{"bad step output expr", func(t *Tool) {
			t.Run.App.Steps[0].Outputs["turn"] = "$inputs.agent"
		}},
		{"app output undeclared", func(t *Tool) {
			t.Run.App.Outputs["turn"] = "$steps.start.outputs.nope"
		}},
		{"bad interpolation", func(t *Tool) {
			t.Run.App.Steps[0].RequestBody["prompt"] = "see {$inputs.missing}"
		}},
		{"bad ref in a conditional branch", func(t *Tool) {
			t.Run.App.Steps[0].RequestBody["prompt"] = map[string]any{
				"if_present": "$inputs.agent",
				"then":       "ok {$inputs.missing}",
				"else":       "ok",
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := askAgentApp()
			c.mut(&tool)
			if why := Check(&tool); why == "" {
				t.Fatalf("case %q was accepted; expected a refusal", c.name)
			}
		})
	}
}
