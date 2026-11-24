package homefiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// CopyGrants puts a file and a folder at their place in the session's
// home, in place of what was there, and leaves symlinks inside a folder
// out.
func TestCopyGrants(t *testing.T) {
	user, home := t.TempDir(), t.TempDir()
	write(t, filepath.Join(user, ".aws", "config"), "[profile a]\n")
	write(t, filepath.Join(user, ".aws", "credentials"), "not granted\n")
	write(t, filepath.Join(user, ".config", "gcloud", "a", "b.json"), "{}")
	if err := os.Symlink(filepath.Join(user, ".aws"), filepath.Join(user, ".config", "gcloud", "link")); err != nil {
		t.Fatal(err)
	}
	if err := CopyGrants(home, home+".grants", user, []string{"~/.aws/config", "~/.config/gcloud"}, signIns); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".aws", "config")); err != nil || string(b) != "[profile a]\n" {
		t.Errorf(".aws/config: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "credentials")); err == nil {
		t.Error(".aws/credentials was copied, and only .aws/config was granted")
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "gcloud", "a", "b.json")); err != nil {
		t.Error("a folder's file was not copied")
	}
	if _, err := os.Lstat(filepath.Join(home, ".config", "gcloud", "link")); err == nil {
		t.Error("a symlink inside a granted folder was copied")
	}
}

// A grant that is missing, outside ~, or too big is refused with an Error;
// a symlink the agent left in its home is not followed out of it.
func TestCopyGrantsRefuses(t *testing.T) {
	user, home := t.TempDir(), t.TempDir()
	var e *Error
	for _, g := range []string{"~/not-there", "/etc/passwd", "~/../x", "~/"} {
		if err := CopyGrants(home, home+".grants", user, []string{g}, signIns); !errors.As(err, &e) {
			t.Errorf("%s: %v, want an Error", g, err)
		}
	}
	big := filepath.Join(user, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxGrant + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := CopyGrants(home, home+".grants", user, []string{"~/big"}, signIns); !errors.As(err, &e) || !strings.Contains(err.Error(), "64 MiB") {
		t.Errorf("too big: %v", err)
	}
	// The agent made .aws a link to a folder outside its home.
	outside := t.TempDir()
	write(t, filepath.Join(user, ".aws", "config"), "[profile a]\n")
	if err := os.Symlink(outside, filepath.Join(home, ".aws")); err != nil {
		t.Fatal(err)
	}
	_ = CopyGrants(home, home+".grants", user, []string{"~/.aws/config"}, signIns)
	if _, err := os.Stat(filepath.Join(outside, "config")); err == nil {
		t.Error("the grant was written through a symlink out of the home")
	}
}

// An entry taken out of the list is gone from the home at the next call,
// and so is every entry when the list is empty (review of 2026-11-11, round
// 3). The record lives beside the home, not in it.
func TestCopyGrantsRevokes(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home, record := filepath.Join(dir, "S"), filepath.Join(dir, "S.grants")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(user, ".aws", "config"), "[profile a]\n")
	write(t, filepath.Join(user, ".gitconfig"), "[user]\n")
	if err := CopyGrants(home, record, user, []string{"~/.aws/config", "~/.gitconfig"}, signIns); err != nil {
		t.Fatal(err)
	}
	if err := CopyGrants(home, record, user, []string{"~/.gitconfig"}, signIns); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "config")); err == nil {
		t.Error("an entry taken out is still in the home")
	}
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err != nil {
		t.Error("an entry kept is gone")
	}
	if err := CopyGrants(home, record, user, nil, signIns); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err == nil {
		t.Error("with no entries left, one is still in the home")
	}
	// A record not of this shape is refused, never followed.
	write(t, record, "../outside")
	if err := CopyGrants(home, record, user, nil, signIns); err == nil {
		t.Error("a record naming a path out of the home was used")
	}
}

// A grant writes files beside what the session has, and replaces one of
// the session's own only when the original is newer; a refused call
// leaves the home as it was; the files it wrote follow the original and,
// taken out, are removed, and what the session wrote beside them stays
// (reviews of 2026-11-11/12, rounds 3, 4, 6 and 7; 0.5.7: newer wins).
func TestCopyGrantsFilesOnly(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home, record := filepath.Join(dir, "S"), filepath.Join(dir, "S.grants")
	write(t, filepath.Join(home, ".claude", "projects", "s.jsonl"), "{}")
	write(t, filepath.Join(user, ".claude", "settings.json"), "{}")
	write(t, filepath.Join(user, ".aws", "config"), "[profile a]\n")
	write(t, filepath.Join(user, ".aws", "sso", "cache.json"), "{}")
	// Beside the session's own folder: merged, its file stays.
	if err := CopyGrants(home, record, user, []string{"~/.claude"}, signIns); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "projects", "s.jsonl")); err != nil {
		t.Error("the session's own file is gone")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Error("the granted file was not copied")
	}
	// Over one of the session's own files that is newer: it stays.
	write(t, filepath.Join(user, ".claude", "projects", "s.jsonl"), "theirs")
	age(t, filepath.Join(user, ".claude", "projects", "s.jsonl"), time.Hour)
	if err := CopyGrants(home, record, user, []string{"~/.claude"}, signIns); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "projects", "s.jsonl")); string(b) != "{}" {
		t.Error("the session's own newer file was replaced")
	}
	// The original newer: it replaces the session's file.
	age(t, filepath.Join(home, ".claude", "projects", "s.jsonl"), 2*time.Hour)
	if err := CopyGrants(home, record, user, []string{"~/.claude"}, signIns); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "projects", "s.jsonl")); string(b) != "theirs" {
		t.Errorf("s.jsonl: %q, want the newer original", b)
	}
	// Over something that is not a file: refused, nothing changes.
	if err := os.MkdirAll(filepath.Join(home, ".gitconfig"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(user, ".gitconfig"), "[user]\n")
	var e *Error
	if err := CopyGrants(home, record, user, []string{"~/.aws/config", "~/.gitconfig"}, signIns); !errors.As(err, &e) || !strings.Contains(err.Error(), "not a file") {
		t.Fatalf("a grant over the session's own folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aws")); err == nil {
		t.Error("a refused call copied part of its grants")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Error("a refused call removed an earlier grant's file")
	}
	// Widened from a file to its folder; the agent changes the copy, then
	// the original changes.
	for _, step := range [][]string{{"~/.aws/config"}, {"~/.aws"}} {
		if err := CopyGrants(home, record, user, step, signIns); err != nil {
			t.Fatalf("%v: %v", step, err)
		}
	}
	write(t, filepath.Join(home, ".aws", "config"), "changed by the agent\n")
	write(t, filepath.Join(home, ".aws", "sso", "login.json"), "the session's own")
	if err := CopyGrants(home, record, user, []string{"~/.aws"}, signIns); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".aws", "config")); string(b) != "changed by the agent\n" {
		t.Errorf(".aws/config: %q, want the agent's newer change", b)
	}
	write(t, filepath.Join(user, ".aws", "config"), "[profile b]\n")
	age(t, filepath.Join(home, ".aws", "config"), time.Hour)
	if err := CopyGrants(home, record, user, []string{"~/.aws"}, signIns); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".aws", "config")); string(b) != "[profile b]\n" {
		t.Errorf(".aws/config: %q, want the newer original", b)
	}
	// Taken out: its files go, the session's own stay.
	if err := CopyGrants(home, record, user, nil, signIns); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".aws/config", ".aws/sso/cache.json", ".claude/settings.json", ".claude/projects/s.jsonl"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			t.Errorf("%s stays once its grant is taken out", f)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "sso", "login.json")); err != nil {
		t.Error("a file the session wrote beside a grant is gone")
	}
}

// signIns stands for settings.SignInFile, which homefiles may not use: the
// same paths.
func signIns(rel string) string {
	return map[string]string{".claude/.credentials.json": "Claude Code's sign-in", ".claude/.claude.json": "Claude Code's account file",
		".codex/auth.json": "codex's sign-in", ".gemini/antigravity-cli/antigravity-oauth-token": "agy's sign-in"}[filepath.Clean(rel)]
}

// age sets p's time to d ago.
func age(t *testing.T, p string, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// A copy keeps the original's time, so an unchanged original is not
// copied again: a file the agent left as it was stays the same file.
func TestCopyGrantsKeepsTime(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home, record := filepath.Join(dir, "S"), filepath.Join(dir, "S.grants")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(user, ".config", "gh", "a", "b", "hosts.yml"), "x")
	age(t, filepath.Join(user, ".config", "gh", "a", "b", "hosts.yml"), time.Hour)
	for range 2 {
		if err := CopyGrants(home, record, user, []string{"~/.config/gh"}, signIns); err != nil {
			t.Fatal(err)
		}
	}
	src, _ := os.Stat(filepath.Join(user, ".config", "gh", "a", "b", "hosts.yml"))
	dst, err := os.Stat(filepath.Join(home, ".config", "gh", "a", "b", "hosts.yml"))
	if err != nil || !dst.ModTime().Equal(src.ModTime()) {
		t.Fatalf("a file deep in a folder: %v, time %v, want %v", err, dst, src.ModTime())
	}
}

// A folder of more than MaxGrantFiles files is refused in plain words.
func TestCopyGrantsTooManyFiles(t *testing.T) {
	user, home := t.TempDir(), t.TempDir()
	for i := range MaxGrantFiles + 1 {
		write(t, filepath.Join(user, "many", strconv.Itoa(i)), "")
	}
	var e *Error
	if err := CopyGrants(home, home+".grants", user, []string{"~/many"}, signIns); !errors.As(err, &e) || !strings.Contains(err.Error(), "10000 files") {
		t.Fatalf("too many files: %v", err)
	}
}

// A vendor sign-in never goes into a session's home: named, the call is
// refused; inside a folder listed, or reached through a link, it is left
// out and the rest goes.
func TestCopyGrantsSignInsNever(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home, record := filepath.Join(dir, "S"), filepath.Join(dir, "S.grants")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".claude/.credentials.json", ".codex/auth.json", ".gemini/antigravity-cli/antigravity-oauth-token", ".claude/.claude.json"} {
		write(t, filepath.Join(user, f), "secret-shaped")
	}
	write(t, filepath.Join(user, ".claude", "settings.json"), "{}")
	write(t, filepath.Join(user, ".codex", "config.toml"), "")
	if err := os.Symlink(filepath.Join(user, ".codex", "auth.json"), filepath.Join(user, "link")); err != nil {
		t.Fatal(err)
	}
	var e *Error
	for _, g := range []string{"~/.claude/.credentials.json", "~/.codex/auth.json", "~/.gemini/antigravity-cli/antigravity-oauth-token", "~/link"} {
		if err := CopyGrants(home, record, user, []string{g}, signIns); !errors.As(err, &e) || !strings.Contains(err.Error(), "sign-in") {
			t.Errorf("%s: %v, want a refusal naming the sign-in", g, err)
		}
	}
	if err := CopyGrants(home, record, user, []string{"~/.claude", "~/.codex", "~/.gemini"}, signIns); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".claude/.credentials.json", ".codex/auth.json", ".gemini/antigravity-cli/antigravity-oauth-token", ".claude/.claude.json", "link"} {
		if _, err := os.Lstat(filepath.Join(home, f)); err == nil {
			t.Errorf("%s went into the session's home", f)
		}
	}
	for _, f := range []string{".claude/settings.json", ".codex/config.toml"} {
		if _, err := os.Stat(filepath.Join(home, f)); err != nil {
			t.Errorf("%s did not go", f)
		}
	}
}

// ~/.claude.json gives its mcpServers only: merged into the session's own
// .claude.json, at the home and in its claude config folder, beside the
// session's own keys, never the account or sign-in fields. Taken out, the
// part goes and the session's file stays.
func TestCopyGrantsClaudeJSON(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home, record := filepath.Join(dir, "S"), filepath.Join(dir, "S.grants")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(user, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@b"},"userID":"u1","primaryApiKey":"k",`+
		`"projects":{"/x":{}},"mcpServers":{"fs":{"command":"npx","args":["fs"]}}}`)
	write(t, filepath.Join(home, ".claude", ".claude.json"), `{"numStartups":3,"mcpServers":{"old":{}}}`)
	if err := CopyGrants(home, record, user, []string{"~/.claude.json"}, signIns); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) map[string]json.RawMessage {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return m
	}
	for _, rel := range []string{".claude.json", ".claude/.claude.json"} {
		m := read(rel)
		var servers map[string]any
		if err := json.Unmarshal(m["mcpServers"], &servers); err != nil || len(servers) != 1 || servers["fs"] == nil {
			t.Errorf("%s: mcpServers %s", rel, m["mcpServers"])
		}
		for _, k := range []string{"oauthAccount", "userID", "primaryApiKey", "projects"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s: %s was copied", rel, k)
			}
		}
	}
	if m := read(".claude/.claude.json"); string(m["numStartups"]) != "3" {
		t.Errorf("the session's own key is gone: %v", m)
	}
	if err := CopyGrants(home, record, user, nil, signIns); err != nil {
		t.Fatal(err)
	}
	m := read(".claude/.claude.json")
	if _, ok := m["mcpServers"]; ok || string(m["numStartups"]) != "3" {
		t.Errorf("taken out: %v", m)
	}
}

// A grant that is a named pipe is refused, never waited on (review of
// 2026-11-12, round 7).
func TestCopyGrantsPipe(t *testing.T) {
	user, dir := t.TempDir(), t.TempDir()
	home := filepath.Join(dir, "S")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(user, "pipe")); err != nil {
		t.Skip("no named pipes here")
	}
	done := make(chan error, 1)
	go func() { done <- CopyGrants(home, home+".grants", user, []string{"~/pipe"}, signIns) }()
	select {
	case err := <-done:
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("a pipe: %v, want an Error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CopyGrants waits on a named pipe")
	}
}
