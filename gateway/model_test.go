package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/gemini"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

// Each tool's model call on each access it offers, as the tool sends it,
// is recorded with the model it runs: from the body, or, where the path
// names the model (agy on the Gemini API), from the path. There the reply
// names no model either, so its model.responded carries the path's.
func TestRecordedModelEachToolEachAccess(t *testing.T) {
	const (
		anthReq   = `{"model":"%s","messages":[{"role":"user","content":"hi"}]}`
		anthReply = `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		oaiReq    = `{"model":"%s","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
		oaiReply  = `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
		gemReply  = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`
		caReq     = `{"model":"%s","project":"p","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`
		caReply   = `{"response":` + gemReply + `}`
		apiReq    = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	)
	for _, tc := range []struct {
		tool, access string
		a            adapter.Adapter
		target, body string
		reply, model string
	}{
		{"claude", "subscription", anthropic.Adapter{}, "/v1/messages?beta=true", strings.Replace(anthReq, "%s", "claude-opus-5-5", 1), anthReply, "claude-opus-5-5"},
		{"claude", "openrouter", anthropic.Adapter{}, "/v1/messages?beta=true", strings.Replace(anthReq, "%s", "deepseek/deepseek-v4.1-flash", 1), anthReply, "deepseek/deepseek-v4.1-flash"},
		{"claude", "api", anthropic.Adapter{}, "/v1/messages?beta=true", strings.Replace(anthReq, "%s", "claude-sonnet-5", 1), anthReply, "claude-sonnet-5"},
		{"codex", "subscription", openai.Adapter{}, "/backend-api/codex/responses", strings.Replace(oaiReq, "%s", "gpt-6.1", 1), oaiReply, "gpt-6.1"},
		{"codex", "openrouter", openai.Adapter{}, "/v1/responses", strings.Replace(oaiReq, "%s", "openai/gpt-oss-120b", 1), oaiReply, "openai/gpt-oss-120b"},
		{"codex", "api", openai.Adapter{}, "/v1/responses", strings.Replace(oaiReq, "%s", "gpt-5.5", 1), oaiReply, "gpt-5.5"},
		{"agy", "subscription", gemini.Adapter{}, "/v1internal:generateContent", strings.Replace(caReq, "%s", "gemini-3.8-flash-medium", 1), caReply, "gemini-3.8-flash-medium"},
		{"agy", "api", gemini.Adapter{}, "/v1beta/models/gemini-3.1-pro-preview:generateContent", apiReq, gemReply, "gemini-3.1-pro-preview"},
	} {
		t.Run(tc.tool+"/"+tc.access, func(t *testing.T) {
			vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.reply))
			}))
			defer vendor.Close()
			u, _ := url.Parse(vendor.URL)
			mem := &event.Memory{}
			srv := httptest.NewServer(New(u, tc.a, mem))
			defer srv.Close()
			resp, err := http.Post(srv.URL+tc.target, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			got := map[event.Kind]string{}
			for _, e := range waitForKinds(t, mem, event.ModelRequested, event.ModelResponded) {
				got[e.Kind] = e.Model
			}
			fromPath := tc.a.Vendor() == "gemini" && strings.HasPrefix(tc.target, "/v1beta/models/")
			if got[event.ModelRequested] != tc.model || fromPath && got[event.ModelResponded] != tc.model {
				t.Errorf("recorded models %v, want %s", got, tc.model)
			}
		})
	}
}

// waitForKinds waits until mem holds an event of each kind.
func waitForKinds(t *testing.T, mem *event.Memory, kinds ...event.Kind) []event.Event {
	t.Helper()
	for range 400 {
		evs := mem.Events()
		have := map[event.Kind]bool{}
		for _, e := range evs {
			have[e.Kind] = true
		}
		all := true
		for _, k := range kinds {
			all = all && have[k]
		}
		if all {
			return evs
		}
		waitALittle()
	}
	t.Fatalf("not every kind of %v: %+v", kinds, mem.Events())
	return nil
}

func waitALittle() { time.Sleep(5 * time.Millisecond) }
