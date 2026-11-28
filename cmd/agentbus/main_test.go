package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/jobs"
)

func startService(t *testing.T, home string, upstreamURL string) *service {
	t.Helper()
	up, _ := url.Parse(upstreamURL)
	// codex's upstream is unreachable here: no test reaches chatgpt.com.
	// agentbus up sends every model call to its own relay, which the upstream
	// backs (nosecrets and up_signin tests cover the credential path).
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: up, chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	return svc
}

// connectFake keeps a fake subscription token for each of harnesses in a
// data folder, as the cockpit's Connect would: every turn of agentbus up
// gets its credential from control since 0.5.1. Without harnesses: claude
// and codex. The token is fake and lasts an hour; nothing renews it.
func connectFake(t *testing.T, home string, harnesses ...string) {
	t.Helper()
	if len(harnesses) == 0 {
		harnesses = []string{"claude", "codex"}
	}
	subs := &credentials.Subscriptions{Dir: filepath.Join(home, subscriptionsDir)}
	for _, h := range harnesses {
		if err := subs.Put(h, fakeSubscription, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
}

// codexRelayArgs is how codex is pointed at a turn's base URL (BASE) when
// its credential comes from control: a provider of its own, whose key is
// the placeholder and whose models are the account's (harness codex
// WireRelay).
const codexRelayArgs = `-c model_provider=agentbus_chatgpt -c model_providers.agentbus_chatgpt.name="OpenAI" ` +
	`-c model_providers.agentbus_chatgpt.base_url="BASE/backend-api/codex" -c model_providers.agentbus_chatgpt.env_key="AGENTBUS_CODEX_TOKEN" ` +
	`-c model_providers.agentbus_chatgpt.wire_api="responses" -c model_providers.agentbus_chatgpt.supports_websockets=false ` +
	`-c model_providers.agentbus_chatgpt.model_catalog_url="BASE/backend-api/codex/models"`

// fakeSubscription is connectFake's token.
const fakeSubscription = "fake-subscription-token-for-tests"

func readToken(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "token"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// rawGet sends the path exactly as written, so no client library cleans it.
func rawGet(t *testing.T, addr, path, auth string) int {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n"
	if auth != "" {
		req += "Authorization: " + auth + "\r\n"
	}
	fmt.Fprint(c, req+"\r\n")
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestAPIPathsNeverReachUpstream(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Errorf("upstream reached: %s", r.URL.Path)
	}))
	defer upstream.Close()
	home := t.TempDir()
	svc := startService(t, home, upstream.URL)
	tok := readToken(t, home)

	paths := []string{
		"/api", "/api/", "/api/v1/events", "/api/v0/sessions", "/api/v0/actions",
		"/s/abc/api", "/s/abc/api/", "/s/abc/api/v0/events", "/s/a.b/api/x", "/s/abc%2Fapi/x",
		"//api/v0/sessions", "/v1/../api/v0", "/s/abc/../api", "/s/abc/v1/../../api/v1/x",
		// Raw paths under an API prefix that clean to a vendor path.
		"/api/../v1/messages", "/s/abc/api/../v1/messages", "/api/%2e%2e/v1/messages",
		// A run's base URL names its turn: the API under it is still never forwarded.
		"/s/abc/t/t_0123456789abcdef/api/v0/sessions", "/s/abc/t/t_1/api/../v1/messages",
	}
	for _, p := range paths {
		for _, auth := range []string{"", "Bearer wrong", "Bearer " + tok} {
			if code := rawGet(t, svc.addr, p, auth); code/100 == 5 || code == 0 {
				t.Errorf("%s: status %d", p, code)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream reached %d times", hits.Load())
	}
}

func TestTokenMadeOnceWithMode0600(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", "home")
	svc := startService(t, home, "http://127.0.0.1:1")
	info, err := os.Stat(filepath.Join(home, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v", info.Mode().Perm())
	}
	tok := readToken(t, home)
	id := svc.store.ID()
	svc.stop()

	svc2 := startService(t, home, "http://127.0.0.1:1")
	if readToken(t, home) != tok {
		t.Fatal("token changed across restarts")
	}
	if svc2.store.ID() != id {
		t.Fatal("store id changed across restarts")
	}
	if code := rawGet(t, svc2.addr, "/api/v0/sessions", "Bearer "+tok); code != 200 {
		t.Fatalf("old token after restart: %d", code)
	}
}

func TestTokenReadableByOthersRefused(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "token"), []byte("abc\n"), 0o644)
	os.Chmod(filepath.Join(home, "token"), 0o644)
	up, _ := url.Parse("http://127.0.0.1:1")
	_, err := start(config{listen: "127.0.0.1:0", home: home, upstream: up, logf: t.Logf})
	if err == nil || strings.Contains(err.Error(), "abc") {
		t.Fatalf("err %v", err)
	}
}

func TestOriginFlag(t *testing.T) {
	for o, ok := range map[string]bool{
		"": true, "http://localhost:5173": true, "https://cockpit.example": true,
		"http://localhost:5173/": false, "localhost:5173": false, "*": false, "http://a/b": false,
	} {
		if err := checkOrigin(o); (err == nil) != ok {
			t.Errorf("%q: %v", o, err)
		}
	}
}

func TestDataFolder(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"AGENTBUS_HOME": "/a", "XDG_DATA_HOME": "/x", "HOME": "/h"}, "/a"},
		{map[string]string{"XDG_DATA_HOME": "/x", "HOME": "/h"}, "/x/agentbus"},
		{map[string]string{"HOME": "/h"}, "/h/.local/share/agentbus"},
	}
	for _, c := range cases {
		if got, _ := dataFolder(env(c.env)); got != c.want {
			t.Errorf("%v: %q, want %q", c.env, got, c.want)
		}
	}
}

func TestUsage(t *testing.T) {
	var b bytes.Buffer
	if run(nil, os.Getenv, &b, &b) != 2 || run([]string{"down"}, os.Getenv, &b, &b) != 2 ||
		run([]string{"version", "x"}, os.Getenv, &b, &b) != 2 {
		t.Fatal("unknown subcommand accepted")
	}
}

func TestSecondServiceOnOneFolderRefused(t *testing.T) {
	home := t.TempDir()
	startService(t, home, "http://127.0.0.1:1")
	up, _ := url.Parse("http://127.0.0.1:1")
	if svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: up, logf: t.Logf}); err == nil {
		svc.stop()
		t.Fatal("a second service started on the same data folder")
	} else if !strings.Contains(err.Error(), "another agentbus service") {
		t.Fatalf("message %q", err)
	}
}

func TestOddPathsRefusedBeforeRouting(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()
	home := t.TempDir()
	svc := startService(t, home, upstream.URL)
	tok := readToken(t, home)

	refused := []string{
		"/./api/../v1/messages", "/s/abc/./api/../v1/messages",
		"/api/../v1/messages", "/s/abc/api/../v1/messages",
		"/v1/./messages", "/v1/../v1/messages", "/v1/messages/.", "/v1/messages/..", "/.", "/..",
		"//v1/messages", "/v1//messages", "/s//v1/messages", "//api/v0/sessions",
		"/%2e/v1/messages", "/api/%2E%2E/v1/messages", "/v1/%2e%2e/x", "/v1/.%2E/x",
		"/v1%2fmessages", "/s/abc%2Fapi/x", "/v1%5cmessages", "/v1%5Cmessages",
		"http://" + svc.addr + "/v1/../api/x", "*",
	}
	for _, p := range refused {
		for _, auth := range []string{"", "Bearer " + tok} {
			if code := rawGet(t, svc.addr, p, auth); code != 400 {
				t.Errorf("%s: %d, want 400", p, code)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream reached %d times by refused paths", hits.Load())
	}
	// That a normal path routes to its vendor is proven across processes in
	// TestXProcEachVendorPathReachesOnlyItsVendor; here only that an odd path
	// is refused before routing, which needs no running turn.
	if code := rawGet(t, svc.addr, "/api/v0/sessions", "Bearer "+tok); code != 200 {
		t.Errorf("/api/v0/sessions: %d", code)
	}
	if code := rawGet(t, svc.addr, "/api/", "Bearer "+tok); code != 404 {
		t.Errorf("/api/: %d", code)
	}
}

// readable by other users. Start makes it 0700.
func TestExistingDataFolderMadePrivate(t *testing.T) {
	home := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	startService(t, home, "http://127.0.0.1:1")
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("data folder mode = %o, want 700", mode)
	}
}

// own prefix. It starts, stops and reads turns, and nothing else.
func TestTurnsTokenMadeAndScoped(t *testing.T) {
	home := t.TempDir()
	svc := startService(t, home, "http://127.0.0.1:1")
	path := filepath.Join(home, "turns_token")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("turns_token mode %v", info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(string(b))
	if !strings.HasPrefix(tok, turnsTokenPrefix) || len(tok) != len(turnsTokenPrefix)+64 || tok == readToken(t, home) {
		t.Fatal("turns_token is not abq_ and 64 hex characters, or is the main token")
	}
	if code := rawGet(t, svc.addr, "/api/v0/turns", "Bearer "+tok); code != 200 {
		t.Errorf("GET /api/v0/turns with the turns token: %d, want 200", code)
	}
	if code := rawGet(t, svc.addr, "/api/v0/sessions", "Bearer "+tok); code != 403 {
		t.Errorf("GET /api/v0/sessions with the turns token: %d, want 403", code)
	}
	svc.stop()
	startService(t, home, "http://127.0.0.1:1")
	if b2, _ := os.ReadFile(path); string(b2) != string(b) {
		t.Fatal("turns_token changed across restarts")
	}
}

// -max-runs was retired in 0.6.7: a command that still sets it starts, and is
// told where the limit is now. A runner's -capacity stays, as its machine's
// own ceiling; without it the settings decide.
func TestMaxRunsFlag(t *testing.T) {
	var b bytes.Buffer
	c, ok := parse([]string{"up", "-max-runs", "4"}, t.TempDir(), &b)
	if !ok || !strings.Contains(b.String(), "-max-runs was retired in 0.6.7") || !strings.Contains(b.String(), "max_delegated_at_once") {
		t.Fatalf("up -max-runs 4: ok %v, %q", ok, b.String())
	}
	if c.capacity != 0 {
		t.Fatalf("-max-runs set a capacity: %d", c.capacity)
	}
	b.Reset()
	if _, ok := parse([]string{"up"}, t.TempDir(), &b); !ok || b.Len() != 0 {
		t.Fatalf("up: ok %v, %q", ok, b.String())
	}
	if runnerCapacity(0) != jobs.MaxCapacity || runnerCapacity(3) != 3 {
		t.Fatalf("runner capacity: %d %d", runnerCapacity(0), runnerCapacity(3))
	}
}
