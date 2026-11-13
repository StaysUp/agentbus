package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
// write deadline before it, the write deadline cleared after. A token's
// request, a stream among them, gets neither from the router.
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
			if !got {
				continue
			}
			if len(w.read) == 0 || w.read[0].IsZero() || w.write[0].IsZero() {
				t.Errorf("%s %s: deadlines read %v write %v", tc.method, tc.path, w.read, w.write)
			}
			if last := w.write[len(w.write)-1]; !last.IsZero() {
				t.Errorf("%s %s: the write deadline was left set for the connection's next request", tc.method, tc.path)
			}
		}
	}
}
