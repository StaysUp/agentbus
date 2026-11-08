package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

	"github.com/staysup/agentbus/api"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

func startService(t *testing.T, home string, upstreamURL string) *service {
	t.Helper()
	up, _ := url.Parse(upstreamURL)
	// codex's upstream is unreachable here: no test reaches chatgpt.com.
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: up, chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	return svc
}

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

func TestGatewayPathsReachUpstreamAndStore(t *testing.T) {
	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-x","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	home := t.TempDir()
	svc := startService(t, home, upstream.URL)

	req, _ := http.NewRequest("POST", "http://"+svc.addr+"/s/sess-1/v1/messages",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer vendor-sign-in")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || gotAuth.Load() != "Bearer vendor-sign-in" {
		t.Fatalf("status %d, upstream auth %v", resp.StatusCode, gotAuth.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		recs, _ := svc.store.After(0, store.Filter{Session: "sess-1", Only: true}, 100)
		var kinds []string
		for _, r := range recs {
			kinds = append(kinds, string(r.Kind))
			if bytes.Contains(r.Data, []byte("vendor-sign-in")) {
				t.Fatal("a header reached a stored event")
			}
		}
		if strings.Contains(strings.Join(kinds, " "), "model.responded") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored kinds %v", kinds)
		}
		time.Sleep(10 * time.Millisecond)
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
	var gotPath atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath.Store(r.URL.Path)
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

	// Normal paths still route.
	if code := rawGet(t, svc.addr, "/v1/messages?q=/../x", ""); code != 200 || hits.Load() != 1 {
		t.Errorf("/v1/messages: %d, hits %d", code, hits.Load())
	}
	if code := rawGet(t, svc.addr, "/s/abc/v1/models", ""); code != 200 || gotPath.Load() != "/v1/models" {
		t.Errorf("/s/abc/v1/models: %d, upstream path %v", code, gotPath.Load())
	}
	if code := rawGet(t, svc.addr, "/api/v0/sessions", "Bearer "+tok); code != 200 {
		t.Errorf("/api/v0/sessions: %d", code)
	}
	if code := rawGet(t, svc.addr, "/api/", "Bearer "+tok); code != 404 {
		t.Errorf("/api/: %d", code)
	}
}

func TestBarePathIsTheDefaultSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-x","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	home := t.TempDir()
	svc := startService(t, home, upstream.URL)
	tok := readToken(t, home)

	resp, err := http.Post("http://"+svc.addr+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		req, _ := http.NewRequest("GET", "http://"+svc.addr+"/api/v0/sessions/default/events", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Events []struct {
				Kind    string `json:"kind"`
				Session string `json:"session"`
			} `json:"events"`
		}
		json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		var kinds []string
		for _, e := range page.Events {
			if e.Session != "default" {
				t.Fatalf("event of session %q", e.Session)
			}
			kinds = append(kinds, e.Kind)
		}
		if strings.Contains(strings.Join(kinds, " "), "model.responded") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session default has %v", kinds)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An existing data folder kept its mode, so a 0755 folder left the store
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

// The service prices every model.responded before storing it.
func TestStoredReplyCarriesCost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"hi"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":2000,"cache_read_input_tokens":100000,`+
			`"cache_creation_input_tokens":50000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":50000},"service_tier":"standard"}}`)
	}))
	defer upstream.Close()
	svc := startService(t, t.TempDir(), upstream.URL)

	resp, err := http.Post("http://"+svc.addr+"/s/sess-1/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		recs, _ := svc.store.After(0, store.Filter{Session: "sess-1", Only: true}, 100)
		for _, r := range recs {
			if r.Kind != event.ModelResponded {
				continue
			}
			var e event.Event
			if err := json.Unmarshal(r.Data, &e); err != nil {
				t.Fatal(err)
			}
			// 1000*4 + 2000*20 + 100000*0.20 + 50000*8 (1h write), per million.
			if e.CostUSD == nil || math.Abs(*e.CostUSD-0.464) > 1e-9 || e.CostComplete == nil || !*e.CostComplete {
				t.Fatalf("stored cost_usd %v cost_complete %v, want 0.464 true", e.CostUSD, e.CostComplete)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no model.responded stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// permissions.toml is made once, mode 0600, with the starter's empty lists,
// and an existing file is never written again.
func TestMakePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), api.PermissionsFile)
	if err := makePermissions(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	if b, _ := os.ReadFile(path); string(b) != api.PermissionsStarter {
		t.Errorf("starter %q", b)
	}
	const mine = "allow = [\"go test\"]\n"
	if err := os.WriteFile(path, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := makePermissions(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != mine {
		t.Errorf("an existing file was written: %q", b)
	}
}

// The turns token is made once beside the main token, with mode 0600 and its
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

func TestMaxRunsFlag(t *testing.T) {
	var b bytes.Buffer
	for _, v := range []string{"0", "-1", "x", "65"} {
		b.Reset()
		if code := run([]string{"up", "-max-runs", v}, func(string) string { return t.TempDir() }, &b, &b); code != 2 {
			t.Errorf("-max-runs %s: exit %d, want 2", v, code)
		}
	}
}
