package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/staysup/agentbus/isolation"
)

// Only up and runner start themselves again in a namespace; AGENTBUS_USERNS
// =off keeps them out, and an overlay's refusal says so.
func TestUsernsOnlyWhereItShould(t *testing.T) {
	env := func(k string) string {
		if k == usernsEnv {
			return usernsOff
		}
		return ""
	}
	for _, args := range [][]string{nil, {"server"}, {"version"}, {"api", "GET", "/api/v0/turns"}, {"up"}, {"runner", "-server", "http://x"}} {
		if code, done := userns(args, env, io.Discard); done || code != 0 {
			t.Fatalf("%v: done %v, code %d", args, done, code)
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("root mounts without a namespace")
	}
	if why := isolation.OverlayWhy(t.TempDir()); !strings.Contains(why, usernsEnv+"="+usernsOff) {
		t.Fatalf("the refusal does not say why there is no namespace: %q", why)
	}
}
