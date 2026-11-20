package sandbox

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// gitDefaultsEnv rewrites both public ssh git hosts to HTTPS and rules ssh
// out, with a key/value count that matches its entries.
func TestGitDefaultsEnv(t *testing.T) {
	m := map[string]string{}
	for _, e := range gitDefaultsEnv() {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	n, err := strconv.Atoi(m["GIT_CONFIG_COUNT"])
	if err != nil || n == 0 {
		t.Fatalf("GIT_CONFIG_COUNT = %q", m["GIT_CONFIG_COUNT"])
	}
	if m["GIT_SSH_COMMAND"] != "false" {
		t.Errorf("GIT_SSH_COMMAND = %q, want false (ssh ruled out)", m["GIT_SSH_COMMAND"])
	}
	var gitHub, gitLab bool
	for i := 0; i < n; i++ {
		s := strconv.Itoa(i)
		k, v := m["GIT_CONFIG_KEY_"+s], m["GIT_CONFIG_VALUE_"+s]
		if !strings.HasPrefix(k, "url.https://") || !strings.HasSuffix(k, ".insteadOf") {
			t.Errorf("entry %d key %q is not an insteadOf rewrite to https", i, k)
		}
		if k == "url.https://github.com/.insteadOf" && v == "git@github.com:" {
			gitHub = true
		}
		if k == "url.https://gitlab.com/.insteadOf" && v == "git@gitlab.com:" {
			gitLab = true
		}
	}
	if !gitHub || !gitLab {
		t.Errorf("github (%t) or gitlab (%t) ssh remote not rewritten", gitHub, gitLab)
	}
}

// secretPaths lists every named secret under the home it is told to use, so
// a test never has to name a real one.
func TestSecretPaths(t *testing.T) {
	old := homeDir
	homeDir = func() (string, error) { return "/home/probe", nil }
	defer func() { homeDir = old }()
	got := map[string]bool{}
	for _, p := range secretPaths() {
		got[p] = true
	}
	for _, rel := range []string{".ssh", ".aws", ".gnupg", ".config/gh", ".docker/config.json", ".npmrc", ".netrc"} {
		if p := filepath.Join("/home/probe", rel); !got[p] {
			t.Errorf("secretPaths is missing %q", p)
		}
	}
}
