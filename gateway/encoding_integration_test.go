//go:build integration

package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
)

// A quota check answered 200 with end_turn must not claim a turn end for a
// turn that never started. A probe exchange reports model.requested and
// model.responded only.
func TestQuotaCheckAnsweredClaimsNoTurnEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"text","text":"I"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	evs := postFor(t, gw, mem, "s1", `{"model":"claude-sonnet-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}],"metadata":{"user_id":"u"}}`, 2)
	time.Sleep(100 * time.Millisecond) // let any later event arrive
	evs = mem.Events()
	if len(evs) != 2 || evs[0].Kind != event.ModelRequested || evs[1].Kind != event.ModelResponded {
		t.Fatalf("kinds = %v, want model.requested and model.responded only", kindsOf(evs))
	}
	for _, e := range evs {
		if !e.Probe || e.Turn != "" {
			t.Errorf("%s: probe %v turn %q, want probe and no turn", e.Kind, e.Probe, e.Turn)
		}
	}
}

func kindsOf(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}
