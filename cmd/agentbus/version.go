package main

// agentbus version: which build this is.

import (
	"runtime/debug"
	"strings"
)

// version and commit are set by release builds, through the ldflags in
// .goreleaser.yaml. A source build keeps these defaults and reads what Go
// recorded instead (versionLine).
var (
	version = "dev"
	commit  = ""
)

// versionLine is what agentbus version prints: the version, then the commit
// when one is known. A release build has both from its ldflags. A build from
// a clone has "dev" and the commit Go read from git, with ", modified" when
// the tree had changes. go install module@v0.1.0 has the module's version and
// no commit, since the module cache is not a git checkout.
func versionLine(info *debug.BuildInfo, ok bool) string {
	v, c, modified := version, commit, false
	if ok && info != nil {
		revision := ""
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		// A build from a clone also gets a made-up module version from git
		// (v0.0.0-<time>-<commit>+dirty, Go 1.24 and later), which only
		// repeats the commit; it keeps "dev". Only a build with no git
		// checkout, from the module cache, takes the module's version.
		if v == "dev" && revision == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = strings.TrimPrefix(info.Main.Version, "v")
		}
		if c == "" {
			c = revision
		} else {
			modified = false // a release build: its commit came with its version
		}
	}
	line := "agentbus " + v
	switch {
	case c != "" && modified:
		line += " (commit " + c + ", modified)"
	case c != "":
		line += " (commit " + c + ")"
	}
	return line
}
