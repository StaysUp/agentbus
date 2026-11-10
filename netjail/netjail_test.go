package netjail

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAllowed(t *testing.T) {
	allow := []string{"github.com", "*.npmjs.org", "10.0.0.5"}
	for host, want := range map[string]bool{
		"github.com": true, "GitHub.com.": true, "api.github.com": false, "evilgithub.com": false,
		"registry.npmjs.org": true, "a.b.npmjs.org": true, "npmjs.org": false, "xnpmjs.org": false,
		"10.0.0.5": true, "10.0.0.6": false, "": false,
	} {
		if got := Allowed(allow, host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// refusals keeps what a jail reported.
type refusals struct {
	mu   sync.Mutex
	list []string
}

func (r *refusals) add(target, why string) {
	r.mu.Lock()
	r.list = append(r.list, target+" "+why)
	r.mu.Unlock()
}

func (r *refusals) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.list...)
}

// The proxy and the forwarder, served on this machine's own network: what
// they let through and what they refuse, whatever the platform.
func TestProxyAndForward(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "site "+r.URL.Path)
	}))
	defer site.Close()
	siteAddr := strings.TrimPrefix(site.URL, "http://")
	_, sitePort, _ := net.SplitHostPort(siteAddr)

	var got refusals
	fwd, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	prox, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	j := New(Spec{Forward: []string{siteAddr}, Pass: []string{"pass.test:9"}, Allow: []string{"localhost"}, Refused: got.add})
	j.serve([]net.Listener{fwd, prox})
	defer j.Close()
	proxyURL, _ := url.Parse("http://" + prox.Addr().String())
	via := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	get := func(c *http.Client, u string) (int, string) {
		t.Helper()
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// The forwarder passes to the same address outside.
	if code, body := get(http.DefaultClient, "http://"+fwd.Addr().String()+"/f"); code != 200 || body != "site /f" {
		t.Errorf("forwarded: %d %q", code, body)
	}
	// Plain HTTP to an allowed host goes through.
	if code, body := get(via, "http://localhost:"+sitePort+"/p"); code != 200 || body != "site /p" {
		t.Errorf("allowed: %d %q", code, body)
	}
	// Plain HTTP to another host is refused and reported.
	if code, body := get(via, "http://127.0.0.1:"+sitePort+"/x"); code != 403 || !strings.Contains(body, "127.0.0.1 is not in [network] allow") {
		t.Errorf("refused: %d %q", code, body)
	}
	// CONNECT to an allowed host tunnels, bytes sent with the CONNECT
	// included.
	c, err := net.Dial("tcp", prox.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "CONNECT localhost:"+sitePort+" HTTP/1.1\r\nHost: localhost:"+sitePort+"\r\n\r\n"+
		"GET /t HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	c.Close()
	if string(b) != "site /t" {
		t.Errorf("through the tunnel: %q", b)
	}
	// CONNECT to a raw IP and to a host not listed are refused.
	for _, target := range []string{"1.1.1.1:443", "example.com:443"} {
		c, err := net.Dial("tcp", prox.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		c.Close()
		if err != nil || resp.StatusCode != 403 {
			t.Errorf("CONNECT %s: %v %v", target, resp, err)
		}
	}
	want := []string{
		"127.0.0.1:" + sitePort + " 127.0.0.1 is not in [network] allow of permissions.toml",
		"1.1.1.1:443 1.1.1.1 is not in [network] allow of permissions.toml",
		"example.com:443 example.com is not in [network] allow of permissions.toml",
	}
	if g := got.all(); strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("refusals:\n%s\nwant:\n%s", strings.Join(g, "\n"), strings.Join(want, "\n"))
	}
	// Pass lets that one host:port through, and no other port of it.
	if why := j.check("pass.test:9"); why != "" {
		t.Errorf("pass: %s", why)
	}
	if why := j.check("pass.test:10"); why == "" {
		t.Error("pass.test:10 let through")
	}
	// Once closed, nothing is served.
	j.Close()
	if _, err := http.Get("http://" + fwd.Addr().String() + "/f"); err == nil {
		t.Error("forwarder still served after Close")
	}
}

func TestEnvLeavesLoopbackOut(t *testing.T) {
	j := New(Spec{Forward: []string{"127.0.0.1:3128"}})
	env := strings.Join(j.Env(), " ")
	if !strings.Contains(env, "HTTPS_PROXY=http://127.0.0.1:3129") || !strings.Contains(env, "no_proxy=localhost,127.0.0.1,::1") {
		t.Errorf("env: %s", env)
	}
}

// A connection the plain proxy made, upgraded (101) and so out of the
// HTTP server's hands, ends with the jail. Found by codex in review.
func TestCloseEndsUpgradedConnection(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		io.Copy(c, rw)
	}))
	defer site.Close()
	prox, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	j := New(Spec{Allow: []string{"127.0.0.1"}})
	j.serve([]net.Listener{prox})
	defer j.Close()
	c, err := net.Dial("tcp", prox.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, "GET "+site.URL+"/ HTTP/1.1\r\nHost: "+strings.TrimPrefix(site.URL, "http://")+"\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	io.WriteString(c, "before\n")
	if line, err := br.ReadString('\n'); err != nil || line != "before\n" {
		t.Fatalf("before Close: %q %v", line, err)
	}
	j.Close()
	io.WriteString(c, "after\n")
	line, err := br.ReadString('\n')
	var ne net.Error
	switch {
	case err == nil:
		t.Fatalf("bytes still pass after Close: %q", line)
	case errors.As(err, &ne) && ne.Timeout():
		t.Fatal("the connection is still open after Close")
	}
}
