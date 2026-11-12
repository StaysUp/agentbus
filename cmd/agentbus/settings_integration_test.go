//go:build integration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/api"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// A data folder of 0.4: control's first start copies it beside itself, then
// imports its settings files, and the import's changes are in the record.
// The next start copies and imports nothing, and a hand edit of a file is
// never read.
func TestStartUpgradesSettings(t *testing.T) {
	home := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(home, "sessions", "s1"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		settings.PolicyFile:       "forbid = [\"git push\"]\n",
		settings.InstructionsFile: "Be brief.\n",
		settings.AgentsFile: `{"agents":[{"id":"a_0123456789abcdef","name":"DeepSeek","harness":"claude","access":"openrouter",` +
			`"model":"deepseek/deepseek-v4.1-flash","key_env":"OPENROUTER_API_KEY","created":"2026-11-01T00:00:00Z","updated":"2026-11-01T00:00:00Z"}]}`,
		filepath.Join("sessions", "s1", "work"): "a session's copy",
	} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	startServer := func() *service {
		s, err := start(config{mode: modeServer, listen: "127.0.0.1:0", home: home, logf: t.Logf, heartbeat: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := startServer()
	tok := readToken(t, home)
	copyDir := settings.CopyName(home, time.Now())
	if b, err := os.ReadFile(filepath.Join(copyDir, settings.PolicyFile)); err != nil || !strings.Contains(string(b), "git push") {
		t.Fatalf("the copy: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(copyDir, "sessions")); err == nil {
		t.Fatal("the session copies were copied")
	}
	var p settings.Policy
	if code := callAPI(t, s, tok, "GET", "/api/v0/settings/permissions", nil, &p); code != 200 || len(p.Forbid) != 1 || p.Forbid[0] != "git push" {
		t.Fatalf("policy: %d %+v", code, p)
	}
	var in api.SettingsInfo
	if code := callAPI(t, s, tok, "GET", "/api/v0/settings", nil, &in); code != 200 || in.Version != 3 || in.Imported == nil ||
		strings.Join(in.ImportedFiles, ",") != "permissions.toml,instructions.md,agents.json" {
		t.Fatalf("settings: %d %+v", code, in)
	}
	// The file held one agent and none of the defaults: so do the agents.
	var list struct{ Agents []struct{ ID string } }
	if code := callAPI(t, s, tok, "GET", "/api/v0/agents", nil, &list); code != 200 || len(list.Agents) != 1 || list.Agents[0].ID != "a_0123456789abcdef" {
		t.Fatalf("agents: %d %+v", code, list)
	}
	// The import's changes are in the record, by file and key, no value.
	recs, err := s.store.After(0, store.Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, r := range recs {
		var ev event.Event
		if json.Unmarshal(r.Data, &ev) == nil && ev.Kind == event.SettingsChanged {
			files = append(files, ev.File)
			if strings.Contains(string(r.Data), "git push") || strings.Contains(string(r.Data), "brief") {
				t.Fatalf("a value is in the record: %s", r.Data)
			}
		}
	}
	if strings.Join(files, ",") != "permissions.toml,instructions.md,agents.json" {
		t.Fatalf("settings.changed files %v", files)
	}
	s.stop()

	// The second start: the files are not read again.
	if err := os.WriteFile(filepath.Join(home, settings.PolicyFile), []byte("forbid = [\"rm\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = startServer()
	defer s.stop()
	if code := callAPI(t, s, tok, "GET", "/api/v0/settings/permissions", nil, &p); code != 200 || p.Forbid[0] != "git push" {
		t.Fatalf("after the second start: %d %+v", code, p)
	}
	if code := callAPI(t, s, tok, "GET", "/api/v0/settings", nil, &in); code != 200 || in.Version != 3 {
		t.Fatalf("settings after the second start: %d %+v", code, in)
	}
	m, _ := filepath.Glob(filepath.Join(filepath.Dir(home), "*"+settings.CopySuffix+"*"))
	if len(m) != 1 {
		t.Fatalf("copies %v", m)
	}
}
