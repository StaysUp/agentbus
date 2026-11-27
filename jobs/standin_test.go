package jobs

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/queue"
)

// A turn's model calls carry its stand-in, never its turn token: an agent
// sends its sign-in to its vendor directly too (Claude Code 2.1.321 to
// api.anthropic.com, agy 1.5.9 to www.googleapis.com, measured 2026-11-15).
// The relay swaps a running turn's stand-in for the real credential, and
// refuses the turn token, a forged stand-in, one sealed by another control,
// another turn's (in agentbus up, TurnGate) and one whose attempt ended. The
// stand-in opens nothing on control's API.
func TestStandIn(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(vendor.Close)
	calls := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
	u, _ := url.Parse(vendor.URL)
	g := gateway.New(u, anthropic.Adapter{}, &event.Memory{})
	t.Cleanup(g.Close)
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	src := credentials.Plug{credentials.AccessSubscription: fixedKey{"claude": {Token: "fake-claude-access"}}}
	relays := map[string]Upstream{"anthropic": {URL: u, Access: credentials.AccessSubscription, Harness: "claude", Gateways: []*gateway.Gateway{g}}}
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, Credentials: src, Relay: relays})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := q.RegisterRunner(queue.DefaultTeam, "r", nil, 2, queue.HashToken("tok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	type started struct {
		a     queue.Attempt
		j     queue.Job
		tok   string
		stand string
	}
	var turns []started
	for range 2 {
		if _, _, err := q.Create(queue.DefaultTeam, queue.KindTurn, "", queue.Spec{Harness: "claude", Folder: "/x", Prompt: "hi"}, queue.Links{}); err != nil {
			t.Fatal(err)
		}
		_, a, ok, _, err := q.Take(rn.ID, 30*time.Second, queue.LaneInteractive)
		if err != nil || !ok {
			t.Fatalf("take: %v %v", ok, err)
		}
		if _, _, err := q.Start(rn.ID, a.ID, a.Number, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		tok := s.mint(rn.ID, a.ID, a.Number)
		j, _, ok := s.turnJob(tok)
		if !ok {
			t.Fatal("a running turn's token")
		}
		turns = append(turns, started{a: a, j: j, tok: tok, stand: s.standIn(rn.ID, a.ID, a.Number)})
	}
	one, two := turns[0], turns[1]

	// A stand-in reads as random: it names neither runner nor attempt, and
	// each one minted differs, every one good while its attempt runs.
	again := s.standIn(rn.ID, one.a.ID, one.a.Number)
	if !strings.HasPrefix(one.stand, StandInPrefix) || strings.Contains(one.stand, rn.ID) || strings.Contains(one.stand, one.a.ID) ||
		again == one.stand || strings.Contains(one.stand, one.tok) {
		t.Fatalf("stand-ins %q %q of runner %s attempt %s", one.stand, again, rn.ID, one.a.ID)
	}
	if j, ok := s.standInJob(again); !ok || j.ID != one.j.ID {
		t.Fatal("a second stand-in of a running attempt is not its job's")
	}
	// The stand-in opens nothing on control's API; the turn token still does.
	if _, ok := s.TurnGrant(one.stand); ok || s.TurnTokenEnded(one.stand) {
		t.Fatal("the stand-in reads as a turn token on the API")
	}
	if _, ok := s.TurnGrant(one.tok); !ok {
		t.Fatal("the turn token no longer works on the API")
	}

	relay := s.Relay()
	do := func(cred string) int {
		r := httptest.NewRequest("POST", "/relay/anthropic/v1/messages",
			strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Authorization", "Bearer "+cred)
		w := httptest.NewRecorder()
		relay.ServeHTTP(w, r)
		return w.Code
	}
	// The running turn's stand-in gets the real credential, in its place.
	if code := do(one.stand); code != http.StatusOK {
		t.Fatalf("the stand-in: %d", code)
	}
	if got := calls(); len(got) != 1 || got[0] != "/v1/messages Bearer fake-claude-access" {
		t.Fatalf("the vendor saw %v", got)
	}
	other, err := New(Config{Queue: q, Token: "another-main-token", EnrollToken: enrollTok})
	if err != nil {
		t.Fatal(err)
	}
	last := "0"
	if strings.HasSuffix(one.stand, "0") {
		last = "1"
	}
	forged := one.stand[:len(one.stand)-1] + last
	for name, cred := range map[string]string{
		"the turn token":                  one.tok,
		"a forged stand-in":               forged,
		"a stand-in of another control":   other.standIn(rn.ID, one.a.ID, one.a.Number),
		"a stand-in with no sealed bytes": StandInPrefix,
		"a stand-in that is not hex":      StandInPrefix + "zz",
	} {
		if code := do(cred); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	if got := calls(); len(got) != 1 {
		t.Fatalf("a refused call reached the vendor: %v", got)
	}

	// agentbus up: the gateway entrance check (TurnGate) lets a call's own
	// running stand-in through, refuses another turn's and the turn token, and
	// adds no credential of its own — the relay above does the swap (do), as it
	// does for a runner on another machine.
	gate := s.TurnGate()
	call := func(cred string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header.Set("Authorization", "Bearer "+cred)
		return r
	}
	if ref := gate.Turn(call(one.stand), one.j.Session, one.j.Turn); ref != nil {
		t.Fatalf("up, its own stand-in: %d %s", ref.Status, ref.Body)
	}
	if set, _, ref := gate.Read(call(one.stand)); ref != nil || set != nil {
		t.Fatal("up's gate added a credential of its own; the relay does the swap")
	}
	if ref := gate.Turn(call(two.stand), one.j.Session, one.j.Turn); ref == nil || ref.Status != http.StatusForbidden {
		t.Fatalf("up, another turn's stand-in: %+v", ref)
	}
	if ref := gate.Turn(call(one.tok), one.j.Session, one.j.Turn); ref == nil || ref.Status != http.StatusUnauthorized {
		t.Fatalf("up, the turn token: %+v", ref)
	}

	// Once the attempt ended, its stand-in is refused at the gate and the
	// relay; the other turn's still passes.
	if _, err := q.End(rn.ID, one.a.ID, one.a.Number, queue.Finish{Status: queue.Finished}); err != nil {
		t.Fatal(err)
	}
	if code := do(one.stand); code != http.StatusUnauthorized {
		t.Errorf("an ended turn's stand-in: %d", code)
	}
	if ref := gate.Turn(call(one.stand), one.j.Session, one.j.Turn); ref == nil || ref.Status != http.StatusUnauthorized {
		t.Errorf("up, an ended turn's stand-in: %+v", ref)
	}
	if code := do(two.stand); code != http.StatusOK {
		t.Errorf("the running turn's stand-in after another ended: %d", code)
	}
}
