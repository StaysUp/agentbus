package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/staysup/agentbus/store"
)

// recorder is a fake vendor that records the paths it was sent.
type recorder struct {
	mu    sync.Mutex
	paths []string
	srv   *httptest.Server
}

func newRecorder(t *testing.T, reply string) *recorder {
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func startTwoVendors(t *testing.T, home, anthropicURL, chatgptURL string) *service {
	t.Helper()
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL(anthropicURL),
		chatgpt: mustURL(chatgptURL), logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	return svc
}

func post(t *testing.T, addr, path, body string) int {
	t.Helper()
	resp, err := http.Post("http://"+addr+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

const (
	claudeReply = `{"type":"message","model":"claude-x","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`
	codexReply  = `{"model":"gpt-x","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":1}}`
)

// A request carries the agent's sign-in, so it must reach only its own
// vendor: codex's paths go to chatgpt.com and never to Anthropic, and Claude
// Code's go to Anthropic and never to chatgpt.com.
func TestEachVendorPathReachesOnlyItsVendor(t *testing.T) {
	anth := newRecorder(t, claudeReply)
	cg := newRecorder(t, codexReply)
	home := t.TempDir()
	svc := startTwoVendors(t, home, anth.srv.URL, cg.srv.URL)

	codexBody := `{"model":"gpt-x","input":[{"type":"message","role":"user","content":"hello"}]}`
	for _, p := range []string{"/s/sess-c/backend-api/codex/responses", "/backend-api/codex/responses"} {
		if code := post(t, svc.addr, p, codexBody); code != 200 {
			t.Errorf("%s: status %d", p, code)
		}
	}
	if got := anth.seen(); len(got) != 0 {
		t.Fatalf("codex requests reached Anthropic: %v", got)
	}
	if got := strings.Join(cg.seen(), " "); got != "/backend-api/codex/responses /backend-api/codex/responses" {
		t.Fatalf("chatgpt saw %q", got)
	}

	if code := post(t, svc.addr, "/s/sess-a/v1/messages", `{"model":"claude-x","messages":[{"role":"user","content":"hello"}]}`); code != 200 {
		t.Errorf("/v1/messages: status %d", code)
	}
	if got := strings.Join(anth.seen(), " "); got != "/v1/messages" {
		t.Fatalf("Anthropic saw %q", got)
	}
	if len(cg.seen()) != 2 {
		t.Fatalf("a Claude request reached chatgpt: %v", cg.seen())
	}

	// Each vendor's events are stored, under its session, by its adapter.
	vendorOf := func(session string) string {
		var vendor string
		waitFor(t, "events of "+session, func() bool {
			recs, _ := svc.store.After(0, store.Filter{Session: session, Only: true}, 100)
			for _, r := range recs {
				var e struct {
					Vendor string `json:"vendor"`
				}
				if string(r.Kind) == "model.responded" && json.Unmarshal(r.Data, &e) == nil {
					vendor = e.Vendor
					return true
				}
			}
			return false
		})
		return vendor
	}
	if v := vendorOf("sess-c"); v != "openai" {
		t.Errorf("sess-c vendor %q, want openai", v)
	}
	if v := vendorOf("default"); v != "openai" {
		t.Errorf("default vendor %q, want openai", v)
	}
	if v := vendorOf("sess-a"); v != "anthropic" {
		t.Errorf("sess-a vendor %q, want anthropic", v)
	}
}

// A path neither vendor claims is answered 404 here and sent nowhere, so no
// sign-in goes to a vendor that did not ask for it. Odd paths are still
// refused with 400 before routing.
func TestUnclaimedPathRefusedLocally(t *testing.T) {
	anth := newRecorder(t, claudeReply)
	cg := newRecorder(t, codexReply)
	svc := startTwoVendors(t, t.TempDir(), anth.srv.URL, cg.srv.URL)

	// GET / redirects to the docs (main.go route); every other method on it
	// is unclaimed.
	if code := post(t, svc.addr, "/", `{}`); code != 404 {
		t.Errorf("POST /: status %d, want 404", code)
	}
	for _, p := range []string{"/v2/messages", "/v1x", "/backend-api/other", "/backend-api/codexx/responses",
		"/s/abc/backend-api/wham/usage", "/s/abc", "/s/a.b/v1/messages", "/responses"} {
		if code := rawGet(t, svc.addr, p, ""); code != 404 {
			t.Errorf("%s: status %d, want 404", p, code)
		}
		if code := post(t, svc.addr, p, `{}`); code != 404 {
			t.Errorf("POST %s: status %d, want 404", p, code)
		}
	}
	if code := rawGet(t, svc.addr, "/backend-api/codex/../../v1/messages", ""); code != 400 {
		t.Errorf("a dot path: status %d, want 400", code)
	}
	if a, c := anth.seen(), cg.seen(); len(a)+len(c) != 0 {
		t.Fatalf("unclaimed paths reached a vendor: anthropic %v, chatgpt %v", a, c)
	}
}
