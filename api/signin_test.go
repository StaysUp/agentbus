package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/staysup/agentbus/store"
)

// signInEnv is an API on a clock the test moves.
func signInEnv(t *testing.T) (*httptest.Server, *API, *atomic.Int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC).UnixNano())
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv, a, &clock
}

// trade sends code to POST /api/v0/signin with the given Origin ("" for
// none) and the browser's other headers.
func trade(t *testing.T, srv *httptest.Server, code, origin string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(SignInRequest{Code: code})
	r, err := http.NewRequest("POST", srv.URL+SignInPath, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return do(t, r)
}

func TestSignInCodeWorksOnce(t *testing.T) {
	srv, a, _ := signInEnv(t)
	code := a.NewSignInCode()
	resp, body := trade(t, srv, code, srv.URL, map[string]string{"Sec-Fetch-Site": "same-origin"})
	var got SignedIn
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &got) != nil || got.Token != token {
		t.Fatalf("first use: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("first use headers: %v", resp.Header)
	}
	resp, body = trade(t, srv, code, srv.URL, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second use: %d %s", resp.StatusCode, body)
	}
	noToken(t, resp, body)
	if resp, body := trade(t, srv, "not-a-code", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a made-up code: %d %s", resp.StatusCode, body)
	}
}

func TestSignInCodeExpires(t *testing.T) {
	srv, a, clock := signInEnv(t)
	code := a.NewSignInCode()
	early := a.NewSignInCode()
	clock.Add(int64(SignInTTL - time.Second))
	if resp, body := trade(t, srv, early, "", nil); resp.StatusCode != 200 {
		t.Fatalf("a second before its expiry: %d %s", resp.StatusCode, body)
	}
	clock.Add(int64(time.Second))
	if resp, body := trade(t, srv, code, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("at its expiry: %d %s", resp.StatusCode, body)
	}
	// A new code works after the old ones expire.
	fresh := a.NewSignInCode()
	if resp, body := trade(t, srv, fresh, "", nil); resp.StatusCode != 200 {
		t.Fatalf("a new code: %d %s", resp.StatusCode, body)
	}
}

// A page on another origin can neither read the reply nor use up a code.
func TestSignInRefusesOtherOrigins(t *testing.T) {
	srv, a, _ := signInEnv(t)
	code := a.NewSignInCode()
	for _, tc := range []struct {
		name, origin string
		hdr          map[string]string
	}{
		{"another origin", "http://evil.example", nil},
		{"the cockpit's dev server", "http://localhost:5173", nil},
		{"a null origin", "null", nil},
		{"another port", "http://127.0.0.1:1", nil},
		{"cross-site", "", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site", "", map[string]string{"Sec-Fetch-Site": "same-site"}},
	} {
		resp, body := trade(t, srv, code, tc.origin, tc.hdr)
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: %d %s, Allow-Origin %q", tc.name, resp.StatusCode, body, resp.Header.Get("Access-Control-Allow-Origin"))
		}
		noToken(t, resp, body)
	}
	// No preflight is answered, so a page cannot send application/json.
	r, _ := http.NewRequest("OPTIONS", srv.URL+SignInPath, nil)
	r.Header.Set("Origin", "http://evil.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	if resp, body := do(t, r); resp.StatusCode < 400 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	// A simple request, which needs no preflight, is refused for its type.
	r, _ = http.NewRequest("POST", srv.URL+SignInPath, strings.NewReader(`{"code":"`+code+`"}`))
	r.Header.Set("Content-Type", "text/plain")
	if resp, body := do(t, r); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d %s", resp.StatusCode, body)
	}
	// None of that used the code up.
	if resp, body := trade(t, srv, code, srv.URL, nil); resp.StatusCode != 200 {
		t.Fatalf("the code after the refusals: %d %s", resp.StatusCode, body)
	}
}

// The Host check still comes first: a rebound name gets nothing.
func TestSignInHostChecked(t *testing.T) {
	srv, a, _ := signInEnv(t)
	code := a.NewSignInCode()
	r, _ := http.NewRequest("POST", srv.URL+SignInPath, strings.NewReader(`{"code":"`+code+`"}`))
	r.Host = "evil.example"
	r.Header.Set("Content-Type", "application/json")
	if resp, body := do(t, r); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rebound Host: %d %s", resp.StatusCode, body)
	}
}

// Only so many codes wait at once; the oldest goes first.
func TestSignInCodesBounded(t *testing.T) {
	srv, a, clock := signInEnv(t)
	first := a.NewSignInCode()
	for range maxCodes {
		clock.Add(int64(time.Millisecond))
		a.NewSignInCode()
	}
	if resp, _ := trade(t, srv, first, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the oldest code past the bound: %d", resp.StatusCode)
	}
}

// Public is every route answered without a token, and only those: a route
// made public later is bounded too, with no list to keep in step.
func TestPublicIsEveryRouteWithoutAToken(t *testing.T) {
	publicPath := map[string]bool{}
	for _, rt := range routes {
		if rt.public {
			publicPath[rt.path] = true
		}
	}
	for _, rt := range routes {
		r, _ := http.NewRequest(rt.method, "http://127.0.0.1"+strings.ReplaceAll(strings.ReplaceAll(rt.path, "{id}", "0123456789abcdef"), "{call}", "0123456789abcdef"), nil)
		if want := publicPath[rt.path]; Public(r) != want {
			t.Errorf("%s %s: Public %v, want %v", rt.method, rt.path, Public(r), want)
		}
	}
	for _, p := range []string{"/docs", "/docs/runner", "/docs/llms.txt", "/docs/redoc.standalone.js", "/docs/nothing", SignInPath} {
		for _, m := range []string{"GET", "POST"} {
			r, _ := http.NewRequest(m, "http://127.0.0.1"+p, nil)
			if !Public(r) {
				t.Errorf("%s %s is not Public", m, p)
			}
		}
	}
}
