//go:build integration

package main

import (
	"net/http"
	"path/filepath"
	"testing"
)

// agentbus up on a folder that is not there yet says it is new.
func TestUpSaysADataFolderIsNew(t *testing.T) {
	home := filepath.Join(t.TempDir(), "data")
	fresh := freshFolder(home)
	svc := runService(t, home, nil, 0)
	if got, want := svc.dataFolderLine(home, fresh), "Data folder: "+home+" (new)"; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
}

// agentbus up on a folder an earlier run used says what it holds: here the
// agent that run saved and the session of its one turn.
func TestUpSaysWhatAnExistingDataFolderHolds(t *testing.T) {
	home := filepath.Join(t.TempDir(), "data")
	svc := runService(t, home, nil, 0)
	tok := readToken(t, home)
	if code, ag := sendAPI(t, svc, tok, "POST", "/api/v0/agents", map[string]any{"name": "Reviewer", "harness": "claude", "access": "subscription"}); code != http.StatusCreated {
		t.Fatalf("saving an agent: %d %v", code, ag)
	}
	session, _ := startRun(t, svc, tok, "claude", "")
	waitClosed(t, svc.store, session)
	svc.stop()

	fresh := freshFolder(home)
	svc = runService(t, home, nil, 0)
	if got, want := svc.dataFolderLine(home, fresh), "Data folder: "+home+" (existing: 1 agent, 1 session)"; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
}
