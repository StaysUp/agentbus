//go:build integration

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The command itself, as a person runs it: on Linux it starts itself again
// in a user namespace, as up does, and says nothing of that on stderr. It
// answers within a second, fails on the variable that would make claude's
// turns refuse, and leaves nothing beside the data folder it did not make.
func TestDoctorProcess(t *testing.T) {
	bin := agentbusBinary(t)
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	home := filepath.Join(parent, "agentbus")
	cmd := exec.Command(bin, "doctor")
	cmd.Env = []string{"PATH=" + path, "HOME=" + t.TempDir(), "AGENTBUS_HOME=" + home, "ANTHROPIC_BASE_URL=http://127.0.0.1:1"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("%v, stderr %q:\n%s", err, errb.String(), out.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr: %q", errb.String())
	}
	for _, want := range []string{
		"ok    claude is on PATH: " + filepath.Join(path, "claude"),
		"fail  ANTHROPIC_BASE_URL is set, so every claude turn is refused",
		"Copy ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
	if took > time.Second {
		t.Errorf("doctor took %v", took)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Fatalf("doctor left %v", entries)
	}
}
