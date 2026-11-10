package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/body"
)

func call(exchange string, pieces ...string) body.Call {
	c := body.Call{Exchange: exchange, Session: "s1", Vendor: "anthropic", Time: time.Unix(1793858039, 0),
		Request: body.Request{Top: json.RawMessage(`{"model":"m","max_tokens":10}`)}}
	for _, p := range pieces {
		c.Pieces = append(c.Pieces, body.Piece{Kind: body.Message, Label: "user", JSON: json.RawMessage(p)})
	}
	return c
}

func count(t *testing.T, s *Store, q string) int {
	t.Helper()
	var n int
	if err := s.r.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The same piece in two calls is stored once.
func TestKeepCallStoresEachPieceOnce(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	sys := `{"type":"text","text":"You are Claude Code."}`
	if err := s.KeepCall(call("x-1", sys, `{"role":"user","content":"a"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.KeepCall(call("x-2", sys, `{"role":"user","content":"a"}`, `{"role":"user","content":"b"}`)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM pieces`); n != 3 {
		t.Errorf("%d pieces stored, want 3", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM calls`); n != 2 {
		t.Errorf("%d calls stored, want 2", n)
	}
}

// A call's pieces come back in the request's order, with the request's bytes,
// even when a piece repeats inside one call and sorts before the others.
func TestCallContextKeepsOrder(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "events.db"))
	order := []string{`"zzz"`, `{"b":2}`, `"aaa"`, `{"b":2}`, `[1, 2]`}
	if err := s.KeepCall(call("x-1", order...)); err != nil {
		t.Fatal(err)
	}
	c, found, err := s.CallContext("x-1", true)
	if err != nil || !found {
		t.Fatalf("found %v, err %v", found, err)
	}
	if len(c.Pieces) != len(order) {
		t.Fatalf("%d pieces, want %d", len(c.Pieces), len(order))
	}
	for i, p := range c.Pieces {
		if string(p.JSON) != order[i] || p.Size != len(order[i]) || p.Hash != (body.Piece{JSON: []byte(order[i])}).Hash() {
			t.Errorf("piece %d = %s (size %d), want %s", i, p.JSON, p.Size, order[i])
		}
	}
	if string(c.Request) != `{"model":"m","max_tokens":10}` || c.Session != "s1" || c.Vendor != "anthropic" {
		t.Errorf("call = %+v", c)
	}
	// full=false: sizes and labels, no bodies.
	c, _, err = s.CallContext("x-1", false)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range c.Pieces {
		if p.JSON != nil || p.Size != len(order[i]) || p.Label != "user" {
			t.Errorf("piece %d without bodies = %+v", i, p)
		}
	}
	if _, found, _ := s.CallContext("x-none", true); found {
		t.Error("an unknown exchange was found")
	}
}

// A call another service kept is stored once its pieces are all here: one
// without its bytes that this store lacks is named, and the call waits.
func TestKeepSentAsksForMissingPieces(t *testing.T) {
	s := newStore(t)
	sys := body.Piece{Kind: body.System, JSON: json.RawMessage(`{"type":"text","text":"sys"}`)}
	msg := body.Piece{Kind: body.Message, Label: "user", JSON: json.RawMessage(`{"role":"user","content":"hi"}`)}
	c := CallContext{Exchange: "x-r-1", Session: "s1", Vendor: "anthropic", Time: time.Now(), Size: 10, Request: json.RawMessage(`{"model":"m"}`),
		Pieces: []Piece{{Kind: sys.Kind, Hash: sys.Hash()}, {Kind: msg.Kind, Label: msg.Label, Hash: msg.Hash(), JSON: msg.JSON}}}
	missing, err := s.KeepSent(c)
	if err != nil || len(missing) != 1 || missing[0] != sys.Hash() {
		t.Fatalf("%v %v", missing, err)
	}
	if _, found, _ := s.CallContext("x-r-1", false); found {
		t.Fatal("a call with a missing piece was stored")
	}
	c.Pieces[0].JSON = sys.JSON
	c.Pieces[1].JSON = nil // stored by the first send
	if missing, err = s.KeepSent(c); err != nil || len(missing) != 0 {
		t.Fatalf("%v %v", missing, err)
	}
	got, found, err := s.CallContext("x-r-1", true)
	if err != nil || !found || len(got.Pieces) != 2 || string(got.Pieces[0].JSON) != string(sys.JSON) || string(got.Pieces[1].JSON) != string(msg.JSON) {
		t.Fatalf("%+v %v", got, err)
	}
	c.Pieces[0].JSON = json.RawMessage(`{"forged":true}`)
	if _, err := s.KeepSent(c); err == nil {
		t.Fatal("a piece that does not match its hash was taken")
	}
}
