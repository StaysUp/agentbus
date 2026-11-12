package api

import (
	"bytes"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

func getNoToken(t *testing.T, e *env, path, host string) (*http.Response, string) {
	t.Helper()
	r := e.req(t, "GET", path, nil)
	r.Header.Del("Authorization")
	if host != "" {
		r.Host = host
	}
	return do(t, r)
}

// The description, the viewer and the guide for agents
// are served without the token, from the binary, with no session data.
func TestDocsServedWithoutToken(t *testing.T) {
	e := newEnv(t)
	emit(t, e.store, "turn.started", "secret-session")
	desc, _ := spec()
	guide, _ := os.ReadFile("../docs/llms.txt")
	viewer, _ := os.ReadFile("viewer/redoc.standalone.js")
	for _, tc := range []struct {
		path, ctype string
		want        []byte
	}{
		{"/api/v0/openapi.json", "application/json", desc},
		{"/docs", "text/html; charset=utf-8", nil},
		{"/docs/llms.txt", "text/markdown; charset=utf-8", guide},
		{"/docs/redoc.standalone.js", "text/javascript; charset=utf-8", viewer},
	} {
		resp, body := getNoToken(t, e, tc.path, "")
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d %s", tc.path, resp.StatusCode, body)
			continue
		}
		if got := resp.Header.Get("Content-Type"); got != tc.ctype {
			t.Errorf("%s: Content-Type %q, want %q", tc.path, got, tc.ctype)
		}
		if tc.want != nil && (len(tc.want) == 0 || !bytes.Equal([]byte(body), tc.want)) {
			t.Errorf("%s: not what the service built (%d bytes, want %d)", tc.path, len(body), len(tc.want))
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", tc.path)
		}
		for _, secret := range []string{token, "secret-session", e.store.ID()} {
			if strings.Contains(body, secret) {
				t.Errorf("%s: carries %q", tc.path, secret)
			}
		}
	}
	if len(viewer) < 100000 || !bytes.Contains(viewer[:2000], []byte("exports.Redoc")) {
		t.Errorf("viewer/redoc.standalone.js is not the Redoc bundle (%d bytes)", len(viewer))
	}
}

// The page loads the bundled viewer from this service and names no other
// address, so it works offline, and its policy keeps the browser from
// fetching anything elsewhere.
func TestDocsPageIsLocal(t *testing.T) {
	e := newEnv(t)
	resp, page := getNoToken(t, e, "/docs", "")
	if resp.StatusCode != 200 {
		t.Fatalf("/docs: %d", resp.StatusCode)
	}
	if regexp.MustCompile(`(?i)(https?:)?//[a-z0-9]`).MatchString(page) {
		t.Errorf("the page names an outside address:\n%s", page)
	}
	for _, want := range []string{`<script src="/docs/redoc.standalone.js"></script>`, `spec-url="/api/v0/openapi.json"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	// The menu shows the summaries the headings show, the method and path
	// sit under each heading, and search is off.
	for _, want := range []string{" hide-hostname", " path-in-middle-panel", ` side-nav-style="summary-only"`, " disable-search"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "img-src 'self' data:", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "worker-src") {
		t.Errorf("Content-Security-Policy allows a worker, which only Redoc's search used: %q", csp)
	}
	if strings.Contains(csp, "http") {
		t.Errorf("Content-Security-Policy allows an outside address: %q", csp)
	}
}

// The docs obey the Host check like the rest of the API.
func TestDocsRefusedForWrongHost(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/docs", "/docs/llms.txt", "/docs/redoc.standalone.js", "/api/v0/openapi.json"} {
		resp, body := getNoToken(t, e, p, "evil.example:"+e.port)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with a wrong Host: %d %s, want 403", p, resp.StatusCode, body)
		}
		if strings.Contains(body, "ReDoc") || strings.Contains(body, "openapi") {
			t.Errorf("%s with a wrong Host leaked the content", p)
		}
	}
}

// Only GET and HEAD reach the docs; an unknown path under /docs is 404
// without a token, and the rest of the API still needs one.
func TestDocsMethodsAndUnknownPaths(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/docs", "/docs/llms.txt", "/api/v0/openapi.json"} {
		r := e.req(t, "POST", p, strings.NewReader("{}"))
		r.Header.Del("Authorization")
		if resp, body := do(t, r); resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("POST %s: %d %q %s, want 405 with Allow GET, HEAD", p, resp.StatusCode, resp.Header.Get("Allow"), body)
		}
		r = e.req(t, "HEAD", p, nil)
		r.Header.Del("Authorization")
		if resp, _ := do(t, r); resp.StatusCode != 200 {
			t.Errorf("HEAD %s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := getNoToken(t, e, "/docs/nothing", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/docs/nothing: %d, want 404", resp.StatusCode)
	}
	if resp, _ := getNoToken(t, e, "/api/v0/sessions", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/v0/sessions without a token: %d, want 401", resp.StatusCode)
	}
	if resp, _ := getNoToken(t, e, "/api/v0/openapi.jsonx", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/v0/openapi.jsonx without a token: %d, want 401", resp.StatusCode)
	}
}

func TestReservedDocs(t *testing.T) {
	for p, want := range map[string]bool{"/docs": true, "/docs/": true, "/docs/llms.txt": true,
		"/docsx": false, "/s/abc/docs": false, "/v1/docs": false} {
		r, _ := http.NewRequest("GET", "http://127.0.0.1"+p, nil)
		if got := Reserved(r); got != want {
			t.Errorf("Reserved(%s) = %v, want %v", p, got, want)
		}
	}
}
