package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake key: the shape OpenRouter's have, no one's credential.
const fakeORKey = "sk-or-v1-fake0000111122223333444455556666777788889999"

func TestKeyKeptOwnerOnly(t *testing.T) {
	s := &Subscriptions{Dir: filepath.Join(t.TempDir(), "subscriptions")}
	if st, err := s.KeyStatus(AccessOpenRouter); err != nil || st.Set {
		t.Fatalf("before: %+v %v", st, err)
	}
	if err := s.SetKey(AccessOpenRouter, "  "+fakeORKey+"\n"); err != nil {
		t.Fatal(err)
	}
	st, err := s.KeyStatus(AccessOpenRouter)
	if err != nil || !st.Set || st.SetAt == nil {
		t.Fatalf("after: %+v %v", st, err)
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "openrouter.key.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key's file: %v %v", fi, err)
	}
	if di, err := os.Stat(s.Dir); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("the folder: %v %v", di, err)
	}
	// Not a subscription: no harness reads it.
	if st, _ := s.Status("openrouter"); st.Connected {
		t.Fatal("the key reads as a subscription")
	}
	k := KeyFirst{Kept: s, Env: Env{Getenv: func(string) string { return "" }}}
	if c, err := k.Credential(Need{Access: AccessOpenRouter, KeyEnv: "OPENROUTER_API_KEY"}); err != nil || c.Token != fakeORKey {
		t.Fatalf("the trimmed key was not given: %v", err)
	}
	// Replace, then remove; removing twice is not an error.
	if err := s.SetKey(AccessOpenRouter, fakeORKey+"x"); err != nil {
		t.Fatal(err)
	}
	if c, _ := k.Credential(Need{Access: AccessOpenRouter}); c.Token != fakeORKey+"x" {
		t.Fatal("the key was not replaced")
	}
	if err := s.RemoveKey(AccessOpenRouter); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveKey(AccessOpenRouter); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.KeyStatus(AccessOpenRouter); st.Set {
		t.Fatal("still set after remove")
	}
	left, _ := os.ReadDir(s.Dir)
	if len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
}

func TestKeyRefusedShapes(t *testing.T) {
	s := &Subscriptions{Dir: t.TempDir()}
	for _, bad := range []string{"", "   ", "a b", "a\tb", "a\nb", "a\x00b", "a\x7fb", strings.Repeat("k", MaxKeyLen+1)} {
		if err := s.SetKey(AccessOpenRouter, bad); !errors.Is(err, ErrBadKey) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if err := s.SetKey("subscription", fakeORKey); err == nil {
		t.Fatal("a key kept for another access")
	}
	if left, _ := os.ReadDir(s.Dir); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
}

// A key set in Settings wins over the variable; without one the variable
// is read, as before; with neither, the refusal says where to set it and
// never quotes a value.
func TestKeyFirstOverEnv(t *testing.T) {
	s := &Subscriptions{Dir: t.TempDir()}
	env := Env{Getenv: func(k string) string {
		if k == "AB_KEY" {
			return "fake-env-key"
		}
		return ""
	}}
	p := Plug{AccessOpenRouter: KeyFirst{Kept: s, Env: env}}
	n := Need{Access: AccessOpenRouter, KeyEnv: "AB_KEY"}
	if c, err := p.Credential(n); err != nil || c.Token != "fake-env-key" || p.Ready(n) != "" {
		t.Fatalf("env: %v", err)
	}
	if err := s.SetKey(AccessOpenRouter, fakeORKey); err != nil {
		t.Fatal(err)
	}
	if c, err := p.Credential(n); err != nil || c.Token != fakeORKey {
		t.Fatalf("settings did not win: %v", err)
	}
	// An agent naming an unset or unusable variable still gets it.
	for _, name := range []string{"", "AB_UNSET", "1X"} {
		if c, err := p.Credential(Need{Access: AccessOpenRouter, KeyEnv: name}); err != nil || c.Token != fakeORKey {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if err := s.RemoveKey(AccessOpenRouter); err != nil {
		t.Fatal(err)
	}
	why := p.Ready(Need{Access: AccessOpenRouter, KeyEnv: "AB_UNSET"})
	if why != "set your OpenRouter key in Settings, or set AB_UNSET where control runs and restart agentbus" {
		t.Fatalf("%q", why)
	}
	if why := p.Ready(Need{Access: AccessOpenRouter, KeyEnv: "1X"}); why != "set your OpenRouter key in Settings" {
		t.Fatalf("%q", why)
	}
	// A key file that cannot be used is refused in fixed words.
	if err := os.WriteFile(filepath.Join(s.Dir, "openrouter.key.json"), []byte("not json "+fakeORKey), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := p.Credential(n)
	if err == nil || strings.Contains(err.Error(), fakeORKey) || !strings.Contains(err.Error(), "set it again in Settings") {
		t.Fatalf("%v", err)
	}
	if _, err := s.KeyStatus(AccessOpenRouter); err == nil || strings.Contains(err.Error(), fakeORKey) {
		t.Fatalf("%v", err)
	}
	// No store: the variable only, with its old wording.
	if why := (KeyFirst{Env: env}).Ready(Need{Access: AccessOpenRouter, KeyEnv: "AB_UNSET"}); !strings.Contains(why, "restart agentbus") || strings.Contains(why, "Settings") {
		t.Fatalf("%q", why)
	}
}

// Each vendor's API key, set in Settings, goes to its own harness's agents,
// in its vendor's header; one not set says where to set it, and quotes no
// key.
func TestAPIKeysByHarness(t *testing.T) {
	const fakeAnth, fakeOAI = "sk-ant-api03-fake0000111122223333", "sk-proj-fake0000111122223333"
	s := &Subscriptions{Dir: filepath.Join(t.TempDir(), "subscriptions")}
	k := APIKeys{Kept: s, Of: map[string]VendorKey{
		"claude": {Name: KeyAnthropic, Label: "Anthropic API key", Header: "X-Api-Key"},
		"codex":  {Name: KeyOpenAI, Label: "OpenAI API key"},
	}}
	if why := k.Ready(Need{Access: AccessAPI, Harness: "claude"}); why != "set your Anthropic API key in Settings" {
		t.Fatalf("before: %q", why)
	}
	if err := s.SetKey(KeyAnthropic, fakeAnth); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKey(KeyOpenAI, fakeOAI); err != nil {
		t.Fatal(err)
	}
	if c, err := k.Credential(Need{Access: AccessAPI, Harness: "claude"}); err != nil || c.Token != fakeAnth || c.Header != "X-Api-Key" {
		t.Fatalf("claude: %v %v", c.Header, err)
	}
	if c, err := k.Credential(Need{Access: AccessAPI, Harness: "codex"}); err != nil || c.Token != fakeOAI || c.Header != "" {
		t.Fatalf("codex: %v %v", c.Header, err)
	}
	_, err := k.Credential(Need{Access: AccessAPI, Harness: "agy"})
	var m *Missing
	if !errors.As(err, &m) || strings.Contains(m.Why, "sk-") {
		t.Fatalf("agy: %v", err)
	}
	// A file that cannot be read is never taken for no key, nor quoted.
	p, _ := s.KeyFile(KeyAnthropic)
	if err := os.WriteFile(p, []byte("not json "+fakeAnth), 0o600); err != nil {
		t.Fatal(err)
	}
	if why := k.Ready(Need{Access: AccessAPI, Harness: "claude"}); !strings.Contains(why, "cannot be read") || strings.Contains(why, fakeAnth) {
		t.Fatalf("unreadable: %q", why)
	}
	if _, err := s.KeyFile("claude"); err == nil {
		t.Fatal("a name that is no key has a file")
	}
}
