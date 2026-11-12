package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
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

// The usage text lists the commands a person runs, then agentbus api in a
// group of its own, and no agentbus connect.
func TestUsageGroups(t *testing.T) {
	var b bytes.Buffer
	if code := run(nil, os.Getenv, &b, &b); code != 2 || b.String() != usage+"\n" {
		t.Fatalf("exit %d, %q", code, b.String())
	}
	main, agents, ok := strings.Cut(usage, "\n\nFor agents and scripts:\n")
	if !ok || strings.Contains(main, "agentbus api") || !strings.Contains(agents, "agentbus api METHOD PATH [BODY]") {
		t.Fatalf("groups: %q", usage)
	}
	if strings.Contains(usage, "connect") {
		t.Fatalf("usage names connect: %q", usage)
	}
}

// agentbus connect is removed: one line pointing to the cockpit's Connect,
// exit 2, and nothing made in the data folder.
func TestConnectRemoved(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	env := func(k string) string {
		if k == "AGENTBUS_HOME" {
			return home
		}
		return ""
	}
	for _, args := range [][]string{{"connect"}, {"connect", "claude"}, {"connect", "codex", "-x"}} {
		var out, errb bytes.Buffer
		if code := run(args, env, &out, &errb); code != 2 || out.Len() != 0 ||
			errb.String() != "agentbus connect was removed in 0.5: connect a subscription in the cockpit (Agents, the agent's Connect step).\n" {
			t.Errorf("%v: exit %d, %q %q", args, code, out.String(), errb.String())
		}
	}
	if _, err := os.Stat(home); err == nil {
		t.Error("agentbus connect made the data folder")
	}
}

// agentbus api works from the top-level command as before: the call goes to
// AGENTBUS_URL with the turn's token, and a bad line prints its own usage.
func TestAPICommandUnchanged(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	env := func(k string) string {
		return map[string]string{"AGENTBUS_URL": srv.URL, "AGENTBUS_TOKEN": "abt_test"}[k]
	}
	var out, errb bytes.Buffer
	if code := run([]string{"api", "GET", "/api/v0/turns"}, env, &out, &errb); code != 0 ||
		got != "GET /api/v0/turns Bearer abt_test" || !strings.Contains(out.String(), `"ok":true`) {
		t.Fatalf("exit %d, %q, %q %q", code, got, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"api", "GET"}, env, &out, &errb); code != 2 || errb.String() != apiUsage+"\n" {
		t.Fatalf("exit %d, %q", code, errb.String())
	}
}
