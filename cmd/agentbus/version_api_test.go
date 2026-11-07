package main

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
	"testing"

	"github.com/staysup/agentbus/api"
)

// GET /api/v0/version answers what agentbus version prints, and needs the
// token like the rest of the API.
func TestVersionInTheAPI(t *testing.T) {
	home := t.TempDir()
	svc := startService(t, home, "http://127.0.0.1:1")
	tok := readToken(t, home)
	if code := rawGet(t, svc.addr, "/api/v0/version", ""); code != http.StatusUnauthorized {
		t.Errorf("without a token: %d, want 401", code)
	}
	req, _ := http.NewRequest("GET", "http://"+svc.addr+"/api/v0/version", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d, %v", resp.StatusCode, err)
	}
	want := build(debug.ReadBuildInfo())
	b, _ := json.Marshal(raw)
	var got api.Version
	_ = json.Unmarshal(b, &got)
	if got != want || got.Version == "" {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if _, ok := raw["modified"]; !ok {
		t.Errorf("modified absent: %v", raw)
	}
}
