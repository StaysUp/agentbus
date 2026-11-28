//go:build integration

package wall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeSrt writes an srt stand-in: it answers --version with version and runs
// the -c command with /bin/sh. holds makes it refuse writes outside the
// folder the probe allows, by making the probe's out folder read-only first,
// as a wall would; without it the stand-in walls nothing.
func fakeSrt(t *testing.T, version string, holds bool) func(string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	hold := ""
	if holds {
		hold = `chmod 0500 out; trap 'chmod 0700 out' EXIT; `
	}
	script := "#!/bin/sh\n" +
		`if [ "$1" = --version ]; then echo ` + version + "; exit 0; fi\n" +
		`while [ "$1" != -c ]; do shift; done` + "\n" +
		hold + `/bin/sh -c "$2"` + "\n"
	bin := filepath.Join(dir, "srt")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return func(name string) (string, error) {
		if name != Engine {
			return "", errors.New("not found")
		}
		return bin, nil
	}
}

func TestProbe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only folder, so the stand-in cannot hold a write")
	}
	ctx, env := context.Background(), os.Environ()
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	for _, tc := range []struct {
		name string
		look func(string) (string, error)
		want Result
	}{
		{"not installed", missing, Result{Problem: NotInstalled}},
		{"holds", fakeSrt(t, Version, true), Result{Version: Version}},
		// Only the binary: a probe that ran srt and read its version alone
		// would pass this one.
		{"walls nothing", fakeSrt(t, Version, false), Result{Version: Version, Problem: Leaky}},
		{"another version", fakeSrt(t, "0.0.80", true), Result{Version: "0.0.80", Problem: "srt 0.0.80 is installed; AgentBus needs " + Version + ": " + install}},
		{"no version", fakeSrt(t, "oops", true), Result{Problem: noRun}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Probe(ctx, tc.look, env)
			if got.Version != tc.want.Version || got.Problem != tc.want.Problem || len(got.Levels) != 0 {
				t.Fatalf("Probe: %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The real srt, where it is installed: the probe passes on a machine srt
// can wall. Set AGENTBUS_WALL_REQUIRED=1 to fail when it is missing.
func TestProbeRealSrt(t *testing.T) {
	bin, err := exec.LookPath(Engine)
	if err != nil {
		if os.Getenv("AGENTBUS_WALL_REQUIRED") != "" {
			t.Fatal("srt is not on PATH")
		}
		t.Skip("srt is not on PATH")
	}
	got := Probe(context.Background(), func(string) (string, error) { return bin, nil }, os.Environ())
	if got.Problem != "" {
		if os.Getenv("AGENTBUS_WALL_REQUIRED") == "" {
			t.Skipf("srt on %s: %s (a sandboxed shell refuses a nested one)", runtime.GOOS, got.Problem)
		}
		t.Fatalf("srt on %s: %+v", runtime.GOOS, got)
	}
}
