package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/body"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/store"
)

const signIn = "sk-ant-oat01-FAKE-SIGN-IN-MUST-NEVER-BE-STORED"

const contextRequest = `{"model":"claude-sonnet-5-5","max_tokens":32000,` +
	`"system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"CLAUDE.md: keep data."}],` +
	`"tools":[{"name":"Read","input_schema":{"type":"object"}},{"name":"Edit","input_schema":{"type":"object"}}],` +
	`"messages":[{"role":"user","content":"Fix the login bug"},{"role":"assistant","content":"Reading it."},{"role":"user","content":"thanks"}],` +
	`"stream":false}`

// A request sent through the gateway comes back from GET
// /api/v0/turns/{id}/calls/{call} in the request's order, each piece the bytes the
// vendor received, and no header value is in the store's files.
func TestCallContextThroughGateway(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "events.db")
	e := newEnvAt(t, db)

	var forwarded []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	mem := &event.Memory{}
	gw := gateway.New(u, anthropic.Adapter{}, mem)
	gw.KeepBodies(e.store)
	front := httptest.NewServer(gw)
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/s/sess-1/v1/messages", strings.NewReader(contextRequest))
	req.Header.Set("Authorization", "Bearer "+signIn)
	req.Header.Set("X-Api-Key", signIn)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	gw.Close() // keeps every queued body

	var exchange string
	for _, ev := range mem.Events() {
		if ev.Kind == event.ModelRequested {
			exchange = ev.Exchange
		}
	}
	if exchange == "" {
		t.Fatal("no model.requested")
	}

	inTurn(t, e, "sess-1", "t_1", exchange)
	r, body := do(t, e.req(t, "GET", "/api/v0/turns/t_1/calls/"+exchange, nil))
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
	var c store.CallContext
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if c.Exchange != exchange || c.Session != "sess-1" || c.Vendor != "anthropic" || c.Size != len(forwarded) {
		t.Errorf("call = %+v", c)
	}
	wantKinds := []string{"system", "system", "tool", "tool", "message", "message", "message"}
	wantLabels := []string{"text", "text", "Read", "Edit", "user", "assistant", "user"}
	if len(c.Pieces) != len(wantKinds) {
		t.Fatalf("%d pieces, want %d", len(c.Pieces), len(wantKinds))
	}
	at := 0
	for i, p := range c.Pieces {
		if p.Kind != wantKinds[i] || p.Label != wantLabels[i] || p.Size != len(p.JSON) || p.ApproxTokens < 1 {
			t.Errorf("piece %d = %s %s size %d tokens %d", i, p.Kind, p.Label, p.Size, p.ApproxTokens)
		}
		j := bytes.Index(forwarded[at:], p.JSON)
		if j < 0 {
			t.Fatalf("piece %d is not the forwarded bytes after offset %d: %s", i, at, p.JSON)
		}
		at += j + len(p.JSON)
	}
	if string(c.Request) != `{"model":"claude-sonnet-5-5","max_tokens":32000,"stream":false}` {
		t.Errorf("request = %s", c.Request)
	}

	// full=false: the same pieces without their JSON.
	_, slim := do(t, e.req(t, "GET", "/api/v0/turns/t_1/calls/"+exchange+"?full=false", nil))
	var s store.CallContext
	if err := json.Unmarshal([]byte(slim), &s); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(slim, `"json"`) || len(s.Pieces) != len(c.Pieces) {
		t.Errorf("full=false: %s", slim)
	}
	for i := range s.Pieces {
		if s.Pieces[i].Size != c.Pieces[i].Size || s.Pieces[i].Hash != c.Pieces[i].Hash {
			t.Errorf("piece %d differs without bodies", i)
		}
	}
	if r, _ := do(t, e.req(t, "GET", "/api/v0/turns/t_1/calls/"+exchange+"?full=maybe", nil)); r.StatusCode != 400 {
		t.Errorf("full=maybe: %d", r.StatusCode)
	}
	if r, _ := do(t, e.req(t, "GET", "/api/v0/turns/t_1/calls/x-none-1", nil)); r.StatusCode != 404 {
		t.Errorf("unknown call: %d", r.StatusCode)
	}

	// The sign-in is in no file of the store, while the body is.
	files, _ := filepath.Glob(db + "*")
	var all []byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	if !bytes.Contains(all, []byte("CLAUDE.md: keep data.")) {
		t.Fatal("the request body is not in the store's files, so the header check proves nothing")
	}
	if bytes.Contains(all, []byte(signIn)) || bytes.Contains(all, []byte("FAKE-SIGN-IN")) {
		t.Error("a header value is in the store's files")
	}
}

// Each returned piece is the stored bytes: its SHA-256 is hash and its length
// is size, even with <, > and & and whitespace that encoding/json would
// escape and compact.
func TestCallContextBytesUnchanged(t *testing.T) {
	e := newEnv(t)
	pieces := []string{
		`{ "type" : "text",  "text" : "if a < b && c > d { return }" }`,
		"[1,\n  2,\t3]",
		`"<script>&amp;</script>"`,
	}
	c := body.Call{Exchange: "x-raw-1", Session: "s1", Vendor: "anthropic", Time: time.Now(), Size: 99,
		Request: body.Request{Top: json.RawMessage(`{"model":"m","note":"a<b & c>d"}`)}}
	for _, p := range pieces {
		c.Pieces = append(c.Pieces, body.Piece{Kind: body.Message, Label: "user", JSON: json.RawMessage(p)})
	}
	if err := e.store.KeepCall(c); err != nil {
		t.Fatal(err)
	}
	inTurn(t, e, "s1", "t_1", "x-raw-1")
	r, got := do(t, e.req(t, "GET", "/api/v0/turns/t_1/calls/x-raw-1", nil))
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, got)
	}
	// json.RawMessage keeps a value's raw text, so this reads the bytes sent.
	var reply struct {
		Request json.RawMessage
		Pieces  []struct {
			Hash string
			Size int
			JSON json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(got), &reply); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, got)
	}
	if len(reply.Pieces) != len(pieces) {
		t.Fatalf("%d pieces", len(reply.Pieces))
	}
	for i, p := range reply.Pieces {
		sum := sha256.Sum256(p.JSON)
		if hex.EncodeToString(sum[:]) != p.Hash || len(p.JSON) != p.Size || string(p.JSON) != pieces[i] {
			t.Errorf("piece %d: got %q (%d bytes), want %q (%d bytes), hash match %v",
				i, p.JSON, len(p.JSON), pieces[i], p.Size, hex.EncodeToString(sum[:]) == p.Hash)
		}
	}
	if string(reply.Request) != `{"model":"m","note":"a<b & c>d"}` {
		t.Errorf("request = %s", reply.Request)
	}
}
