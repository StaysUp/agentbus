package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/wall"
)

// One srt version everywhere: the one the runner's probe accepts
// (wall.Version), the one install.sh says it installs, and the one
// packaging/srt pins, with every package locked.
func TestWallPinAgrees(t *testing.T) {
	sh, err := os.ReadFile(filepath.Join(root(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sh), "\nsrt_version="+wall.Version+"\n") {
		t.Fatalf("install.sh does not set srt_version=%s", wall.Version)
	}
	var pkg struct {
		Engines      map[string]string `json:"engines"`
		Dependencies map[string]string `json:"dependencies"`
	}
	b, err := os.ReadFile(filepath.Join(root(t), "packaging", "srt", "package.json"))
	if err != nil || json.Unmarshal(b, &pkg) != nil {
		t.Fatalf("packaging/srt/package.json: %v", err)
	}
	if len(pkg.Dependencies) != 1 || pkg.Dependencies[wall.Package] != wall.Version || pkg.Engines["node"] != ">="+wall.Node {
		t.Fatalf("packaging/srt/package.json pins %v, node %q; want only %s %s, node >=%s", pkg.Dependencies, pkg.Engines["node"], wall.Package, wall.Version, wall.Node)
	}
	var lock struct {
		Packages map[string]struct {
			Version   string `json:"version"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	b, err = os.ReadFile(filepath.Join(root(t), "packaging", "srt", "package-lock.json"))
	if err != nil || json.Unmarshal(b, &lock) != nil {
		t.Fatalf("packaging/srt/package-lock.json: %v", err)
	}
	if got := lock.Packages["node_modules/"+wall.Package].Version; got != wall.Version {
		t.Fatalf("the lockfile holds %s %q, want %s", wall.Package, got, wall.Version)
	}
	for name, p := range lock.Packages {
		if name != "" && !strings.HasPrefix(p.Integrity, "sha512-") {
			t.Fatalf("the lockfile's %s has no sha512 integrity", name)
		}
	}
}
