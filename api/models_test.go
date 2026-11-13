package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/models"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// A fake OpenRouter key: no one's credential.
const fakeORModelsKey = "sk-or-v1-fake-models-key-0000111122223333"

// modelsEnv is an API whose lists come from a fake OpenRouter, a fake agy
// listing and a runner's report, on a control with a subscriptions folder.
func modelsEnv(t *testing.T, own func(string) ([]byte, error)) (*env, *credentials.Subscriptions, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/models" || r.Header.Get("Authorization") != "Bearer "+fakeORModelsKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[{"id":"deepseek/deepseek-v4.1-flash","name":"DeepSeek: V4.1 Flash"},{"id":"openai/gpt-oss-120b","name":"OpenAI: gpt-oss-120b"}]}`))
	}))
	t.Cleanup(or.Close)
	subs := &credentials.Subscriptions{Dir: filepath.Join(dir, "subscriptions"), Own: map[string]credentials.Own{
		"agy": {Home: "agy-home", File: ".gemini/antigravity-cli/antigravity-oauth-token", TokenKey: "token.access_token", ExpiryKey: "token.expiry"}}}
	src := &ModelSources{Cache: &models.Cache{}, Client: or.Client(), OpenRouterBase: or.URL + "/api", Own: own,
		OpenRouterKey: func() string { k, _ := subs.Key(credentials.AccessOpenRouter); return k }}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port, Queue: q,
		Subscriptions: &credentials.Manager{Store: subs}, Models: src})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); q.Close(); st.Close() })
	return &env{srv: srv, api: a, store: st, port: port, q: q}, subs, &calls
}

func listOf(t *testing.T, e *env, path string) (int, ModelList, string) {
	t.Helper()
	resp, raw := do(t, e.req(t, "GET", path, nil))
	var l ModelList
	json.Unmarshal([]byte(raw), &l)
	return resp.StatusCode, l, raw
}

func idsOf(l ModelList) string {
	var s []string
	for _, m := range l.Models {
		if m.Label != "" {
			s = append(s, m.ID+"="+m.Label)
		} else {
			s = append(s, m.ID)
		}
	}
	return strings.Join(s, ",")
}

func TestModelLists(t *testing.T) {
	agyOut := []byte("Signed in as someone@example.com\ngemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\ngemini-4-argon-medium\tGemini 4 Argon (Medium)\n")
	var ownRuns atomic.Int32
	e, subs, orCalls := modelsEnv(t, func(h string) ([]byte, error) {
		ownRuns.Add(1)
		if h != "agy" {
			return nil, errors.New("no")
		}
		return agyOut, nil
	})

	// claude on its subscription: the aliases, fixed.
	code, l, raw := listOf(t, e, "/api/v0/harnesses/claude/models?access=subscription")
	if code != 200 || l.Source != models.FromFixed || idsOf(l) != "sonnet,opus,haiku" || l.FetchedAt != nil {
		t.Fatalf("claude: %d %s", code, raw)
	}
	// No such harness, or an access it does not offer.
	for _, p := range []string{"/api/v0/harnesses/opencode/models?access=subscription", "/api/v0/harnesses/agy/models?access=openrouter",
		"/api/v0/harnesses/claude/models?access=api"} {
		if code, _, raw := listOf(t, e, p); code != http.StatusNotFound {
			t.Errorf("%s: %d %s", p, code, raw)
		}
	}

	if code, _, raw := listOf(t, e, "/api/v0/harnesses/claude/models"); code != http.StatusBadRequest {
		t.Errorf("no access: %d %s", code, raw)
	}

	// OpenRouter: with no key set, OpenRouter refuses, in fixed words.
	code, l, raw = listOf(t, e, "/api/v0/harnesses/claude/models?access=openrouter")
	if code != 200 || len(l.Models) != 0 || l.Error != models.ErrRefused.Error() {
		t.Fatalf("openrouter, no key: %d %s", code, raw)
	}
	if err := subs.SetKey(credentials.AccessOpenRouter, fakeORModelsKey); err != nil {
		t.Fatal(err)
	}
	code, l, raw = listOf(t, e, "/api/v0/harnesses/codex/models?access=openrouter")
	if code != 200 || l.Source != models.FromAPI || l.FetchedAt == nil || idsOf(l) != "deepseek/deepseek-v4.1-flash=DeepSeek: V4.1 Flash,openai/gpt-oss-120b=OpenAI: gpt-oss-120b" {
		t.Fatalf("openrouter: %d %s", code, raw)
	}
	// Kept: one list for every harness, fetched again only on demand.
	n := orCalls.Load()
	listOf(t, e, "/api/v0/harnesses/claude/models?access=openrouter")
	if orCalls.Load() != n {
		t.Fatal("fetched again within the hour")
	}
	listOf(t, e, "/api/v0/harnesses/claude/models?access=openrouter&refresh=true")
	if orCalls.Load() != n+1 {
		t.Fatal("refresh=true did not fetch again")
	}
	if strings.Contains(raw, fakeORModelsKey) {
		t.Fatal("a reply holds the key")
	}

	// agy: not connected, so agy is not run.
	code, l, raw = listOf(t, e, "/api/v0/harnesses/agy/models?access=subscription")
	if code != 200 || len(l.Models) != 0 || l.Error != "connect your agy subscription in Settings" || ownRuns.Load() != 0 {
		t.Fatalf("agy, not connected: %d %s", code, raw)
	}
	// Connected: its own listing, only its model lines.
	// Connected, as agy acp --login leaves it: a folder of control's own
	// that holds agy's file, with a fake token.
	os.MkdirAll(subs.Dir, 0o700)
	login, _ := os.MkdirTemp(subs.Dir, ".login-*")
	file := filepath.Join(login, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	os.MkdirAll(filepath.Dir(file), 0o700)
	os.WriteFile(file, []byte(`{"token":{"access_token":"fake-agy-access","refresh_token":"fake-agy-refresh","expiry":"`+
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}}`), 0o600)
	if err := subs.Keep("agy", credentials.Signin{Home: login}); err != nil {
		t.Fatal(err)
	}
	code, l, raw = listOf(t, e, "/api/v0/harnesses/agy/models?access=subscription")
	if code != 200 || l.Source != models.FromCommand || idsOf(l) != "gemini-3.8-flash-medium=Gemini 3.8 Flash (Medium),gemini-4-argon-medium=Gemini 4 Argon (Medium)" ||
		strings.Contains(raw, "example.com") || ownRuns.Load() != 1 {
		t.Fatalf("agy: %d %s", code, raw)
	}
}

// codex on its sign-in: the names a runner reported from codex's own cache,
// the newest report of the runners seen.
func TestCodexModelsFromRunners(t *testing.T) {
	e, _, _ := modelsEnv(t, nil)
	code, l, raw := listOf(t, e, "/api/v0/harnesses/codex/models?access=subscription")
	if code != 200 || len(l.Models) != 0 || l.Source != models.FromRunner || !strings.Contains(l.Error, "first turn") {
		t.Fatalf("before a report: %d %s", code, raw)
	}
	older, newer := time.Now().Add(-time.Hour).UTC(), time.Now().UTC()
	for i, rep := range []harness.Report{
		{Name: "codex", Found: true, Models: []string{"gpt-5.6-luna"}, ModelsAt: &older},
		{Name: "codex", Found: true, Models: []string{"gpt-6.1-sol", "gpt-6-luna", "bad name"}, ModelsAt: &newer},
	} {
		if _, err := e.q.RegisterRunner(queue.DefaultTeam, "r"+string(rune('0'+i)), nil, 1, "hash"+string(rune('0'+i)),
			&queue.Capabilities{Harnesses: []harness.Report{rep}}); err != nil {
			t.Fatal(err)
		}
	}
	code, l, raw = listOf(t, e, "/api/v0/harnesses/codex/models?access=subscription")
	if code != 200 || idsOf(l) != "gpt-6.1-sol,gpt-6-luna" || l.FetchedAt == nil || !l.FetchedAt.Equal(newer.Truncate(0)) && l.FetchedAt.Sub(newer).Abs() > time.Second {
		t.Fatalf("after reports: %d %s", code, raw)
	}
}
