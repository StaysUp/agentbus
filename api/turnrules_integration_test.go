//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// A turn's rules_version on GET /api/v0/turns and /api/v0/turns/{id} is the
// one its turn.started recorded: the ETag of GET /api/v0/settings/permissions
// when it started, without the quotes. A turn queued behind another has none
// until it starts, and a change of the permissions while it waits is the
// version it then starts under (#121).
func TestTurnRulesVersionFromItsStart(t *testing.T) {
	dir := t.TempDir()
	// It names its session first, as claude does, so the second turn can
	// continue the session.
	script := `echo '{"type":"result","session_id":"sid-'$$'"}'; : > "$D/child-$$"; i=0; while [ ! -e "$D/release" ] && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done`
	wire := func(agent, baseURL string, _ run.Access) (run.Launch, error) {
		return run.Launch{Path: "/bin/sh", Args: []string{"-c", script, "sh"}, Env: []string{"PATH=/usr/bin:/bin", "D=" + dir}}, nil
	}
	perm := filepath.Join(t.TempDir(), settings.PolicyFile)
	if err := os.WriteFile(perm, []byte("[limits]\nmax_delegated_at_once = 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := stackEnv(t, stack{wire: wire, turnsToken: true, permissions: perm})
	t.Cleanup(func() { os.WriteFile(filepath.Join(dir, "release"), nil, 0o600) })

	etag := func() string {
		t.Helper()
		resp, out := send(t, e, "GET", "/api/v0/settings/permissions", "", "")
		v, err := strconv.Unquote(resp.Header.Get("ETag"))
		if resp.StatusCode != 200 || err != nil || v == "" {
			t.Fatalf("GET permissions: %d %q %s", resp.StatusCode, resp.Header.Get("ETag"), out)
		}
		return v
	}
	post := func(session, prompt string) Started {
		t.Helper()
		code, out := asTok(t, e, turnsTok, "POST", "/api/v0/turns", `{"session":"`+session+`","agent":"claude","prompt":"`+prompt+`"}`)
		var s Started
		if code != 202 || json.Unmarshal([]byte(out), &s) != nil || s.Turn == "" {
			t.Fatalf("POST %s: %d %s", prompt, code, out)
		}
		return s
	}
	// rows are the turns of the list and the single turn, each as JSON keys.
	type row map[string]json.RawMessage
	listed := func(session, turn string) row {
		t.Helper()
		var list struct{ Turns []row }
		if code := getJSON(t, e, "/api/v0/turns?session="+session, &list); code != 200 {
			t.Fatalf("GET turns: %d", code)
		}
		for _, r := range list.Turns {
			if string(r["id"]) == strconv.Quote(turn) {
				return r
			}
		}
		t.Fatalf("turn %s is not listed", turn)
		return nil
	}
	one := func(turn string) row {
		t.Helper()
		var got struct{ Turn row }
		if code := getJSON(t, e, "/api/v0/turns/"+turn, &got); code != 200 {
			t.Fatalf("GET turn %s: %d", turn, code)
		}
		return got.Turn
	}
	version := func(r row) (string, bool) {
		raw, ok := r["rules_version"]
		if !ok {
			return "", false
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("rules_version %s: %v", raw, err)
		}
		return v, true
	}

	before := etag()
	session := e.newSession(t, t.TempDir(), true)
	first := post(session, "one")
	for deadline := time.Now().Add(10 * time.Second); children(t, dir) < 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first turn did not start")
		}
	}
	second := post(session, "two")
	if second.State != "queued" {
		t.Fatalf("second: %+v, want queued behind the first", second)
	}
	// Queued: no turn.started yet, so no rules version on either route.
	for name, r := range map[string]row{"list": listed(session, second.Turn), "single": one(second.Turn)} {
		if v, ok := version(r); ok {
			t.Errorf("queued turn on the %s: rules_version %q, want none", name, v)
		}
	}
	// The permissions change while the second waits: it starts under the new
	// version, and the first keeps the one it started under.
	if resp, out := send(t, e, "PUT", "/api/v0/settings/permissions", strconv.Quote(before), `{"forbid":["git push"]}`); resp.StatusCode != 200 {
		t.Fatalf("PUT permissions: %d %s", resp.StatusCode, out)
	}
	after := etag()
	if after == before {
		t.Fatalf("the permissions version did not change: %q", after)
	}
	os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
	waitEnded(t, e, first.Turn)
	waitEnded(t, e, second.Turn)

	for _, tc := range []struct {
		turn, want string
	}{{first.Turn, before}, {second.Turn, after}} {
		recs, err := e.store.After(0, store.Filter{Turn: tc.turn, TurnSession: session}, 100)
		if err != nil {
			t.Fatal(err)
		}
		recorded := ""
		for _, rec := range recs {
			var ev event.Event
			if rec.Kind == event.TurnStarted && json.Unmarshal(rec.Data, &ev) == nil {
				recorded = ev.RulesVersion
			}
		}
		if recorded != tc.want {
			t.Fatalf("%s: turn.started rules_version %q, want %q", tc.turn, recorded, tc.want)
		}
		for name, r := range map[string]row{"list": listed(session, tc.turn), "single": one(tc.turn)} {
			if v, ok := version(r); !ok || v != recorded {
				t.Errorf("%s on the %s: rules_version %q (present %v), want %q from its turn.started", tc.turn, name, v, ok, recorded)
			}
		}
	}
	if strings.Contains(before, `"`) {
		t.Errorf("the version %q carries quotes", before)
	}
}
