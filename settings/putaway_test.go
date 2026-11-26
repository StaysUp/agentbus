package settings

import (
	"strings"
	"testing"
	"time"
)

// [workspaces] put_away_after_minutes: 30 by default, 0 is off, below 0 is
// refused, and the starter file writes it.
func TestPutAwayAfter(t *testing.T) {
	if d, on := DefaultPolicy().PutAwayAfter(); !on || d != 30*time.Minute {
		t.Fatalf("default: %v %v", d, on)
	}
	p, err := ParsePolicy([]byte("[workspaces]\nput_away_after_minutes = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, on := p.PutAwayAfter(); on {
		t.Fatal("0 is off")
	}
	if _, err := ParsePolicy([]byte("[workspaces]\nput_away_after_minutes = -1\n")); err == nil {
		t.Fatal("below 0 is refused")
	}
	if !strings.Contains(PolicyStarter, "put_away_after_minutes = 30") {
		t.Fatal("the starter file holds the key")
	}
	p, err = DecodePolicy([]byte(`{"workspaces":{"put_away_after_minutes":5}}`))
	if d, on := p.PutAwayAfter(); err != nil || !on || d != 5*time.Minute {
		t.Fatalf("JSON: %v %v %v", d, on, err)
	}
}
