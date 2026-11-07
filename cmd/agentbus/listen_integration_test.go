//go:build integration

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// The root redirects a browser to the docs on loopback and on 0.0.0.0, and
// the docs it lands on answer. A path no vendor claims gets 404 on both.
func TestRootRedirectsToDocs(t *testing.T) {
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, listen := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
		home := t.TempDir()
		svc, err := start(config{listen: listen, home: home, upstream: mustURL("http://127.0.0.1:1"),
			chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(svc.addr)
		base := "http://127.0.0.1:" + port
		for _, method := range []string{"GET", "HEAD"} {
			req, _ := http.NewRequest(method, base+"/", nil)
			resp, err := noFollow.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/docs" {
				t.Errorf("%s: %s /: %d, Location %q; want 302 to /docs", listen, method, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
		if code, _ := get(t, svc.addr, "127.0.0.1:"+port, "/", ""); code != 200 {
			t.Errorf("%s: / followed to /docs: %d, want 200", listen, code)
		}
		if code, body := get(t, svc.addr, "127.0.0.1:"+port, "/favicon.ico", ""); code != 404 {
			t.Errorf("%s: /favicon.ico: %d %s, want 404", listen, code, body)
		}
		svc.stop()
	}
}
