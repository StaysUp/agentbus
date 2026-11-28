//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// install.sh installs srt only on Node.js 22.12 or later.
func TestInstallNodeOK(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "node_ok() {")
	j := strings.Index(s[i:], "\n}\n")
	if i < 0 || j < 0 {
		t.Fatal("install.sh has no node_ok")
	}
	fn := s[i : i+j+3]
	for v, want := range map[string]bool{"v22.12.0": true, "v22.13.0": true, "v24.1.0": true, "v23.0.0": true,
		"v22.11.9": false, "v20.19.1": false, "v8.17.0": false, "": false, "vx.y": false} {
		err := exec.Command("sh", "-c", fn+`node_ok "$1"`, "sh", v).Run()
		if (err == nil) != want {
			t.Errorf("node_ok %q: %v, want ok=%v", v, err, want)
		}
	}
}

// install.sh links srt by an absolute target, so the link holds when
// AGENTBUS_INSTALL_DIR is relative. node, npm and curl are stand-ins: npm ci
// writes an srt that prints the pinned version.
func TestInstallSrtRelativeDir(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	fn := func(name string) string {
		i := strings.Index(s, name+"() {")
		j := strings.Index(s[i:], "\n}\n")
		if i < 0 || j < 0 {
			t.Fatalf("install.sh has no %s", name)
		}
		return s[i : i+j+3]
	}
	fake, work := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{
		"node": "echo v22.13.0",
		"curl": `cp "` + filepath.Join(root(t), "packaging", "srt") + `/$(basename "$4")" "$3"`,
		"npm": `while [ "$1" != --prefix ]; do shift; done; mkdir -p "$2/node_modules/.bin"
printf '#!/bin/sh\necho 0.0.83\n' > "$2/node_modules/.bin/srt"; chmod 755 "$2/node_modules/.bin/srt"`,
	} {
		if err := os.WriteFile(filepath.Join(fake, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(work, "local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -eu\nrepo=staysup/agentbus\nsrt_ref=v0\nsrt_version=0.0.83\ntmp=$(mktemp -d)\n" +
		fn("say") + fn("node_ok") + fn("install_srt") + `install_srt local/bin`
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir, cmd.Env = work, append(os.Environ(), "PATH="+fake+":/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	link := filepath.Join(work, "local", "bin", "srt")
	target, err := os.Readlink(link)
	if err != nil || !filepath.IsAbs(target) {
		t.Fatalf("srt link %q %v: %s", target, err, out)
	}
	if got, err := exec.Command(link).Output(); err != nil || strings.TrimSpace(string(got)) != "0.0.83" {
		t.Fatalf("the linked srt: %q %v", got, err)
	}
}
