package sandbox

import (
	"errors"
	"os"
	"testing"

	"github.com/staysup/agentbus/netjail"
)

// Can answers false with the wall's own reason, and the system's hint after
// it; true when the wall was built, and then it is taken down.
func TestCan(t *testing.T) {
	refuse := func(Spec) (*Box, error) { return nil, &Error{Why: "bubblewrap could not start a program in it"} }
	apparmor := func() (string, string) { return "AppArmor", "; AppArmor allows it only to a program with a profile" }
	ok, why := can(refuse, apparmor)
	if ok || why != "bubblewrap could not start a program in it; AppArmor allows it only to a program with a profile" {
		t.Fatalf("can = %v %q", ok, why)
	}
	// A reason that names the setting already is not told it twice.
	named := func(Spec) (*Box, error) {
		return nil, &Error{Why: "refused; on Ubuntu AppArmor allows it only with a profile"}
	}
	if _, why := can(named, apparmor); why != "refused; on Ubuntu AppArmor allows it only with a profile" {
		t.Fatalf("said twice: %q", why)
	}
	other := func(Spec) (*Box, error) { return nil, errors.New("quoted /secret/path") }
	if ok, why := can(other, func() (string, string) { return "", "" }); ok || why != "the sandbox could not be built" {
		t.Fatalf("a foreign error's text is not passed on: %v %q", ok, why)
	}
	var seen Spec
	built := func(s Spec) (*Box, error) {
		seen = s
		for _, d := range []string{s.Copy, s.Home} {
			if info, err := os.Stat(d); err != nil || !info.IsDir() {
				t.Fatalf("the check's folder %s is not there", d)
			}
		}
		return &Box{jail: netjail.New(netjail.Spec{})}, nil
	}
	if ok, why := can(built, func() (string, string) { return "never", "never" }); !ok || why != "" {
		t.Fatalf("can = %v %q", ok, why)
	}
	if _, err := os.Stat(seen.Copy); !os.IsNotExist(err) {
		t.Fatal("the check's folder was left behind")
	}
}
