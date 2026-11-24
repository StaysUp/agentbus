package jobs

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
)

// A listing of codex's models: its token gets the subscription kept on
// control on codex's own list-models call only, on codex's upstream only,
// while the listing runs. Every call is recorded under ModelsSession; a
// call refused gets 403 and reaches no vendor.
func TestListingGetsTheSubscriptionOnItsListOnly(t *testing.T) {
	var mu sync.Mutex
	var got []string
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+r.Header.Get("ChatGPT-Account-ID"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[]}`))
	}))
	t.Cleanup(vendor.Close)
	u, _ := url.Parse(vendor.URL)
	mem := &event.Memory{}
	chat := gateway.New(u, openai.Adapter{}, mem)
	other := gateway.New(u, openai.Adapter{}, mem)
	t.Cleanup(chat.Close)
	t.Cleanup(other.Close)
	src := fixedKey{"codex": {Token: "fake-chatgpt-access", Headers: map[string]string{"ChatGPT-Account-ID": "fake-account"}}}
	s := &Server{cfg: Config{Credentials: credentials.Plug{credentials.AccessSubscription: src}, Relay: map[string]Upstream{
		"chatgpt": {URL: u, Access: credentials.AccessSubscription, Harness: "codex", Gateways: []*gateway.Gateway{chat}},
		"other":   {URL: u, Access: credentials.AccessSubscription, Harness: "claude", Gateways: []*gateway.Gateway{other}},
	}}}
	if _, _, _, ok := s.ModelsToken("agy"); ok {
		t.Fatal("a token for a harness the relay has no upstream for")
	}
	base, tok, end, ok := s.ModelsToken("codex")
	if !ok || base != "/relay/chatgpt" || !strings.HasPrefix(tok, ListingPrefix) {
		t.Fatalf("%q %q %v", base, tok, ok)
	}
	relay := s.Relay()
	do := func(method, path, tok string) int {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		relay.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/relay/chatgpt/backend-api/codex/models?client_version=0.164.0", 200},
		{"POST", "/relay/chatgpt/backend-api/codex/responses", 403},              // a model call
		{"POST", "/relay/chatgpt/backend-api/codex/alpha/search", 403},           // a service call, not the list
		{"GET", "/relay/chatgpt/backend-api/wham/usage", 403},                    // not recorded
		{"GET", "/relay/other/backend-api/codex/models?client_version=1.0", 403}, // another harness's vendor
	} {
		if code := do(c.method, c.path, tok); code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, code, c.want)
		}
	}
	end()
	if code := do("GET", "/relay/chatgpt/backend-api/codex/models?client_version=0.164.0", tok); code != http.StatusUnauthorized {
		t.Errorf("after the listing: %d", code)
	}
	chat.Settle(ModelsSession, "", time.Second)
	other.Settle(ModelsSession, "", time.Second)
	mu.Lock()
	if len(got) != 1 || got[0] != "GET /backend-api/codex/models?client_version=0.164.0 Bearer fake-chatgpt-access fake-account" {
		t.Errorf("the vendor got %q", got)
	}
	mu.Unlock()
	// Each call is recorded: the list by its status, the model call it
	// tried as refused in the vendor's place (model.requested, then its
	// refusal), the unrecorded path as call.refused, and the other vendor's
	// list by the 403 answered in its place.
	want := map[string]bool{"vendor.called GET /backend-api/codex/models 200": false, "model.requested": false,
		"call.refused GET /backend-api/wham/usage 403": false, "vendor.called GET /backend-api/codex/models 403": false}
	for range 200 {
		for _, e := range mem.Events() {
			if e.Session != ModelsSession || e.Turn != "" {
				t.Fatalf("an event outside the listings' session: %+v", e)
			}
			k := string(e.Kind)
			if e.Kind != event.ModelRequested {
				k += " " + e.Call + " " + strconv.Itoa(e.Status)
			}
			if _, ok := want[k]; ok {
				want[k] = true
			}
		}
		all := true
		for _, seen := range want {
			all = all && seen
		}
		if all {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("recorded: %v", want)
}
