package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The docs are answered by the service itself, without the token, and never
// forwarded to a vendor.
func TestDocsAnsweredLocally(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Errorf("upstream reached: %s", r.URL.Path)
	}))
	defer upstream.Close()
	home := t.TempDir()
	svc := startService(t, home, upstream.URL)
	for p, want := range map[string]int{
		"/docs": 200, "/docs/llms.txt": 200, "/docs/redoc.standalone.js": 200, "/api/v0/openapi.json": 200,
		"/docs/": 404, "/docs/v1/messages": 404, "/docs/nothing": 404,
	} {
		for _, auth := range []string{"", "Bearer wrong"} {
			if code := rawGet(t, svc.addr, p, auth); code != want {
				t.Errorf("%s (auth %q): %d, want %d", p, auth, code, want)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream reached %d times", hits.Load())
	}
}
