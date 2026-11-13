package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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

// Each vendor path reaches its own gateway: an OpenRouter agent's calls go
// under /openrouter, by API format.
func TestRouteOpenRouter(t *testing.T) {
	var hit string
	gw := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = name })
	}
	keyed := map[string]http.Handler{apiKeyPrefix("claude"): gw("key-claude"), apiKeyPrefix("codex"): gw("key-codex"), apiKeyPrefix("agy"): gw("key-agy")}
	h := route(nil, vendors{anthropic: gw("anthropic"), openai: gw("openai"), openrouterAnth: gw("or-anthropic"), openrouterResps: gw("or-responses"),
		gemini: gw("gemini"), keyed: keyed}, false)
	for path, want := range map[string]string{
		"/s/abc/t/t_1/v1/messages":                         "anthropic",
		"/s/abc/t/t_1/backend-api/codex/responses":         "openai",
		"/s/abc/t/t_1/openrouter/v1/messages":              "or-anthropic",
		"/s/abc/t/t_1/openrouter/v1/messages/count_tokens": "or-anthropic",
		"/s/abc/t/t_1/openrouter/v1/responses":             "or-responses",
		"/s/abc/t/t_1/openrouter/v1/models":                "or-anthropic",
		"/s/abc/t/t_1/v1internal:streamGenerateContent":    "gemini",
		"/v1internal:loadCodeAssist":                       "gemini",
		"/s/abc/t/t_1/v1internalx:streamGenerateContent":   "",
		// An agent on its vendor's API key, under /key/<harness>.
		"/s/abc/t/t_1/key/claude/v1/messages":                                             "key-claude",
		"/s/abc/t/t_1/key/codex/v1/responses":                                             "key-codex",
		"/s/abc/t/t_1/key/agy/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent": "key-agy",
		"/s/abc/t/t_1/key/claudex/v1/messages":                                            "",
		"/s/abc/t/t_1/key/opencode/v1/messages":                                           "",
	} {
		hit = ""
		r := httptest.NewRequest("POST", path, nil)
		r.RemoteAddr = "127.0.0.1:5000"
		h.ServeHTTP(httptest.NewRecorder(), r)
		if hit != want {
			t.Errorf("%s reached %q, want %q", path, hit, want)
		}
	}
}

// With no agy gateway, the Code Assist path is answered 404 and sent
// nowhere.
func TestRouteNoGemini(t *testing.T) {
	hit := false
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true })
	h := route(nil, vendors{anthropic: gw, openai: gw, openrouterAnth: gw, openrouterResps: gw}, false)
	r := httptest.NewRequest("POST", "/s/abc/t/t_1/v1internal:streamGenerateContent", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if hit || rec.Code != http.StatusNotFound {
		t.Fatalf("reached %v, status %d", hit, rec.Code)
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
		{true, "192.0.2.9:5000", "/s/abc/v1/messages", false},
		{true, "192.0.2.9:5000", "/backend-api/codex", false},
		{true, "192.0.2.9:5000", "/s/abc/t/t_1/openrouter/v1/messages", false},
		{true, "[::1]:5000", "/s/abc/t/t_1/openrouter/v1/messages", true},
	} {
		anth, oai = false, false
		h := route(nil, vendors{anthropic: reached("anthropic", &anth), openai: reached("openai", &oai), openrouterAnth: reached("openrouter", &anth), openrouterResps: reached("openrouter", &oai)}, tc.open)
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

// Only the vendor paths are this machine's alone. In a runner, which serves
// no cockpit, the root sends anyone to the docs, and a path no vendor claims gets 404 from every caller, as on
// loopback: a browser asking for /favicon.ico is told there is none, not that
// it is on the wrong machine.
func TestRootAndUnroutedPaths(t *testing.T) {
	reached := false
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	local := &net.TCPAddr{IP: net.ParseIP("172.17.0.2"), Port: 8787}
	for _, open := range []bool{false, true} {
		for _, remote := range []string{"127.0.0.1:5000", "172.17.0.1:5000"} {
			for _, tc := range []struct {
				method, path string
				code         int
				location     string
			}{
				{"GET", "/", http.StatusFound, "/docs"},
				{"HEAD", "/", http.StatusFound, "/docs"},
				{"GET", "/?x=1", http.StatusFound, "/docs"},
				{"POST", "/", http.StatusNotFound, ""},
				{"GET", "/favicon.ico", http.StatusNotFound, ""},
				{"GET", "/index.html", http.StatusNotFound, ""},
				{"GET", "/s/abc", http.StatusNotFound, ""},
				{"GET", "/s/abc/favicon.ico", http.StatusNotFound, ""},
				{"GET", "/s/abc/t/t_1/", http.StatusNotFound, ""},
				{"GET", "/v1x", http.StatusNotFound, ""},
				{"GET", "/backend-api", http.StatusNotFound, ""},
				{"GET", "/s/abc/t/t_1/openrouter", http.StatusNotFound, ""},
				{"POST", "/s/abc/t/t_1/openrouter/v2/x", http.StatusNotFound, ""},
			} {
				reached = false
				h := route(nil, vendors{anthropic: gw, openai: gw, openrouterAnth: gw, openrouterResps: gw, gemini: gw}, open)
				r := httptest.NewRequest(tc.method, tc.path, nil)
				r.RemoteAddr = remote
				r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.code || w.Header().Get("Location") != tc.location || reached {
					t.Errorf("open %v from %s: %s %s: %d, Location %q, reached %v; want %d, Location %q, not reached",
						open, remote, tc.method, tc.path, w.Code, w.Header().Get("Location"), reached, tc.code, tc.location)
				}
			}
		}
	}
}

// In up and server the cockpit answers / and every GET path no vendor
// claims, to any caller; a vendor path, a session's prefix, a write and the
// API are what they were.
func TestCockpitAtRoot(t *testing.T) {
	gwReached, cockpitReached := false, false
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gwReached = true })
	ck := fakeSite{func(w http.ResponseWriter, r *http.Request) { cockpitReached = true }}
	local := &net.TCPAddr{IP: net.ParseIP("172.17.0.2"), Port: 8787}
	for _, open := range []bool{false, true} {
		for _, tc := range []struct {
			method, path string
			cockpit, gw  bool
			code         int
		}{
			{"GET", "/", true, false, 200},
			{"HEAD", "/", true, false, 200},
			{"GET", "/?t=abc", true, false, 200},
			{"GET", "/settings", true, false, 200},
			{"GET", "/assets/index-1.js", true, false, 200},
			{"GET", "/favicon.ico", false, false, 404},
			{"GET", "/runners", false, false, 404},
			{"POST", "/", false, false, 404},
			{"POST", "/settings", false, false, 404},
			{"GET", "/s/abc", false, false, 404},
			{"GET", "/s/abc/favicon.ico", false, false, 404},
			{"GET", "/s/abc/t/t_1/", false, false, 404},
			{"GET", "/v1x", false, false, 404},
			{"GET", "/backend-api", false, false, 404},
		} {
			gwReached, cockpitReached = false, false
			h := route(nil, vendors{anthropic: gw, openai: gw, openrouterAnth: gw, openrouterResps: gw, gemini: gw, cockpit: ck}, open)
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.RemoteAddr = "172.17.0.1:5000"
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.code || cockpitReached != tc.cockpit || gwReached != tc.gw {
				t.Errorf("open %v: %s %s: %d, cockpit %v, gateway %v; want %d, %v, %v",
					open, tc.method, tc.path, w.Code, cockpitReached, gwReached, tc.code, tc.cockpit, tc.gw)
			}
		}
		// A vendor path stays the gateway's, and this machine's alone.
		gwReached, cockpitReached = false, false
		h := route(nil, vendors{anthropic: gw, openai: gw, openrouterAnth: gw, openrouterResps: gw, gemini: gw, cockpit: ck}, open)
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !gwReached || cockpitReached {
			t.Errorf("open %v: GET /v1/models reached gateway %v, cockpit %v", open, gwReached, cockpitReached)
		}
	}
}

// The link names the address the service listens on, and loopback for a
// wildcard one.
func TestSignInLink(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:8787":    "http://127.0.0.1:8787/?t=abc",
		"0.0.0.0:9000":      "http://127.0.0.1:9000/?t=abc",
		"[::]:9001":         "http://127.0.0.1:9001/?t=abc",
		"[::1]:9002":        "http://[::1]:9002/?t=abc",
		"192.168.1.10:9003": "http://192.168.1.10:9003/?t=abc",
		// up passes the listener's own address, which is numeric; a named
		// port is still only text here, never parsed as a URL.
		"127.0.0.1:http-alt": "http://127.0.0.1:http-alt/?t=abc",
	} {
		if got := signInLink(listen, "abc"); got != want {
			t.Errorf("%s: %s, want %s", listen, got, want)
		}
	}
}

// A browser opens only on a desktop of this person's: not over SSH, not
// on a Linux with no display.
func TestBrowserCommand(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	found := func(string) (string, error) { return "/usr/bin/xdg-open", nil }
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	for _, tc := range []struct {
		name, goos string
		getenv     func(string) string
		look       func(string) (string, error)
		want       string
	}{
		{"mac", "darwin", env(), found, "open"},
		{"mac over ssh", "darwin", env("SSH_CONNECTION", "1 2 3 4"), found, ""},
		{"linux desktop", "linux", env("DISPLAY", ":0"), found, "/usr/bin/xdg-open"},
		{"linux wayland", "linux", env("WAYLAND_DISPLAY", "wayland-0"), found, "/usr/bin/xdg-open"},
		{"linux no display", "linux", env(), found, ""},
		{"linux no xdg-open", "linux", env("DISPLAY", ":0"), missing, ""},
		{"linux over ssh", "linux", env("DISPLAY", ":10", "SSH_TTY", "/dev/pts/1"), found, ""},
		{"other", "windows", env(), found, ""},
	} {
		got := strings.Join(browserCommand(tc.goos, tc.getenv, tc.look), " ")
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// fakeSite serves what the cockpit has here: /, /settings and
// /assets/index-1.js.
type fakeSite struct{ h http.HandlerFunc }

func (f fakeSite) ServeHTTP(w http.ResponseWriter, r *http.Request) { f.h(w, r) }
func (f fakeSite) Serves(r *http.Request) bool {
	return (r.Method == "GET" || r.Method == "HEAD") &&
		(r.URL.Path == "/" || r.URL.Path == "/settings" || r.URL.Path == "/assets/index-1.js")
}

// The browser is opened on a page of the person's own, never on the link:
// a program's arguments are in the process list for every user to read.
func TestOpenPageHoldsTheLinkPrivately(t *testing.T) {
	home := t.TempDir()
	link := "http://127.0.0.1:8787/?t=c0ffee"
	page, err := writeOpenPage(home, link)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "c0ffee") {
		t.Fatalf("the page's path %s holds the code", page)
	}
	info, err := os.Stat(page)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the page's mode: %v %v", info, err)
	}
	b, _ := os.ReadFile(page)
	for _, want := range []string{`content="0;url=http://127.0.0.1:8787/?t=c0ffee"`, `location.replace("http://127.0.0.1:8787/?t=c0ffee")`, `content="no-referrer"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the page lacks %s:\n%s", want, b)
		}
	}
	// A later link's page is not removed by an earlier one's timer.
	if _, err := writeOpenPage(home, link+"2"); err != nil {
		t.Fatal(err)
	}
	removeOpenPage(page, link)
	if _, err := os.Stat(page); err != nil {
		t.Fatal("an earlier link's timer removed a later page")
	}
	removeOpenPage(page, link+"2")
	if _, err := os.Stat(page); !os.IsNotExist(err) {
		t.Fatalf("the page stayed: %v", err)
	}
	// A link that would break out of the attribute or the script is escaped.
	if p := openPage(`http://x/?t="><script>`); strings.Contains(p, `"><script>`) {
		t.Fatalf("not escaped: %s", p)
	}
}

// The cockpit takes no body: one sent is refused before the cockpit sees it.
func TestCockpitRefusesABody(t *testing.T) {
	reached := false
	ck := fakeSite{func(w http.ResponseWriter, r *http.Request) { reached = true }}
	h := route(nil, vendors{cockpit: ck}, false)
	r := httptest.NewRequest("GET", "/settings", strings.NewReader("0123456789"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || reached || w.Header().Get("Connection") != "close" {
		t.Fatalf("GET /settings with a body: %d, reached %v, Connection %q", w.Code, reached, w.Header().Get("Connection"))
	}
}
