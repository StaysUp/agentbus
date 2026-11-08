package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each mode takes its own flags, and a line it does not take exits 2 with
// the reason.
func TestModeFlags(t *testing.T) {
	home := t.TempDir()
	env := func(k string) string {
		if k == "AGENTBUS_HOME" {
			return home
		}
		return ""
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"down"}, "usage:"},
		{[]string{"runner"}, "-server must be the server's URL"},
		{[]string{"runner", "-server", "ftp://x"}, "-server must be the server's URL"},
		{[]string{"runner", "-server", "http://127.0.0.1:1", "-gateway", "server"}, "-gateway server is not built yet"},
		{[]string{"runner", "-server", "http://127.0.0.1:1", "-gateway", "other"}, "-gateway must be local or server"},
		{[]string{"runner", "-server", "http://127.0.0.1:1", "-capacity", "65"}, "-capacity must be 1 to 64"},
		{[]string{"runner", "-server", "http://127.0.0.1:1", "-max-runs", "2"}, "not defined: -max-runs"},
		{[]string{"server", "-max-runs", "2"}, "not defined: -max-runs"},
		{[]string{"up", "-max-runs", "0"}, "-max-runs must be 1 to 64"},
	} {
		var b bytes.Buffer
		if code := run(c.args, env, &b, &b); code != 2 || !strings.Contains(b.String(), c.want) {
			t.Errorf("%v: exit %d, %q; want 2 and %q", c.args, code, b.String(), c.want)
		}
	}
	// A runner whose server's enroll_token was not copied over says where it
	// looked, and starts nothing.
	var b bytes.Buffer
	if code := run([]string{"runner", "-server", "http://127.0.0.1:1"}, env, &b, &b); code != 1 ||
		!strings.Contains(b.String(), filepath.Join(home, "enroll_token")+" does not exist") {
		t.Errorf("no enroll token: exit %d, %q", code, b.String())
	}
	if _, err := os.Stat(filepath.Join(home, "events.db")); err == nil {
		t.Error("a runner that could not start opened a store")
	}
}
