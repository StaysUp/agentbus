package gateway

import (
	"context"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/event"
)

// fakeDecider stands in for control's approvals API: it answers how a held
// approval was decided. A test sets it, or leaves it pending to force a timeout.
type fakeDecider struct {
	mu      sync.Mutex
	allow   bool
	decided bool
	err     error
	calls   int
}

func (d *fakeDecider) Decided(_ context.Context, _, _ string) (bool, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return d.allow, d.decided, d.err
}

func (d *fakeDecider) set(allow bool) {
	d.mu.Lock()
	d.allow, d.decided = allow, true
	d.mu.Unlock()
}

// heldGuard runs the guard over a streamed reply with a decider and fast hold
// timing, from ctx, and returns the gateway so a test can read the record.
func heldGuard(t *testing.T, ctx context.Context, stream string, ask []string, d Decider) (io.ReadCloser, *Gateway, *event.Memory) {
	t.Helper()
	mem := &event.Memory{}
	g := New(&url.URL{}, anthropic.Adapter{}, mem)
	g.SetDecider(d)
	g.SetHoldTiming(120*time.Millisecond, 30*time.Millisecond, 10*time.Millisecond)
	b := g.guardReply(ctx, &exchange{id: "x1", session: "s1", turn: "t_1"}, Rules{Ask: ask}, nil, nil,
		io.NopCloser(strings.NewReader(stream)), func() {})
	return b, g, mem
}

func decisions(mem *event.Memory) (requested, decided int, decidedText string) {
	for _, e := range mem.Events() {
		switch e.Kind {
		case event.ApprovalRequested:
			requested++
		case event.ApprovalDecided:
			decided++
			decidedText = e.Text
		}
	}
	return
}

// Allow releases the held call unchanged, so the agent runs it this turn, and
// the gateway records no decision (the API that took the answer does).
func TestHoldAllowReleasesCallUnchanged(t *testing.T) {
	d := &fakeDecider{allow: true, decided: true}
	b, g, mem := heldGuard(t, context.Background(), anthropicAskStream, []string{"git push"}, d)
	raw, _ := io.ReadAll(b)
	b.Close()
	out := string(raw)
	if strings.Contains(out, RefusedPrefix) {
		t.Fatalf("an allowed held call was refused:\n%s", out)
	}
	if !strings.Contains(out, "toolu_1") || !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("the allowed call did not pass through:\n%s", out)
	}
	g.Close()
	req, dec, _ := decisions(mem)
	if req != 1 {
		t.Fatalf("approvals requested: %d", req)
	}
	if dec != 0 {
		t.Fatalf("the gateway recorded a decision for a person-answered hold: %d", dec)
	}
}

// Refuse renames the held call, so the agent cannot run it, and the gateway
// records no decision (the API recorded the person's refusal).
func TestHoldRefuseRenamesCall(t *testing.T) {
	d := &fakeDecider{allow: false, decided: true}
	b, g, mem := heldGuard(t, context.Background(), anthropicAskStream, []string{"git push"}, d)
	raw, _ := io.ReadAll(b)
	b.Close()
	if !strings.Contains(string(raw), RefusedPrefix) {
		t.Fatalf("a refused held call was not renamed:\n%s", string(raw))
	}
	g.Close()
	if req, dec, _ := decisions(mem); req != 1 || dec != 0 {
		t.Fatalf("requested %d decided %d (the API records a person refusal, not the gateway)", req, dec)
	}
}

// No answer in the timeout: the held call is refused with the timeout wording,
// the gateway records approval.decided so it is no longer pending, and pings
// went out while it waited. No continuation turn is involved.
func TestHoldTimeoutRefusesAndRecords(t *testing.T) {
	d := &fakeDecider{} // never decided
	b, g, mem := heldGuard(t, context.Background(), anthropicAskStream, []string{"git push"}, d)
	raw, _ := io.ReadAll(b)
	b.Close()
	out := string(raw)
	if !strings.Contains(out, RefusedPrefix) {
		t.Fatalf("a timed-out held call was not refused:\n%s", out)
	}
	if !strings.Contains(out, "event: ping") {
		t.Fatalf("no keep-alive ping went out during the hold:\n%s", out)
	}
	g.Close()
	req, dec, text := decisions(mem)
	if req != 1 || dec != 1 {
		t.Fatalf("requested %d decided %d (a timeout records the decision)", req, dec)
	}
	if !strings.Contains(text, "no one answered in time") {
		t.Fatalf("the timeout decision text was %q", text)
	}
}

// The agent disconnecting mid-hold ends the hold and records approval.decided,
// so nothing is left pending; the held call is never released.
func TestHoldDisconnectEndsAndRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &fakeDecider{} // never decided, so only the disconnect ends it
	b, g, mem := heldGuard(t, ctx, anthropicAskStream, []string{"git push"}, d)
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	raw, _ := io.ReadAll(b)
	b.Close()
	if strings.Contains(string(raw), "toolu_1") && !strings.Contains(string(raw), "event: ping") {
		t.Fatalf("the held call was released after a disconnect:\n%s", string(raw))
	}
	g.Close()
	req, dec, text := decisions(mem)
	if req != 1 || dec != 1 {
		t.Fatalf("requested %d decided %d (a disconnect records the decision)", req, dec)
	}
	if !strings.Contains(text, "connection closed") {
		t.Fatalf("the disconnect decision text was %q", text)
	}
}

// Both Ask calls of one reply are held under one approval and released together
// on allow.
func TestHoldTwoCallsReleasedTogether(t *testing.T) {
	d := &fakeDecider{allow: true, decided: true}
	b, g, mem := heldGuard(t, context.Background(), anthropicTwoAskStream, []string{"git push", "npm publish"}, d)
	raw, _ := io.ReadAll(b)
	b.Close()
	out := string(raw)
	if strings.Contains(out, RefusedPrefix) {
		t.Fatalf("an allowed held call was refused:\n%s", out)
	}
	if !strings.Contains(out, "toolu_1") || !strings.Contains(out, "toolu_2") {
		t.Fatalf("both held calls did not pass through:\n%s", out)
	}
	g.Close()
	if req, _, _ := decisions(mem); req != 1 {
		t.Fatalf("two Ask calls were not held under one approval: %d requested", req)
	}
}

// Both Ask calls of one reply are refused together on refuse.
func TestHoldTwoCallsRefusedTogether(t *testing.T) {
	d := &fakeDecider{allow: false, decided: true}
	b, _, _ := heldGuard(t, context.Background(), anthropicTwoAskStream, []string{"git push", "npm publish"}, d)
	raw, _ := io.ReadAll(b)
	b.Close()
	out := string(raw)
	if strings.Count(out, RefusedPrefix) < 2 {
		t.Fatalf("both held calls were not refused together:\n%s", out)
	}
}

// An unstreamed (single-JSON) reply is refused inline, not held: it has no frame
// to ping into. The decider is never asked.
func TestHoldUnstreamedRefusedInline(t *testing.T) {
	d := &fakeDecider{allow: true, decided: true}
	b, g, mem := heldGuard(t, context.Background(), anthropicAskJSON, []string{"git push"}, d)
	raw, _ := io.ReadAll(b)
	b.Close()
	if !strings.Contains(string(raw), RefusedPrefix) {
		t.Fatalf("an unstreamed Ask reply was not refused inline:\n%s", string(raw))
	}
	d.mu.Lock()
	calls := d.calls
	d.mu.Unlock()
	if calls != 0 {
		t.Fatalf("the decider was polled for an unstreamed reply: %d calls", calls)
	}
	g.Close()
	if req, _, _ := decisions(mem); req != 1 {
		t.Fatalf("the unstreamed Ask was not recorded as one approval: %d", req)
	}
}

// A reply whose terminal frame arrives together with EOF (no trailing blank
// line) is still held, not released unapproved: the hold runs and the call is
// refused on timeout. Regression: the EOF used to flush the buffered call.
func TestHoldTerminalFrameWithEOFStillHeld(t *testing.T) {
	stream := strings.TrimRight(anthropicAskStream, "\n")
	d := &fakeDecider{} // never decided: times out
	b, g, mem := heldGuard(t, context.Background(), stream, []string{"git push"}, d)
	raw, _ := io.ReadAll(b)
	b.Close()
	if !strings.Contains(string(raw), RefusedPrefix) {
		t.Fatalf("an EOF-terminated held call was released unapproved:\n%s", string(raw))
	}
	g.Close()
	if req, dec, _ := decisions(mem); req != 1 || dec != 1 {
		t.Fatalf("requested %d decided %d", req, dec)
	}
}

// A trailing frame after the reply is complete does not restart a finished hold
// nor spin: after a timeout the held call is refused and the body ends.
func TestHoldTrailingFrameDoesNotRestart(t *testing.T) {
	stream := anthropicAskStream + "data: [DONE]\n\n"
	d := &fakeDecider{} // times out
	b, g, mem := heldGuard(t, context.Background(), stream, []string{"git push"}, d)
	done := make(chan string, 1)
	go func() { raw, _ := io.ReadAll(b); done <- string(raw) }()
	select {
	case out := <-done:
		if !strings.Contains(out, RefusedPrefix) {
			t.Fatalf("the held call was not refused:\n%s", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read did not terminate: a trailing frame restarted the hold or spun")
	}
	b.Close()
	g.Close()
	if _, dec, _ := decisions(mem); dec != 1 {
		t.Fatalf("decided %d (one timeout expected)", dec)
	}
}

// A streamed reply whose Ask call is complete but whose reply ends BEFORE its
// terminal event (a cut, or codex ending after response.output_item.done but
// before response.completed) must NOT release the held call: codex runs a call
// at output_item.done, so an unrefused flush would run it unapproved. The
// pending hold refuses the call on EOF and records no approval (an incomplete
// reply is one no person should answer).
func TestHoldPendingEOFRefuses(t *testing.T) {
	// anthropicAskStream truncated before its terminal event: the Ask call is
	// complete (content_block_stop) but the reply never reaches message_stop.
	cut := anthropicAskStream
	if i := strings.Index(cut, "event: message_delta"); i >= 0 {
		cut = cut[:i]
	}
	d := &fakeDecider{} // never consulted: the reply never completes, so no hold goes live
	b, g, mem := heldGuard(t, context.Background(), cut, []string{"git push"}, d)
	raw, _ := io.ReadAll(b)
	b.Close()
	if !strings.Contains(string(raw), RefusedPrefix) {
		t.Fatalf("a pending Ask call was released on an early EOF, unapproved:\n%s", string(raw))
	}
	d.mu.Lock()
	calls := d.calls
	d.mu.Unlock()
	if calls != 0 {
		t.Fatalf("the decider was polled for an incomplete reply: %d", calls)
	}
	g.Close()
	if req, dec, _ := decisions(mem); req != 0 || dec != 0 {
		t.Fatalf("an incomplete reply recorded approval events: requested %d decided %d", req, dec)
	}
}

// Closing the body while a hold is live records the approval ended, so it is not
// left pending. This is the ping-write-failure path: the proxy reads a ping,
// its write to the gone client fails, and it closes the body — sequentially,
// the same goroutine, as httputil.ReverseProxy does. No answer and no timeout
// decided it, so the Close must.
func TestHoldCloseRecordsDecision(t *testing.T) {
	mem := &event.Memory{}
	g := New(&url.URL{}, anthropic.Adapter{}, mem)
	g.SetDecider(&fakeDecider{}) // never decided
	g.SetHoldTiming(5*time.Second, 20*time.Millisecond, 100*time.Millisecond)
	b := g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"},
		Rules{Ask: []string{"git push"}}, nil, nil, io.NopCloser(strings.NewReader(anthropicAskStream)), func() {})
	// Read until a ping shows the hold is live, in this one goroutine.
	buf := make([]byte, 4096)
	var got string
	for i := 0; i < 50 && !strings.Contains(got, "event: ping"); i++ {
		n, err := b.Read(buf)
		got += string(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got, "event: ping") {
		t.Fatal("no ping went out, so the hold never became live")
	}
	// The proxy's write of that ping failed: it closes the body. Record the end.
	b.Close()
	g.Close()
	if _, dec, text := decisions(mem); dec != 1 || !strings.Contains(text, "connection closed") {
		t.Fatalf("Close did not record the approval ended: decided %d text %q", dec, text)
	}
}

// A reply that is inline-refused (no decider, so no hold) and then closed after
// a clean read records NO decision: closing it is not a hold ending. Regression
// for a Close that recorded "connection closed" for any reply with an approval
// id, which registerApproval sets for an inline refusal too.
func TestInlineRefusalCloseRecordsNoDecision(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
	}{
		{"streamed", anthropicAskStream},
		{"unstreamed", anthropicAskJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := &event.Memory{}
			g := New(&url.URL{}, anthropic.Adapter{}, mem)
			// No SetDecider: an Ask call is refused inline, never held.
			b := g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"},
				Rules{Ask: []string{"git push"}}, nil, nil, io.NopCloser(strings.NewReader(tc.stream)), func() {})
			raw, _ := io.ReadAll(b)
			b.Close()
			if !strings.Contains(string(raw), RefusedPrefix) {
				t.Fatalf("the Ask call was not inline-refused:\n%s", string(raw))
			}
			g.Close()
			if req, dec, _ := decisions(mem); req != 1 || dec != 0 {
				t.Fatalf("requested %d decided %d: closing an inline refusal must record no decision", req, dec)
			}
		})
	}
}
