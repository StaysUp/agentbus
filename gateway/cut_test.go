package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
)

func responded(t *testing.T, evs []event.Event) event.Event {
	t.Helper()
	for _, e := range evs {
		if e.Kind == event.ModelResponded {
			return e
		}
	}
	t.Fatalf("no model.responded in %+v", evs)
	return event.Event{}
}

// A complete body that fails to decode is still an error.
func TestCompleteBadGzipReplyIsAnError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Encoding", "gzip")
		w.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff, 'n', 'o', 't', ' ', 'f', 'l', 'a', 't', 'e'})
	}))
	defer upstream.Close()
	gw, mem := newOpenAIGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	evs := waitFor(t, mem, 3)
	if r := responded(t, evs); !strings.Contains(r.Error, "could not be decoded") {
		t.Fatalf("model.responded = %+v; want a decode error", r)
	}
}
