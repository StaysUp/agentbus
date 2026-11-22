//go:build integration

package run

import (
	"strings"
	"testing"

	"github.com/staysup/agentbus/store"
)

// closedTurn runs harness on an agent that is /bin/sh -c script and returns
// the turn as the record lists it once it ended.
func closedTurn(t *testing.T, harness, script string) store.Turn {
	t.Helper()
	e, st := sandboxEngine(t, "", true, script)
	turn, err := e.Start(Request{Harness: harness, Folder: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-turn.Done()
	list, err := st.Turns(store.TurnQuery{Limit: 2})
	if err != nil || len(list.Turns) != 1 {
		t.Fatalf("Turns: %d turns, %v, want the one turn", len(list.Turns), err)
	}
	return list.Turns[0]
}

// A failure its harness knows shows the harness's plain words as the turn's
// error, and the agent's own line stays in the error tail.
func TestKnownFailurePlainWords(t *testing.T) {
	const line = "Eligibility check failed: failed to fetch user info: 401 Unauthorized"
	got := closedTurn(t, "agy", `echo "`+line+`" >&2; exit 1`)
	if want := "Antigravity's Google sign-in can't run sessions yet. Add a Gemini API key in Settings, Subscriptions."; got.Error != want {
		t.Errorf("error %q, want %q", got.Error, want)
	}
	if !strings.Contains(got.ErrorTail, line) {
		t.Errorf("error tail %q lost the agent's own line", got.ErrorTail)
	}
}

// An error no harness lists is the turn's error as the agent wrote it.
func TestUnknownFailureUnchanged(t *testing.T) {
	const line = "error: invalid model selection"
	if got := closedTurn(t, "agy", `echo "`+line+`" >&2; exit 1`); got.Error != line {
		t.Errorf("error %q, want %q", got.Error, line)
	}
}
