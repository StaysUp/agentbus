package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/event"
)

func countUnexpected(evs []event.Event) (n int, exchanges map[string]bool) {
	exchanges = map[string]bool{}
	for _, e := range evs {
		if e.Kind == event.SchemaUnexpected {
			n++
			exchanges[e.Exchange] = true
		}
	}
	return n, exchanges
}

// The bound holds per exchange, across the request and the reply together.
func TestUnexpectedCappedPerExchange(t *testing.T) {
	var req, rep []string
	for i := 0; i < 12; i++ {
		req = append(req, fmt.Sprintf(`{"type":"req_kind_%d"}`, i))
		rep = append(rep, fmt.Sprintf(`{"type":"rep_kind_%d"}`, i))
	}
	reply := `{"model":"m","content":[` + strings.Join(rep, ",") + `],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},` + strings.Join(req, ",") + `]}]}`
	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	// turn.completed comes before the reply's schema.unexpected events, so
	// waiting for it saw 14 of 16 once in a parallel run. Wait for the
	// count itself.
	waitForKind(t, mem, event.TurnCompleted)
	deadline := time.Now().Add(2 * time.Second)
	n, exchanges := countUnexpected(mem.Events())
	for n < adapter.MaxUnexpected && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		n, exchanges = countUnexpected(mem.Events())
	}
	if n != adapter.MaxUnexpected || len(exchanges) != 1 {
		t.Fatalf("schema.unexpected = %d in %d exchanges, want %d in 1", n, len(exchanges), adapter.MaxUnexpected)
	}
}

// A reply cut off claims no turn end, but what it showed of its shape is
// still reported: the vendor did send it.
func TestUnexpectedKeptOnCutReply(t *testing.T) {
	stream := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"data: {\"type\":\"content_block_flash\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(stream)+100))
		io.WriteString(w, stream)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err == nil {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	waitForKind(t, mem, event.ModelResponded)
	time.Sleep(50 * time.Millisecond)
	var got []event.Unexpected
	for _, e := range mem.Events() {
		if e.Kind == event.TurnCompleted {
			t.Fatal("a cut body must not end the turn")
		}
		if e.Kind == event.SchemaUnexpected {
			got = append(got, *e.Unexpected)
		}
	}
	if len(got) != 1 || got[0] != (event.Unexpected{Where: "stream.event", Value: "content_block_flash"}) {
		t.Fatalf("schema.unexpected = %+v", got)
	}
}

func waitForKind(t *testing.T, mem *event.Memory, k event.Kind) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range mem.Events() {
			if e.Kind == k {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s in %+v", k, mem.Events())
}

// A cut inside the next stream line makes the adapter fail, and the shapes
// seen before the cut are still reported.
func TestUnexpectedKeptWhenCutLineFailsParse(t *testing.T) {
	stream := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"data: {\"type\":\"new_vendor_event\"}\n\n" +
		"data: {\"type\":\"content_blo"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(stream)+100))
		io.WriteString(w, stream)
	}))
	defer upstream.Close()
	gw, mem := newGateway(t, upstream.URL)
	resp, err := rawClient().Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(okRequest))
	if err == nil {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	waitForKind(t, mem, event.ModelResponded)
	time.Sleep(50 * time.Millisecond)
	var got []event.Unexpected
	failed := false
	for _, e := range mem.Events() {
		if e.Kind == event.ModelResponded && e.Error != "" {
			failed = true
		}
		if e.Kind == event.SchemaUnexpected {
			got = append(got, *e.Unexpected)
		}
	}
	if !failed {
		t.Error("the failed reply was not reported")
	}
	if len(got) != 1 || got[0] != (event.Unexpected{Where: "stream.event", Value: "new_vendor_event"}) {
		t.Fatalf("schema.unexpected = %+v", got)
	}
}
