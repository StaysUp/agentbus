package anthropic

import (
	"testing"

	"github.com/staysup/agentbus/event"
)

// capturedQuotaCheck is the request interactive Claude Code 2.1.318 sends at startup,
// captured through a pass-through proxy on 2026-11-05: no stream, no system,
// no tools, max_tokens 1, one user message "quota". Its metadata.user_id holds
// the person's ids, so the value here is a placeholder of the same form.
const capturedQuotaCheck = `{"model":"claude-sonnet-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}],` +
	`"metadata":{"user_id":"{\"device_id\":\"d\",\"account_uuid\":\"a\",\"session_id\":\"s\"}"}}`

func TestQuotaCheckStartsNoTurn(t *testing.T) {
	evs, err := Adapter{}.RequestEvents([]byte(capturedQuotaCheck))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, evs, event.ModelRequested)
	if !evs[0].Probe || evs[0].Model != "claude-sonnet-5-5" {
		t.Errorf("model.requested = %+v, want probe and the model", evs[0])
	}
	if b := (Adapter{}).TurnBasis([]byte(capturedQuotaCheck)); b != nil {
		t.Errorf("TurnBasis = %s, want nil", b)
	}
	// Without metadata it is the same check.
	noMeta := `{"model":"claude-sonnet-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`
	if evs, _ := (Adapter{}).RequestEvents([]byte(noMeta)); len(evs) != 1 || !evs[0].Probe {
		t.Errorf("without metadata: %+v", evs)
	}
}

// A person who types "quota" sends a request with the harness's system prompt,
// tools and streaming. Each of those alone keeps it a turn.
func TestQuotaWordedPromptIsATurn(t *testing.T) {
	for name, body := range map[string]string{
		"real first message": `{"model":"claude-sonnet-5-5","max_tokens":32000,"stream":true,"system":[{"type":"text","text":"You are Claude Code"}],"messages":[{"role":"user","content":"quota"}],"tools":[{"name":"Bash"}]}`,
		"stream":             `{"model":"m","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"quota"}]}`,
		"stream false":       `{"model":"m","max_tokens":1,"stream":false,"messages":[{"role":"user","content":"quota"}]}`,
		"tools":              `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"quota"}],"tools":[{"name":"Bash"}]}`,
		"system":             `{"model":"m","max_tokens":1,"system":"s","messages":[{"role":"user","content":"quota"}]}`,
		"max_tokens":         `{"model":"m","max_tokens":2,"messages":[{"role":"user","content":"quota"}]}`,
		"blocks":             `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"quota"}]}]}`,
		"other words":        `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"quota?"}]}`,
		"two messages":       `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"quota"},{"role":"system","content":"x"}]}`,
	} {
		evs, err := Adapter{}.RequestEvents([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(evs) < 2 || evs[0].Kind != event.TurnStarted {
			t.Errorf("%s: kinds = %v, want a turn.started first", name, kinds(evs))
		}
		for _, e := range evs {
			if e.Probe {
				t.Errorf("%s: %s marked probe", name, e.Kind)
			}
		}
		if (Adapter{}).TurnBasis([]byte(body)) == nil {
			t.Errorf("%s: TurnBasis nil, want a turn", name)
		}
	}
}
