package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
	h := route(nil, vendors{gw("anthropic"), gw("openai"), gw("or-anthropic"), gw("or-responses"), nil}, false)
	for path, want := range map[string]string{
		"/s/abc/t/t_1/v1/messages":                         "anthropic",
		"/s/abc/t/t_1/backend-api/codex/responses":         "openai",
		"/s/abc/t/t_1/openrouter/v1/messages":              "or-anthropic",
		"/s/abc/t/t_1/openrouter/v1/messages/count_tokens": "or-anthropic",
		"/s/abc/t/t_1/openrouter/v1/responses":             "or-responses",
		"/s/abc/t/t_1/openrouter/v1/models":                "or-anthropic",
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
		h := route(nil, vendors{reached("anthropic", &anth), reached("openai", &oai), reached("openrouter", &anth), reached("openrouter", &oai), nil}, tc.open)
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

// Only the vendor paths are this machine's alone. The root sends anyone to
// the docs, and a path no vendor claims gets 404 from every caller, as on
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
				h := route(nil, vendors{gw, gw, gw, gw, nil}, open)
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
