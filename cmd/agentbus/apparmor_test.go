package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install.sh writes the AppArmor profile from its own copy of
// packaging/apparmor/agentbus: the rules must be the same, but the path.
func TestAppArmorProfileInInstall(t *testing.T) {
	rules := func(file, path string) string {
		b, err := os.ReadFile(filepath.Join(root(t), file))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		i := strings.Index(s, "abi <abi/4.0>,")
		j := strings.Index(s[max(i, 0):], "\n}\n")
		if i < 0 || j < 0 {
			t.Fatalf("%s holds no profile", file)
		}
		s = s[i : i+j+3]
		if !strings.Contains(s, `profile agentbus "`+path+`" flags=(unconfined) {`) {
			t.Fatalf("%s: the profile does not name %s:\n%s", file, path, s)
		}
		return strings.Replace(s, path, "BINARY", 1)
	}
	want := rules("packaging/apparmor/agentbus", "/usr/local/bin/agentbus")
	if got := rules("install.sh", "$1"); got != want {
		t.Fatalf("install.sh's profile:\n%s\nwant packaging/apparmor/agentbus's:\n%s", got, want)
	}
	if !strings.Contains(want, "\n  userns,\n") {
		t.Fatal("the profile does not allow userns")
	}
}
