package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

func TestTurnRulesVersion(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a, err := New(Config{Store: st, Token: token, Port: "8787"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	first := event.Event{ID: "rules-start-1", Kind: event.TurnStarted, Time: now,
		Session: "s1", Turn: "t_started", RulesVersion: "v-at-start"}
	next := event.Event{ID: "rules-start-2", Kind: event.TurnStarted, Time: now.Add(time.Second),
		Session: "s1", Turn: "t_next", RulesVersion: "v-later"}
	legacy := event.Event{ID: "rules-legacy", Kind: event.TurnStarted, Time: now.Add(-time.Second),
		Session: "s1", Turn: "t_legacy"}
	appendAll(t, st, legacy, first, next)

	get := func(path string, into any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		a.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), into); err != nil {
			t.Fatal(err)
		}
	}
	type row map[string]json.RawMessage
	check := func(r row, want event.Event) {
		t.Helper()
		if got := string(r["id"]); got != `"`+want.Turn+`"` {
			t.Fatalf("turn id = %s, want %q", got, want.Turn)
		}
		got, present := r["rules_version"]
		if want.RulesVersion == "" {
			if present {
				t.Errorf("%s: rules_version = %s, want field absent", want.Turn, got)
			}
			return
		}
		var version string
		if !present || json.Unmarshal(got, &version) != nil || version != want.RulesVersion {
			t.Errorf("%s: rules_version = %s, want %q", want.Turn, got, want.RulesVersion)
		}
	}

	var list struct{ Turns []row }
	get("/api/v0/turns?session=s1", &list)
	if len(list.Turns) != 3 {
		t.Fatalf("list has %d turns, want 3", len(list.Turns))
	}
	for i, start := range []event.Event{next, first, legacy} {
		check(list.Turns[i], start)
		var one struct{ Turn row }
		get("/api/v0/turns/"+start.Turn, &one)
		check(one.Turn, start)
	}
}

func TestOpenAPITurnRulesVersion(t *testing.T) {
	turn := loadSpec(t).Components.Schemas["Turn"].Value
	field := turn.Properties["rules_version"]
	if field == nil || field.Value == nil || field.Value.Description == "" {
		t.Fatal("Turn.rules_version is not documented")
	}
	if slices.Contains(turn.Required, "rules_version") {
		t.Fatal("Turn.rules_version is required, want optional")
	}
}
