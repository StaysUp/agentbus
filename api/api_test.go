package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/store"
)

const token = "tok-THIS-MUST-NEVER-APPEAR-0123456789"
const origin = "http://localhost:5173"

type env struct {
	srv   *httptest.Server
	api   *API
	store *store.Store
	port  string
	// With a stack (stackEnv): the queue and the engine behind the API.
	q   *queue.Queue
	eng *run.Engine
	// workdir is the runner's folder for copies.
	workdir string
}

func newEnvAt(t *testing.T, dbPath string) *env {
	t.Helper()
	return newEnvOrigin(t, dbPath, origin)
}

// newEnvOrigin is newEnvAt with the given -origin; "" allows every origin.
func newEnvOrigin(t *testing.T, dbPath, allow string) *env {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://127.0.0.1:" + port,
		Origin: allow, Ping: 50 * time.Millisecond, BodyTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		st.Close()
	})
	return &env{srv: srv, api: a, store: st, port: port}
}

func newEnv(t *testing.T) *env {
	return newEnvAt(t, filepath.Join(t.TempDir(), "events.db"))
}

func (e *env) req(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func do(t *testing.T, r *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func noToken(t *testing.T, resp *http.Response, body string) {
	t.Helper()
	if strings.Contains(body, token) {
		t.Fatalf("token in body: %s", body)
	}
	for k, v := range resp.Header {
		if strings.Contains(strings.Join(v, " "), token) {
			t.Fatalf("token in header %s", k)
		}
	}
}

func TestTokenRequiredAndNeverEchoed(t *testing.T) {
	e := newEnv(t)
	for _, auth := range []string{"", "Bearer", "Bearer ", "Bearer wrong", "Bearer " + token + "x", token, "Basic " + token} {
		r := e.req(t, "GET", "/api/v0/sessions", nil)
		r.Header.Set("Authorization", auth)
		if auth == "" {
			r.Header.Del("Authorization")
		}
		resp, body := do(t, r)
		if resp.StatusCode != 401 {
			t.Errorf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
		noToken(t, resp, body)
	}
	// A token of a turn's shape that is not live ended with its turn, and
	// the 401 says so; any other wrong token gets the plain 401.
	for auth, ended := range map[string]bool{
		"Bearer abt_" + strings.Repeat("0a", 32): true,
		"Bearer abt_" + strings.Repeat("0a", 31): false,
		"Bearer abt_" + strings.Repeat("zz", 32): false,
		"Bearer abr_r.a.1.wrong":                 false,
	} {
		r := e.req(t, "GET", "/api/v0/sessions", nil)
		r.Header.Set("Authorization", auth)
		resp, body := do(t, r)
		if resp.StatusCode != 401 || strings.Contains(body, turnTokenEnded) != ended {
			t.Errorf("auth %q: %d %s, want ended %v", auth, resp.StatusCode, body, ended)
		}
	}
	// Unknown paths too: no probing of the namespace without the token.
	r := e.req(t, "GET", "/api/v1/events", nil)
	r.Header.Del("Authorization")
	if resp, _ := do(t, r); resp.StatusCode != 401 {
		t.Errorf("unknown path without token: %d", resp.StatusCode)
	}
	resp, body := do(t, e.req(t, "GET", "/api/v0/sessions", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("with token: %d %s", resp.StatusCode, body)
	}
	noToken(t, resp, body)
	// Not in any stored event either.
	do(t, e.req(t, "POST", "/api/v0/sessions", strings.NewReader(`{"agent":"claude","folder":"/x"}`)))
	recs, _ := e.store.After(0, store.Filter{}, 100)
	for _, rec := range recs {
		if strings.Contains(string(rec.Data), token) {
			t.Fatalf("token stored in an event: %s", rec.Data)
		}
	}
}

func TestHostChecked(t *testing.T) {
	e := newEnv(t)
	for host, want := range map[string]int{
		"127.0.0.1:" + e.port:        200,
		"localhost:" + e.port:        200,
		"evil.example:" + e.port:     403,
		"127.0.0.1":                  403,
		"localhost:1":                403,
		"127.0.0.1.nip.io:" + e.port: 403,
		"[::1]:" + e.port:            200,
		"[::1]":                      403,
		"[::2]:" + e.port:            403,
	} {
		r := e.req(t, "GET", "/api/v0/sessions", nil)
		r.Host = host
		resp, body := do(t, r)
		if resp.StatusCode != want {
			t.Errorf("Host %q: %d, want %d", host, resp.StatusCode, want)
		}
		noToken(t, resp, body)
	}
}

// With AnyHost, the service is beyond loopback: any Host is served, and the
// token is still needed on every API call.
func TestAnyHost(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, AnyHost: true, BaseURL: "http://127.0.0.1:" + port})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	defer func() { srv.Close(); st.Close() }()
	for _, host := range []string{"127.0.0.1:" + port, "127.0.0.1:8788", "localhost:1", "[::1]", "myvm.example.com", "evil.example:" + port} {
		for auth, want := range map[string]int{"Bearer " + token: 200, "": 401, "Bearer wrong": 401} {
			r, _ := http.NewRequest("GET", srv.URL+"/api/v0/sessions", nil)
			r.Host = host
			if auth != "" {
				r.Header.Set("Authorization", auth)
			}
			resp, body := do(t, r)
			if resp.StatusCode != want {
				t.Errorf("Host %q, auth sent %v: %d, want %d", host, auth != "", resp.StatusCode, want)
			}
			noToken(t, resp, body)
		}
	}
}

// Hosts adds names a loopback service answers to, each with its own port
// only; every other name is still refused, and the token still needed.
func TestMoreHosts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if _, err := New(Config{Store: st, Token: token, Port: port, Hosts: []string{"vm.lan:1"}}); err == nil {
		t.Fatal("a host with a port was taken")
	}
	a, err := New(Config{Store: st, Token: token, Port: port, Hosts: []string{"host.lima.internal"}, BaseURL: "http://127.0.0.1:" + port})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	defer func() { srv.Close(); st.Close() }()
	for host, want := range map[string]int{
		"host.lima.internal:" + port: 200,
		"localhost:" + port:          200,
		"host.lima.internal":         403,
		"host.lima.internal:1":       403,
		"evil.example:" + port:       403,
	} {
		r, _ := http.NewRequest("GET", srv.URL+"/api/v0/sessions", nil)
		r.Host = host
		r.Header.Set("Authorization", "Bearer "+token)
		if resp, body := do(t, r); resp.StatusCode != want {
			t.Errorf("Host %q: %d, want %d", host, resp.StatusCode, want)
		} else {
			noToken(t, resp, body)
		}
	}
	r, _ := http.NewRequest("GET", srv.URL+"/api/v0/sessions", nil)
	r.Host = "host.lima.internal:" + port
	if resp, _ := do(t, r); resp.StatusCode != 401 {
		t.Errorf("no token from another host: %d", resp.StatusCode)
	}
}

func TestPreflightOnlyForAllowedOrigin(t *testing.T) {
	e := newEnv(t)
	pre := func(o, path string) *http.Response {
		r, _ := http.NewRequest("OPTIONS", e.srv.URL+path, nil)
		r.Header.Set("Origin", o)
		r.Header.Set("Access-Control-Request-Method", "GET")
		r.Header.Set("Access-Control-Request-Headers", "authorization")
		resp, _ := do(t, r)
		return resp
	}
	resp := pre(origin, "/api/v0/events")
	if resp.StatusCode/100 != 2 ||
		resp.Header.Get("Access-Control-Allow-Origin") != origin ||
		resp.Header.Get("Access-Control-Allow-Methods") != "GET, POST, PUT, PATCH, DELETE" ||
		resp.Header.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type, If-Match" {
		t.Fatalf("allowed preflight: %d %v", resp.StatusCode, resp.Header)
	}
	for _, o := range []string{"http://evil.example", "http://localhost:5174", "null"} {
		resp := pre(o, "/api/v0/events")
		if resp.StatusCode != 403 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("origin %q: %d allow-origin %q", o, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
	}
}

// Without -origin every origin may call the API: the preflight is answered
// with "*" and names Authorization, since "*" in Allow-Headers does not cover
// it. The token is still required on the request itself.
func TestAnyOriginWithoutConfiguredOrigin(t *testing.T) {
	e := newEnvOrigin(t, filepath.Join(t.TempDir(), "events.db"), "")
	for _, o := range []string{"http://example.test", "https://cockpit.example:9443", "null"} {
		r, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api/v0/events", nil)
		r.Header.Set("Origin", o)
		r.Header.Set("Access-Control-Request-Method", "GET")
		r.Header.Set("Access-Control-Request-Headers", "authorization")
		resp, _ := do(t, r)
		if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
			resp.Header.Get("Access-Control-Allow-Methods") != "GET, POST, PUT, PATCH, DELETE" ||
			resp.Header.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type, If-Match" ||
			resp.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("preflight from %q: %d %v", o, resp.StatusCode, resp.Header)
		}
	}
	// A preflight outside /api/ is still refused.
	r, _ := http.NewRequest("OPTIONS", e.srv.URL+"/docs", nil)
	r.Header.Set("Origin", "http://example.test")
	if resp, _ := do(t, r); resp.StatusCode != 403 {
		t.Errorf("preflight to /docs: %d", resp.StatusCode)
	}

	r = e.req(t, "GET", "/api/v0/sessions", nil)
	r.Header.Del("Authorization")
	r.Header.Set("Origin", "http://example.test")
	resp, body := do(t, r)
	if resp.StatusCode != 401 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("no token: %d %v", resp.StatusCode, resp.Header)
	}
	noToken(t, resp, body)

	r = e.req(t, "GET", "/api/v0/sessions", nil)
	r.Header.Set("Origin", "http://example.test")
	if resp, _ := do(t, r); resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		resp.Header.Get("Vary") != "" {
		t.Errorf("with token: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestAllowOriginOnErrorsAndStreams(t *testing.T) {
	e := newEnv(t)
	check := func(name string, r *http.Request, want int) {
		t.Helper()
		r.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want || resp.Header.Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("%s: %d allow-origin %q", name, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
	}
	r := e.req(t, "GET", "/api/v0/sessions", nil)
	r.Header.Del("Authorization")
	check("401", r, 401)
	check("404", e.req(t, "GET", "/api/v0/nothing", nil), 404)
	check("409", e.req(t, "GET", "/api/v0/events?store=st_other&after=0", nil), 409)
	check("405", e.req(t, "DELETE", "/api/v0/sessions", nil), 405)
	check("200", e.req(t, "GET", "/api/v0/sessions", nil), 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	check("stream", e.req(t, "GET", "/api/v0/events", nil).WithContext(ctx), 200)

	// Another origin gets no Allow-Origin, even on success.
	r = e.req(t, "GET", "/api/v0/sessions", nil)
	r.Header.Set("Origin", "http://evil.example")
	resp, _ := do(t, r)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("allow-origin sent to another origin")
	}
}

// Reserved sees only plain paths: cmd/agentbus refuses dot, empty and encoded
// segments before routing (TestOddPathsRefusedBeforeRouting there).
func TestReserved(t *testing.T) {
	for p, want := range map[string]bool{
		"/api":                 true,
		"/api/":                true,
		"/api/v0/sessions":     true,
		"/api/v1/events":       true,
		"/s/abc/api":           true,
		"/s/abc/api/v0/events": true,
		"/s/a.b/api/x":         true,
		"/v1/messages":         false,
		"/s/abc/v1/messages":   false,
		"/apis":                false,
		"/s/abc/apix":          false,
		"/v1/api":              false,
		"/s/api":               false,
	} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:1"+p, nil)
		if got := Reserved(r); got != want {
			t.Errorf("Reserved(%q) = %v, want %v", p, got, want)
		}
	}
}

type sse struct {
	resp   *http.Response
	lines  chan string
	cancel context.CancelFunc
}

func openStream(t *testing.T, e *env, query string) *sse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := http.DefaultClient.Do(e.req(t, "GET", "/api/v0/events"+query, nil).WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream %s: %d %s", query, resp.StatusCode, b)
	}
	s := &sse{resp: resp, lines: make(chan string, 1000), cancel: cancel}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
		close(s.lines)
	}()
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return s
}

// ids reads "id:" lines until n are seen or the wait ends, and returns them.
func (s *sse) ids(n int, wait time.Duration) []string {
	var out []string
	deadline := time.After(wait)
	for len(out) < n {
		select {
		case l, ok := <-s.lines:
			if !ok {
				return out
			}
			if v, ok := strings.CutPrefix(l, "id: "); ok {
				out = append(out, v)
			}
		case <-deadline:
			return out
		}
	}
	return out
}

func emit(t *testing.T, st *store.Store, kind event.Kind, session string) int64 {
	t.Helper()
	out, ok, err := st.Append(event.Event{ID: fmt.Sprint(time.Now().UnixNano()), Kind: kind, Session: session, Time: time.Now()})
	if err != nil || !ok {
		t.Fatal(err)
	}
	return out.Seq
}

func snapshot(t *testing.T, e *env) store.Snapshot {
	t.Helper()
	resp, body := do(t, e.req(t, "GET", "/api/v0/sessions", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("sessions: %d %s", resp.StatusCode, body)
	}
	var s store.Snapshot
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStreamFrameAndPing(t *testing.T) {
	e := newEnv(t)
	seq := emit(t, e.store, event.ModelRequested, "a")
	s := openStream(t, e, fmt.Sprintf("?store=%s&after=0", e.store.ID()))
	want := []string{fmt.Sprintf("id: %s/%d", e.store.ID(), seq), "event: model.requested"}
	for _, w := range want {
		if l := <-s.lines; l != w {
			t.Fatalf("line %q, want %q", l, w)
		}
	}
	if l := <-s.lines; !strings.HasPrefix(l, `data: {"seq":1,`) {
		t.Fatalf("data line %q", l)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case l := <-s.lines:
			if l == ": ping" {
				return
			}
		case <-deadline:
			t.Fatal("no ping")
		}
	}
}

func TestStreamWithoutCursorStartsWithNew(t *testing.T) {
	e := newEnv(t)
	emit(t, e.store, event.ModelRequested, "a")
	s := openStream(t, e, "")
	seq := emit(t, e.store, event.ModelRequested, "a")
	if got := s.ids(1, time.Second); len(got) != 1 || got[0] != fmt.Sprintf("%s/%d", e.store.ID(), seq) {
		t.Fatalf("got %v", got)
	}
}

func TestWrongStoreGets409(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/v0/events?store=st_other&after=5", "/api/v0/sessions/abc/events?store=st_other&after=5"} {
		// Bounded: a stream that wrongly starts would otherwise never end.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		resp, body := do(t, e.req(t, "GET", p, nil).WithContext(ctx))
		var m map[string]string
		_ = json.Unmarshal([]byte(body), &m)
		if resp.StatusCode != 409 || m["store"] != e.store.ID() {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
	for _, p := range []string{"/api/v0/events?store=" + e.store.ID(), "/api/v0/events?after=1", "/api/v0/events?store=x&after=-1"} {
		if resp, _ := do(t, e.req(t, "GET", p, nil)); resp.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", p, resp.StatusCode)
		}
	}
}

func TestSessionEventsPaged(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		emit(t, e.store, event.ModelRequested, "a")
		emit(t, e.store, event.ModelRequested, "b")
	}
	page := func(q string) (seqs []int64, next *int64) {
		resp, body := do(t, e.req(t, "GET", "/api/v0/sessions/a/events"+q, nil))
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", q, resp.StatusCode, body)
		}
		var out struct {
			Store     string        `json:"store"`
			Events    []event.Event `json:"events"`
			NextAfter *int64        `json:"next_after"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.Store != e.store.ID() {
			t.Fatalf("%s: %s", q, body)
		}
		for _, ev := range out.Events {
			if ev.Session != "a" {
				t.Fatalf("event of session %q", ev.Session)
			}
			seqs = append(seqs, ev.Seq)
		}
		return seqs, out.NextAfter
	}
	seqs, next := page("?limit=3")
	if fmt.Sprint(seqs) != "[1 3 5]" || next == nil || *next != 5 {
		t.Fatalf("page 1: %v next %v", seqs, next)
	}
	seqs, next = page(fmt.Sprintf("?store=%s&after=%d&limit=3", e.store.ID(), *next))
	if fmt.Sprint(seqs) != "[7 9]" || next != nil {
		t.Fatalf("page 2: %v next %v", seqs, next)
	}
	for _, q := range []string{"?limit=0", "?limit=10001", "?limit=x"} {
		if resp, _ := do(t, e.req(t, "GET", "/api/v0/sessions/a/events"+q, nil)); resp.StatusCode != 400 {
			t.Errorf("%s: %d", q, resp.StatusCode)
		}
	}
}

// seenSession is a session the store knows from gateway traffic.
func seenSession(t *testing.T, e *env) string {
	t.Helper()
	emit(t, e.store, event.ModelRequested, "seen1")
	return "seen1"
}

func TestUnknownAPIPathIs404(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api", "/api/", "/api/v1/events", "/s/abc/api/v0/sessions", "/api/v0/sessions/a/other", "/api/v0/sessions/a.b/events"} {
		if resp, _ := do(t, e.req(t, "GET", p, nil)); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestHostIPv6Loopback(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback on this machine")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	a, err := New(Config{Store: st, Token: token, Port: port, BaseURL: "http://[::1]:" + port})
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: a}}
	srv.Start()
	defer func() { srv.Close(); st.Close() }()
	r, _ := http.NewRequest("GET", "http://[::1]:"+port+"/api/v0/sessions", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if resp, body := do(t, r); resp.StatusCode != 200 {
		t.Fatalf("Host [::1]:%s: %d %s", port, resp.StatusCode, body)
	}
}

// admitted opens a request and reports its status. A 200 stays open until
// the returned cancel is called.
func admitted(t *testing.T, e *env, path string) (int, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := http.DefaultClient.Do(e.req(t, "GET", path, nil).WithContext(ctx))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode == 429 && strings.Contains(string(body), token) {
			t.Fatal("token in 429 body")
		}
		return resp.StatusCode, func() {}
	}
	return 200, func() { cancel(); resp.Body.Close() }
}

func testAdmissionBound(t *testing.T, e *env, path string, bound int) {
	t.Helper()
	var open []func()
	defer func() {
		for _, c := range open {
			c()
		}
	}()
	for i := 0; i < bound; i++ {
		code, c := admitted(t, e, path)
		if code != 200 {
			t.Fatalf("request %d of %d: %d", i+1, bound, code)
		}
		open = append(open, c)
	}
	if code, c := admitted(t, e, path); code != 429 {
		c()
		t.Fatalf("request past the bound of %d: %d, want 429", bound, code)
	}
	open[0]()
	open = open[1:]
	// The server notices the closed connection a moment later.
	deadline := time.Now().Add(2 * time.Second)
	for {
		code, c := admitted(t, e, path)
		if code == 200 {
			open = append(open, c)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("closing one did not admit another: %d", code)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamsBounded(t *testing.T) {
	e := newEnv(t)
	testAdmissionBound(t, e, "/api/v0/events", MaxStreams)
}

func TestSessionPagesStayUnderByteBudget(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("x", 1<<20) // 1 MiB each
	for i := 0; i < 20; i++ {
		if _, _, err := e.store.Append(event.Event{ID: fmt.Sprint("b", i), Kind: event.ModelResponded,
			Session: "a", Time: time.Now(), Text: big}); err != nil {
			t.Fatal(err)
		}
	}
	var got []int64
	q := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("paging does not move forward")
		}
		resp, body := do(t, e.req(t, "GET", "/api/v0/sessions/a/events"+q, nil))
		if resp.StatusCode != 200 {
			t.Fatalf("%d", resp.StatusCode)
		}
		if len(body) > store.PageBytes+64<<10 {
			t.Fatalf("a page of %d bytes, over the %d budget", len(body), store.PageBytes)
		}
		var out struct {
			Events    []event.Event `json:"events"`
			NextAfter *int64        `json:"next_after"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		for _, ev := range out.Events {
			got = append(got, ev.Seq)
		}
		if out.NextAfter == nil {
			break
		}
		if len(out.Events) == 0 || *out.NextAfter != out.Events[len(out.Events)-1].Seq {
			t.Fatalf("next_after %d is not the last included seq", *out.NextAfter)
		}
		q = fmt.Sprintf("?store=%s&after=%d", e.store.ID(), *out.NextAfter)
	}
	var want []int64
	for i := int64(1); i <= 20; i++ {
		want = append(want, i)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("seqs across pages %v", got)
	}
}

func postEvent(t *testing.T, e *env, body string) (int, map[string]any, string) {
	t.Helper()
	resp, raw := do(t, e.req(t, "POST", "/api/v0/events", strings.NewReader(body)))
	noToken(t, resp, raw)
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return resp.StatusCode, out, raw
}

func TestCustomEventStoredAndStreamed(t *testing.T) {
	e := newEnv(t)
	id := seenSession(t, e)
	s := openStream(t, e, "?store="+e.store.ID()+"&after="+fmt.Sprint(e.store.Last()))
	code, out, raw := postEvent(t, e, `{"kind":"x.review.finding","session":"`+id+`","turn":"t1",
		"text":"line 12 reads past the end","data":{"file":"a.go","line":12}}`)
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, raw)
	}
	seq := int64(out["seq"].(float64))
	evID, _ := out["id"].(string)
	if seq != e.store.Last() || !strings.HasPrefix(evID, "evt_") {
		t.Fatalf("reply %s, store last %d", raw, e.store.Last())
	}
	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case l := <-s.lines:
			if l != "" {
				got = append(got, l)
			}
		case <-deadline:
			t.Fatalf("stream gave %q", got)
		}
	}
	if got[0] != fmt.Sprintf("id: %s/%d", e.store.ID(), seq) || got[1] != "event: x.review.finding" {
		t.Fatalf("stream frame %q", got)
	}
	var ev event.Event
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got[2], "data: ")), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Seq != seq || ev.ID != evID || ev.Kind != "x.review.finding" || ev.Session != id || ev.Turn != "t1" ||
		ev.Vendor != "program" || ev.Text != "line 12 reads past the end" || ev.Time.IsZero() ||
		string(ev.Data) != `{"file":"a.go","line":12}` {
		t.Fatalf("stored event %+v data %s", ev, ev.Data)
	}
	// The default session needs no earlier event.
	if code, _, raw := postEvent(t, e, `{"kind":"x.review.done","session":"default"}`); code != http.StatusCreated {
		t.Fatalf("default session: %d %s", code, raw)
	}
}

func TestCustomEventSessionSeenOnlyThroughGateway(t *testing.T) {
	e := newEnv(t)
	emit(t, e.store, event.ModelRequested, "gw1")
	if code, _, raw := postEvent(t, e, `{"kind":"x.review.finding","session":"gw1"}`); code != http.StatusCreated {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestCustomEventRefusals(t *testing.T) {
	e := newEnv(t)
	id := seenSession(t, e)
	const (
		badKind = `kind must match ^x\.[a-z0-9][a-z0-9_-]{0,31}\.[a-z0-9][a-z0-9_.-]{0,63}$`
		badBody = "the body must be one JSON object with kind and session, and optionally turn, text and data, at most 128 KiB"
		badSize = "turn is at most 128 bytes and text at most 64 KiB"
		badData = "data must be a JSON object, at most 64 KiB"
		badSess = "session must be a session id or default"
	)
	ev := func(kind, extra string) string {
		return `{"kind":"` + kind + `","session":"` + id + `"` + extra + `}`
	}
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"framework kind", ev("turn.completed", ""), 400, badKind},
		{"session kind", ev("session.closed", ""), 400, badKind},
		{"empty program", ev("x..a", ""), 400, badKind},
		{"no name", ev("x.review.", ""), 400, badKind},
		{"upper case", ev("x.Review.finding", ""), 400, badKind},
		{"trailing newline", ev(`x.review.finding\n`, ""), 400, badKind},
		{"program over 32", ev("x."+strings.Repeat("a", 33)+".b", ""), 400, badKind},
		{"name over 64", ev("x.a."+strings.Repeat("b", 65), ""), 400, badKind},
		{"no kind", `{"session":"` + id + `"}`, 400, badKind},
		{"unknown session", `{"kind":"x.review.finding","session":"nosuch"}`, 404, "no such session"},
		{"no session", `{"kind":"x.review.finding"}`, 400, badSess},
		{"session with slash", `{"kind":"x.review.finding","session":"a/b"}`, 400, badSess},
		{"turn over 128", ev("x.review.finding", `,"turn":"`+strings.Repeat("t", 129)+`"`), 400, badSize},
		{"text over 64 KiB", ev("x.review.finding", `,"text":"`+strings.Repeat("t", 64<<10+1)+`"`), 400, badSize},
		{"data over 64 KiB", ev("x.review.finding", `,"data":{"a":"`+strings.Repeat("d", 64<<10)+`"}`), 400, badData},
		{"data an array", ev("x.review.finding", `,"data":[1]`), 400, badData},
		{"data null", ev("x.review.finding", `,"data":null`), 400, badData},
		{"data a string", ev("x.review.finding", `,"data":"x"`), 400, badData},
		{"body over 128 KiB", ev("x.review.finding", `,"text":"`+strings.Repeat("t", 60<<10)+`","data":{"a":"`+strings.Repeat("d", 60<<10)+`"},"turn":"`+strings.Repeat("u", 10<<10)+`"`), 400, badBody},
		{"unknown field", ev("x.review.finding", `,"vendor":"anthropic"`), 400, badBody},
		{"seq supplied", ev("x.review.finding", `,"seq":1`), 400, badBody},
		{"not an object", `"x.review.finding"`, 400, badBody},
		{"trailing data", ev("x.review.finding", "") + "{}", 400, badBody},
	} {
		before := e.store.Last()
		code, out, raw := postEvent(t, e, tc.body)
		if code != tc.status || out["error"] != tc.want {
			t.Errorf("%s: %d %.200s, want %d %q", tc.name, code, raw, tc.status, tc.want)
		}
		if e.store.Last() != before {
			t.Errorf("%s: a refused event was stored", tc.name)
		}
	}
	// The bounds themselves are allowed.
	ok := ev("x."+strings.Repeat("a", 32)+"."+strings.Repeat("b", 64), `,"turn":"`+strings.Repeat("t", 128)+
		`","text":"`+strings.Repeat("t", 60<<10)+`","data":{"a":"`+strings.Repeat("d", 60<<10)+`"}`)
	if code, _, raw := postEvent(t, e, ok); code != http.StatusCreated {
		t.Fatalf("an event at the bounds: %d %.200s", code, raw)
	}
}
