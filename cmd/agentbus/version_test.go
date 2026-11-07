package main

import (
	"bytes"
	"os"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/staysup/agentbus/api"
)

func TestVersionCommand(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-version"} {
		var out, errs bytes.Buffer
		if code := run([]string{arg}, os.Getenv, &out, &errs); code != 0 || errs.Len() != 0 ||
			!strings.HasPrefix(out.String(), "agentbus ") || !strings.HasSuffix(out.String(), "\n") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want agentbus <version> on stdout", arg, code, out.String(), errs.String())
		}
	}
}

func TestVersionLine(t *testing.T) {
	src := &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261106195453-e4fed25c0ffe"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "e4fed25c0ffee0000000000000000000000000ab"}, {Key: "vcs.modified", Value: "false"}}}
	dirty := &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261106195453-e4fed25c0ffe+dirty"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "e4fed25c0ffee0000000000000000000000000ab"}, {Key: "vcs.modified", Value: "true"}}}
	installed := &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}
	for _, c := range []struct {
		name            string
		version, commit string
		info            *debug.BuildInfo
		ok              bool
		want            string
	}{
		{"release", "0.1.0", "5fea80b5aaaabbbbccccddddeeeeffff00001111", dirty, true,
			"agentbus 0.1.0 (commit 5fea80b5aaaabbbbccccddddeeeeffff00001111)"},
		{"clone", "dev", "", src, true, "agentbus dev (commit e4fed25c0ffee0000000000000000000000000ab)"},
		{"clone with changes", "dev", "", dirty, true, "agentbus dev (commit e4fed25c0ffee0000000000000000000000000ab, modified)"},
		{"go install @v0.1.0", "dev", "", installed, true, "agentbus 0.1.0"},
		{"no build info", "dev", "", nil, false, "agentbus dev"},
	} {
		version, commit = c.version, c.commit
		if got := versionLine(c.info, c.ok); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	version, commit = "dev", ""
}

// build gives the API the same values versionLine prints.
func TestBuildFields(t *testing.T) {
	dirty := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "e4fed25c0ffee0000000000000000000000000ab"}, {Key: "vcs.modified", Value: "true"}}}
	noCommit := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}}
	for _, c := range []struct {
		name            string
		version, commit string
		info            *debug.BuildInfo
		want            api.Version
	}{
		{"release", "0.1.0", "5fea80b5aaaabbbbccccddddeeeeffff00001111", dirty,
			api.Version{Version: "0.1.0", Commit: "5fea80b5aaaabbbbccccddddeeeeffff00001111"}},
		{"clone with changes", "dev", "", dirty,
			api.Version{Version: "dev", Commit: "e4fed25c0ffee0000000000000000000000000ab", Modified: true}},
		{"no commit", "dev", "", noCommit, api.Version{Version: "dev"}},
	} {
		version, commit = c.version, c.commit
		if got := build(c.info, true); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	version, commit = "dev", ""
}
