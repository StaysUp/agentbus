//go:build darwin

package sandbox

import (
	"strings"
	"testing"
)

// The Seatbelt profile denies the secret paths and any .env outside the
// copy and home, and the deny comes after the allows, so a deny of a path a
// Read grant also allows wins (Seatbelt takes the last matching rule).
func TestProfileHidesSecrets(t *testing.T) {
	secret := []string{"/home/probe/.ssh", "/home/probe/.aws", "/home/probe/.npmrc"}
	p := profile("/copy", false, "/home", []path{{real: "/home/probe", dir: true}}, secret, []int{3128})
	prof := p.profile

	if i, j := strings.Index(prof, "(allow file-read*"), strings.LastIndex(prof, "(deny file-read*"); i < 0 || j < 0 || j < i {
		t.Fatalf("deny does not come after allow (allow at %d, deny at %d)", i, j)
	}
	params := strings.Join(p.params, " ")
	for _, d := range secret {
		if !strings.Contains(params, d) {
			t.Errorf("secret %q is not a profile parameter", d)
		}
	}
	// The .env rule denies by name but spares the copy and the home.
	if !strings.Contains(prof, `.env`) || !strings.Contains(prof, "require-not") {
		t.Errorf(".env-outside-copy deny missing:\n%s", prof)
	}
}
