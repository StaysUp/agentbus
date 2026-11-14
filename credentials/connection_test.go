package credentials

import (
	"testing"
	"time"
)

// A subscription's credential names its connect: the same across a
// renewal, new with the next connect, and what its status's connected_at
// names, so control shows plan usage of the current connect only.
func TestCredentialNamesItsConnect(t *testing.T) {
	s, _, now := renewing(t)
	need := Need{Access: AccessSubscription, Harness: "codex"}
	first, err := s.Credential(need)
	if err != nil || first.Connection == "" {
		t.Fatalf("%v %q", err, first.Connection)
	}
	st, _ := s.Status("codex")
	if first.Connection != ConnectionOf(*st.ConnectedAt) {
		t.Fatalf("connection %q, status connected_at %v", first.Connection, st.ConnectedAt)
	}
	// Renewed: the same connect.
	*now = now.Add(56 * time.Minute)
	renewed, err := s.Credential(need)
	if err != nil || renewed.Token == first.Token || renewed.Connection != first.Connection {
		t.Fatalf("after renewal: %v, connection %q, want %q", err, renewed.Connection, first.Connection)
	}
	// Connected again: a new one.
	*now = now.Add(time.Minute)
	if err := s.Keep("codex", Signin{Token: fakeJWT("other-account", now.Add(time.Hour)), Refresh: "fake-refresh-x", Expires: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	again, err := s.Credential(need)
	if err != nil || again.Connection == first.Connection {
		t.Fatalf("after a new connect: %v, connection %q", err, again.Connection)
	}
}
