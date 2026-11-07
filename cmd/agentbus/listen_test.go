package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckListen(t *testing.T) {
	type want struct{ ok, loopback bool }
	for addr, w := range map[string]want{
		"127.0.0.1:8787": {true, true}, "[::1]:8787": {true, true},
		"0.0.0.0:8787": {true, false}, ":8787": {true, false}, "[::]:8787": {true, false},
		"192.168.1.2:8787": {true, false}, "[fd00::1]:8787": {true, false},
		// A name resolves through the hosts file, which can map it anywhere.
		"localhost:8787": {false, false}, "myhost:8787": {false, false},
		// Loopback, but the Host check names only 127.0.0.1, localhost and [::1].
		"127.0.0.2:8787": {false, false}, "127.1.2.3:8787": {false, false}, "[::ffff:127.0.0.2]:8787": {false, false},
		"[fe80::1%en0]:8787": {false, false}, "8787": {false, false}, "": {false, false},
	} {
		loopback, err := checkListen(addr)
		if (err == nil) != w.ok || loopback != w.loopback {
			t.Errorf("%q: loopback %v, err %v; want ok %v loopback %v", addr, loopback, err, w.ok, w.loopback)
		}
	}
}

// -listen may name an IP alone; it then gets port 8787. A name gets no port,
// so checkListen still refuses it.
func TestListenWithoutPort(t *testing.T) {
	for in, want := range map[string]string{
		"0.0.0.0": "0.0.0.0:8787", "127.0.0.1": "127.0.0.1:8787", "[::1]": "[::1]:8787", "::1": "[::1]:8787",
		"[::]": "[::]:8787",
		// Already a full form, or not an IP: unchanged.
		"0.0.0.0:9000": "0.0.0.0:9000", ":9000": ":9000", "[::1]:9000": "[::1]:9000",
		"localhost": "localhost", "myhost": "myhost", "8787": "8787", "": "",
	} {
		if got := withPort(in); got != want {
			t.Errorf("withPort(%q) = %q, want %q", in, got, want)
		}
	}
	type want struct{ ok, loopback bool }
	for in, w := range map[string]want{
		"0.0.0.0": {true, false}, "127.0.0.1": {true, true}, "[::1]": {true, true},
		"localhost": {false, false}, "myhost": {false, false}, "127.0.0.2": {false, false},
	} {
		loopback, err := checkListen(withPort(in))
		if (err == nil) != w.ok || loopback != w.loopback {
			t.Errorf("%q: loopback %v, err %v; want ok %v loopback %v", in, loopback, err, w.ok, w.loopback)
		}
	}
}

func TestAgentAddrStaysOnThisMachine(t *testing.T) {
	for in, want := range map[string]string{
		"0.0.0.0:8787": "127.0.0.1:8787", "[::]:8787": "127.0.0.1:8787",
		"127.0.0.1:8787": "127.0.0.1:8787", "[::1]:8787": "[::1]:8787",
		"192.168.1.2:8787": "192.168.1.2:8787",
	} {
		a, _ := net.ResolveTCPAddr("tcp", in)
		if got := agentAddr(a); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// get sends GET path to addr with the given Host and token.
func get(t *testing.T, addr, host, path, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	req.Host = host
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The default, loopback, keeps the Host check exactly: a loopback name on
// another port is refused, as a Docker port publish would send it.
func TestLoopbackListenChecksHost(t *testing.T) {
	home := t.TempDir()
	svc := startService(t, home, "http://127.0.0.1:1")
	tok := readToken(t, home)
	if !svc.loopback {
		t.Fatal("127.0.0.1 not loopback")
	}
	_, port, _ := net.SplitHostPort(svc.addr)
	for host, want := range map[string]int{
		"127.0.0.1:" + port: 200, "localhost:" + port: 200,
		"127.0.0.1:1": 403, "localhost:8788": 403, "evil.example:" + port: 403,
	} {
		if code, body := get(t, svc.addr, host, "/api/v0/agents", tok); code != want {
			t.Errorf("Host %q: %d %s, want %d", host, code, body, want)
		}
	}
}

func TestNonLoopbackListen(t *testing.T) {
	home := t.TempDir()
	svc, err := start(config{listen: "0.0.0.0:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	if svc.loopback {
		t.Fatal("0.0.0.0 reported as loopback")
	}
	tok := readToken(t, home)
	_, port, _ := net.SplitHostPort(svc.addr)
	local := "127.0.0.1:" + port
	// Any Host: a loopback name on another port (a Docker publish), a
	// machine's name, anything. The token is what guards the API.
	for _, host := range []string{local, "127.0.0.1:8788", "localhost:1", "myvm.example.com", "evil.example:" + port} {
		if code, body := get(t, local, host, "/api/v0/agents", tok); code != 200 {
			t.Errorf("Host %q with the token: %d %s, want 200", host, code, body)
		}
		if code, _ := get(t, local, host, "/api/v0/agents", ""); code != 401 {
			t.Errorf("Host %q without a token: %d, want 401", host, code)
		}
		if code, _ := get(t, local, host, "/api/v0/agents", "wrong"); code != 401 {
			t.Errorf("Host %q with a wrong token: %d, want 401", host, code)
		}
	}
	// A vendor path from this machine reaches the gateway: the upstream is
	// unreachable, so the gateway answers 502, not the 403 below.
	if code, body := get(t, local, local, "/v1/messages", ""); code == 403 {
		t.Errorf("vendor path from loopback: %d %s", code, body)
	}
}

// With the service beyond loopback, the vendor paths answer only callers on
// this machine; the API answers anyone with the token.
func TestGatewayThisMachineOnly(t *testing.T) {
	reached := func(name string, hit *bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { *hit = true })
	}
	var anth, oai bool
	local := &net.TCPAddr{IP: net.ParseIP("172.17.0.2"), Port: 8787}
	for _, tc := range []struct {
		open   bool
		remote string
		path   string
		want   bool
	}{
		{false, "192.0.2.9:5000", "/v1/messages", true}, // loopback listen: only this machine connects
		{true, "127.0.0.1:5000", "/v1/messages", true},
		{true, "[::1]:5000", "/s/abc/t/t_1/v1/messages", true},
		{true, "172.17.0.2:5000", "/backend-api/codex/responses", true}, // its own address
		{true, "172.17.0.1:5000", "/v1/messages", false},                // the Docker host
		{true, "192.0.2.9:5000", "/s/abc/t/t_1/backend-api/codex/responses", false},
	} {
		anth, oai = false, false
		h := route(nil, reached("anthropic", &anth), reached("openai", &oai), tc.open)
		r := httptest.NewRequest("POST", tc.path, nil)
		r.RemoteAddr = tc.remote
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := anth || oai; got != tc.want {
			t.Errorf("open %v from %s to %s: reached %v (status %d), want %v", tc.open, tc.remote, tc.path, got, w.Code, tc.want)
		}
		if !tc.want && (w.Code != http.StatusForbidden || w.Header().Get("Connection") != "close") {
			t.Errorf("open %v from %s to %s: status %d, Connection %q; want 403 and close", tc.open, tc.remote, tc.path, w.Code, w.Header().Get("Connection"))
		}
	}
}

// The agent is handed the loopback address, never 0.0.0.0, in both its
// vendor base URL and AGENTBUS_URL.
func TestNonLoopbackHandsAgentsLoopback(t *testing.T) {
	home := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_FAKE_AGENT", "1")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	svc, err := start(config{listen: "0.0.0.0:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, noReplay: true,
		lookPath: func(string) (string, error) { return self, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	tok := readToken(t, home)
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("AGENTBUS_FAKE_OUT", report)
	t.Setenv("AGENTBUS_FAKE_EXIT", "0")
	_, port, _ := net.SplitHostPort(svc.addr)
	want := "http://127.0.0.1:" + port
	code, out, raw := postRun(t, svc, tok, map[string]any{"agent": "claude", "folder": t.TempDir(), "prompt": "hi"})
	if code != http.StatusAccepted {
		t.Fatalf("status %d: %s", code, raw)
	}
	id, _ := out["id"].(string)
	base, _ := out["base_url"].(string)
	if !strings.HasPrefix(base, want+"/s/"+id+"/t/") {
		t.Fatalf("base_url %q, want under %s", base, want)
	}
	waitClosed(t, svc.store, id)
	r := readReport(t, report)
	if u, _ := envValue(r.Env, "ANTHROPIC_BASE_URL"); u != base {
		t.Errorf("child ANTHROPIC_BASE_URL %q, want %q", u, base)
	}
	if u, _ := envValue(r.Env, "AGENTBUS_URL"); u != want {
		t.Errorf("child AGENTBUS_URL %q, want %q", u, want)
	}
}

// A request the router refuses before reading its body is answered at once,
// and its connection closed soon after, even when the body never arrives: the
// server would otherwise wait for the body with no deadline.
func TestRefusalDoesNotWaitForBody(t *testing.T) {
	svc := startService(t, t.TempDir(), "http://127.0.0.1:1")
	for _, path := range []string{"/nothing", "/a/../v1"} {
		c, err := net.Dial("tcp", svc.addr)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 10\r\n\r\n", path, svc.addr)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("%s: no answer while the body is withheld: %v", path, err)
		}
		if resp.StatusCode/100 != 4 || !resp.Close {
			t.Errorf("%s: status %d, close %v; want a 4xx and a closed connection", path, resp.StatusCode, resp.Close)
		}
		io.Copy(io.Discard, resp.Body)
		// The socket itself must close, not only the header say so.
		c.SetReadDeadline(time.Now().Add(refusedBodyWait + 2*time.Second))
		if _, err := c.Read(make([]byte, 1)); err != io.EOF {
			t.Errorf("%s: the connection stayed open while the body is withheld: %v", path, err)
		}
		c.Close()
	}
}
