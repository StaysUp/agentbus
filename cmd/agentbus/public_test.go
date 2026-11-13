package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/api"
	"github.com/staysup/agentbus/store"
)

// deadlineRecorder notes the deadlines a handler sets through
// http.ResponseController.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.read = append(d.read, t)
	return nil
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.write = append(d.write, t)
	return nil
}

// Every reply served without a token goes through boundPublic: a read and a
// write deadline before it, the write deadline left set after the handler
// returns (net/http still flushes the reply then), and Connection: close, so
// no later request shares the connection or inherits the deadline. A token's
// request, a stream among them, gets none of these from the router.
func TestEveryPublicReplyIsBounded(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := api.New(api.Config{Store: st, Token: "tok-public-test", Port: "8787"})
	if err != nil {
		t.Fatal(err)
	}
	ck := fakeSite{func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }}
	for _, withCockpit := range []bool{true, false} {
		v := vendors{}
		if withCockpit {
			v.cockpit = ck
		}
		h := route(a, v, false)
		for _, tc := range []struct {
			method, path string
			bounded      bool
		}{
			{"GET", "/docs", true},
			{"GET", "/docs/redoc.standalone.js", true},
			{"GET", "/docs/llms.txt", true},
			{"GET", "/docs/runner", true},
			{"GET", "/api/v0/openapi.json", true},
			{"GET", "/api/v0/runner/openapi.json", true},
			{"POST", "/api/v0/signin", true},
			{"GET", "/", true},
			{"GET", "/settings", withCockpit},
			{"GET", "/api/v0/sessions", false},
			{"GET", "/api/v0/events", false},
		} {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Host = "127.0.0.1:8787"
			r.Header.Set("Accept", "text/html")
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(w, r)
			got := len(w.write) > 0
			if got != tc.bounded {
				t.Errorf("cockpit %v: %s %s: bounded %v, want %v", withCockpit, tc.method, tc.path, got, tc.bounded)
				continue
			}
			closes := w.Header().Get("Connection") == "close"
			if !got {
				// A refusal (localError) closes its connection on its own;
				// a token's API request must not.
				if closes && strings.HasPrefix(tc.path, "/api/") {
					t.Errorf("%s %s: a token's reply was sent with Connection: close", tc.method, tc.path)
				}
				continue
			}
			if len(w.read) == 0 || w.read[0].IsZero() {
				t.Errorf("%s %s: read deadlines %v", tc.method, tc.path, w.read)
			}
			for _, d := range w.write {
				if d.IsZero() {
					t.Errorf("%s %s: the write deadline was cleared (%v); the flush after the handler is unbounded", tc.method, tc.path, w.write)
					break
				}
			}
			if !closes {
				t.Errorf("%s %s: Connection %q, want close", tc.method, tc.path, w.Header().Get("Connection"))
			}
		}
	}
}
