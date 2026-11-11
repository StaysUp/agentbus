package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/staysup/agentbus/api"
)

// agentbus api sends the turn's token to AGENTBUS_URL, prints the reply, and
// exits 1 on a refusal; it refuses a path outside the API and a missing
// variable without sending anything.
func TestAPICall(t *testing.T) {
	var got struct{ method, path, auth, ctype, body string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.path, got.auth, got.ctype, got.body = r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)
		if strings.HasSuffix(r.URL.Path, "/denied") {
			w.WriteHeader(403)
			io.WriteString(w, `{"error":"no"}`)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	env := map[string]string{"AGENTBUS_URL": srv.URL, "AGENTBUS_TOKEN": "abt_test"}
	call := func(env map[string]string, stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := apiCall(args, func(k string) string { return env[k] }, strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}
	code, out, _ := call(env, "", "post", "/api/v0/turns", `{"agent":"claude","prompt":"hi"}`)
	if code != 0 || out != `{"ok":true}` || got.method != "POST" || got.path != "/api/v0/turns" || got.auth != "Bearer abt_test" ||
		got.ctype != "application/json" || got.body != `{"agent":"claude","prompt":"hi"}` {
		t.Fatalf("POST: %d %q %+v", code, out, got)
	}
	code, out, _ = call(env, "", "GET", "/api/v0/sessions/abc/search?q=a%20b")
	if code != 0 || got.path != "/api/v0/sessions/abc/search?q=a%20b" || got.ctype != "" || got.body != "" {
		t.Fatalf("GET: %d %q %+v", code, out, got)
	}
	code, out, errs := call(env, "", "GET", "/api/v0/denied")
	if code != 1 || out != `{"error":"no"}` || !strings.Contains(errs, "403") {
		t.Fatalf("refused: %d %q %q", code, out, errs)
	}
	if code, _, _ := call(env, `{"from":"stdin"}`, "PUT", "/api/v0/agents/x", "-"); code != 0 || got.body != `{"from":"stdin"}` {
		t.Errorf("stdin body: %d %q", code, got.body)
	}
	// Every method a route takes, PATCH for a session's rename too.
	for _, m := range api.Methods() {
		if code, _, errs := call(env, "", m, "/api/v0/sessions/abc", `{"title":"x"}`); code != 0 || got.method != m {
			t.Errorf("%s: exit %d %q, sent %s", m, code, errs, got.method)
		}
	}
	if !slices.Contains(api.Methods(), "PATCH") {
		t.Errorf("methods %v, want PATCH among them", api.Methods())
	}
	got.path = ""
	for _, args := range [][]string{
		{"GET", "http://elsewhere.example/api/v0/turns"},
		{"GET", "/s/abc/v1/messages"},
		{"TRACE", "/api/v0/turns"},
		{"GET"},
	} {
		if code, _, _ := call(env, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _, errs := call(map[string]string{"AGENTBUS_URL": srv.URL}, "", "GET", "/api/v0/turns"); code != 2 || strings.Contains(errs, "abt_") {
		t.Errorf("no token: %d %q", code, errs)
	}
	if got.path != "" {
		t.Errorf("a refused call reached the service: %s", got.path)
	}
}

// apiLink makes <home>/bin/agentbus-api point at the program, and a later
// start replaces it.
func TestAPILink(t *testing.T) {
	home := t.TempDir()
	quiet := log.New(io.Discard, "", 0)
	dir := apiLink(home, "/opt/one/agentbus", quiet)
	if dir != filepath.Join(home, "bin") {
		t.Fatalf("dir %q", dir)
	}
	if got, err := os.Readlink(filepath.Join(dir, "agentbus-api")); err != nil || got != "/opt/one/agentbus" {
		t.Fatalf("link: %q %v", got, err)
	}
	apiLink(home, "/opt/two/agentbus", quiet)
	if got, _ := os.Readlink(filepath.Join(dir, "agentbus-api")); got != "/opt/two/agentbus" {
		t.Fatalf("replaced link: %q", got)
	}
	if apiLink(home, "", quiet) != "" {
		t.Error("no program, yet a folder")
	}
}
