package cockpit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// browserAccept is what Chrome sends when it opens a page.
const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"

func serve(h http.Handler, method, p string) *httptest.ResponseRecorder {
	return serveAccept(h, method, p, browserAccept)
}

func serveAccept(h http.Handler, method, p, accept string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, p, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	h.ServeHTTP(w, r)
	return w
}

func TestPackedFiles(t *testing.T) {
	files := fstest.MapFS{
		"index.html":            {Data: []byte("<!doctype html><html><head><title>AgentBus</title></head><body></body></html>")},
		"assets/index-ab12.js":  {Data: []byte("console.log(1)")},
		"assets/index-ab12.css": {Data: []byte("body{}")},
		"favicon.svg":           {Data: []byte("<svg/>")},
		".keep":                 {Data: nil},
	}
	h := New(files)
	for _, tc := range []struct {
		path, ctype, body string
		code              int
	}{
		{"/", "text/html; charset=utf-8", `<head><meta name="agentbus" content="served"><title>`, 200},
		{"/?t=abc", "text/html; charset=utf-8", `content="served"`, 200},
		{"/index.html", "text/html; charset=utf-8", `content="served"`, 200},
		// The cockpit's own pages: index.html.
		{"/settings", "text/html; charset=utf-8", `content="served"`, 200},
		{"/runners", "text/html; charset=utf-8", `content="served"`, 200},
		{"/assets/index-ab12.js", "text/javascript; charset=utf-8", "console.log(1)", 200},
		{"/assets/index-ab12.css", "text/css; charset=utf-8", "body{}", 200},
		{"/favicon.svg", "image/svg+xml", "<svg/>", 200},
		// A missing file is not the page.
		{"/assets/gone-1.js", "", "no such cockpit path", 404},
		{"/favicon.ico", "", "no such cockpit path", 404},
		{"/.keep", "", "no such cockpit path", 404},
	} {
		w := serve(h, "GET", tc.path)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("GET %s: %d %q; want %d with %q", tc.path, w.Code, w.Body.String(), tc.code, tc.body)
		}
		if tc.ctype != "" && w.Header().Get("Content-Type") != tc.ctype {
			t.Errorf("GET %s: Content-Type %q, want %q", tc.path, w.Header().Get("Content-Type"), tc.ctype)
		}
		if tc.code == 200 && w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: no nosniff", tc.path)
		}
	}
	// Serves says the same as the answers: the router answers the rest.
	for p, want := range map[string]bool{"/": true, "/settings": true, "/assets/index-ab12.js": true, "/favicon.ico": false, "/.keep": false} {
		r := httptest.NewRequest("GET", p, nil)
		r.Header.Set("Accept", browserAccept)
		if h.Serves(r) != want {
			t.Errorf("Serves GET %s: %v", p, !want)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "OPTIONS"} {
		if h.Serves(httptest.NewRequest(m, "/", nil)) {
			t.Errorf("Serves %s /", m)
		}
	}
	w := serve(h, "GET", "/")
	if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("X-Frame-Options") != "DENY" ||
		w.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" {
		t.Errorf("the page's headers: %v", w.Header())
	}
	// A caller that is not a browser opening a page gets no page for a
	// path the cockpit has no file at: an agent's wrong path stays 404.
	for _, accept := range []string{"", "*/*", "application/json", "text/event-stream"} {
		if w := serveAccept(h, "GET", "/v2/messages", accept); w.Code != 404 {
			t.Errorf("GET /v2/messages with Accept %q: %d", accept, w.Code)
		}
		if w := serveAccept(h, "GET", "/", accept); w.Code != 200 {
			t.Errorf("GET / with Accept %q: %d", accept, w.Code)
		}
		if w := serveAccept(h, "GET", "/assets/index-ab12.js", accept); w.Code != 200 {
			t.Errorf("GET an asset with Accept %q: %d", accept, w.Code)
		}
	}
	if w := serve(h, "HEAD", "/settings"); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD /settings: %d, %d bytes", w.Code, w.Body.Len())
	}
	if got := serve(h, "GET", "/assets/index-ab12.js").Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("an asset's Cache-Control %q", got)
	}
}

func TestNotPacked(t *testing.T) {
	h := New(fstest.MapFS{".keep": {}})
	w := serve(h, "GET", "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "The cockpit is not packed in this build") {
		t.Fatalf("GET /: %d %s", w.Code, w.Body.String())
	}
	if w := serve(h, "GET", "/settings"); w.Code != 404 {
		t.Errorf("GET /settings without the cockpit: %d", w.Code)
	}
}

// A source build has only dist/.keep; a packed one, the cockpit.
func TestEmbeddedFiles(t *testing.T) {
	want := "not packed"
	if Packed() {
		want = `content="served"`
	}
	if w := serve(Handler(), "GET", "/"); !strings.Contains(w.Body.String(), want) {
		t.Fatalf("GET / (packed %v): %s", Packed(), w.Body.String())
	}
}
