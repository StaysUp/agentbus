package sandbox

import (
	"os"
	"strings"
	"testing"
)

// Each Linux switch that refuses user namespaces is named.
func TestUserNSHint(t *testing.T) {
	for _, c := range []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "1\n"}, "AppArmor"},
		{map[string]string{"/proc/sys/kernel/unprivileged_userns_clone": "0\n"}, "unprivileged_userns_clone"},
		{map[string]string{"/proc/sys/user/max_user_namespaces": "0\n"}, "max_user_namespaces"},
		{map[string]string{"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "0\n", "/proc/sys/user/max_user_namespaces": "63000"}, ""},
	} {
		_, got := userNSHint(func(p string) ([]byte, error) {
			if v, ok := c.files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}, func() string { return "; AppArmor allows it only with a profile" })
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%v: %q, want %q in it", c.files, got, c.want)
		}
	}
	// A program with its own profile is not told AppArmor refuses it.
	one := func(string) ([]byte, error) { return []byte("1"), nil }
	if word, got := userNSHint(one, func() string { return "" }); word != "" || got != "" {
		t.Errorf("with a profile: %q %q, want none", word, got)
	}
}
