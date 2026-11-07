//go:build integration

package api

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

func TestSnapshotThenStreamMissesNothingRepeatsNothing(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		emit(t, e.store, event.ModelRequested, "a")
	}
	snap := snapshot(t, e)
	if snap.Seq != 3 {
		t.Fatalf("snapshot seq %d", snap.Seq)
	}
	// Lands between the two calls.
	between := emit(t, e.store, event.ModelRequested, "a")
	s := openStream(t, e, fmt.Sprintf("?store=%s&after=%d", snap.Store, snap.Seq))
	// Lands while the stream is live, some of them as a burst.
	var want []string
	want = append(want, fmt.Sprintf("%s/%d", snap.Store, between))
	for i := 0; i < 20; i++ {
		want = append(want, fmt.Sprintf("%s/%d", snap.Store, emit(t, e.store, event.ToolRequested, "b")))
	}
	got := s.ids(len(want)+1, time.Second)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stream ids\n got %v\nwant %v", got, want)
	}
}

func TestStreamSessionFilterIncludesDrops(t *testing.T) {
	e := newEnv(t)
	s := openStream(t, e, fmt.Sprintf("?store=%s&after=0&session=a", e.store.ID()))
	a1 := emit(t, e.store, event.ModelRequested, "a")
	emit(t, e.store, event.ModelRequested, "b")
	d, _, _ := e.store.Append(event.Event{ID: "d1", Kind: event.EventsDropped, Dropped: 1, Sessions: []string{"a"}})
	e.store.Append(event.Event{ID: "d2", Kind: event.EventsDropped, Dropped: 1, Sessions: []string{"b"}})
	all, _, _ := e.store.Append(event.Event{ID: "d3", Kind: event.EventsDropped, Dropped: 1, AllSessions: true})
	id := e.store.ID()
	want := fmt.Sprint([]string{fmt.Sprintf("%s/%d", id, a1), fmt.Sprintf("%s/%d", id, d.Seq), fmt.Sprintf("%s/%d", id, all.Seq)})
	if got := fmt.Sprint(s.ids(4, 500*time.Millisecond)); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestStalledBodyIsCutOff(t *testing.T) {
	e := newEnv(t)
	c, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /api/v0/events HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", e.port, token)
	start := time.Now()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body within 3s: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 || time.Since(start) > 2*time.Second {
		t.Fatalf("status %d after %v", resp.StatusCode, time.Since(start))
	}
	// Nothing was stored.
	if recs, _ := e.store.After(0, store.Filter{}, 10); len(recs) != 0 {
		t.Fatalf("stored %d events", len(recs))
	}
}

func TestStreamOutlivesBodyTimeout(t *testing.T) {
	e := newEnv(t) // BodyTimeout is 300ms here
	s := openStream(t, e, "")
	time.Sleep(600 * time.Millisecond)
	seq := emit(t, e.store, event.ModelRequested, "a")
	if got := s.ids(1, time.Second); len(got) != 1 || got[0] != fmt.Sprintf("%s/%d", e.store.ID(), seq) {
		t.Fatalf("stream after the body timeout: %v", got)
	}
}
