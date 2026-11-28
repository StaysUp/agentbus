// Package wall is the one wall AgentBus puts around a turn's processes, built
// with sandbox-runtime (srt). See plans/DESIGN_one_wall.md in the notes.
//
// Step 1: a runner probes that srt holds a wall on its machine and reports
// it. No turn runs inside the wall yet, so this build knows no level
// (Levels is empty) and a runner refuses every walled job.
//
// It imports nothing of the module, and only the runner side uses it.
package wall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// Engine names what builds the wall.
	Engine = "srt"
	// Package and Version are the srt that install.sh installs, and the only
	// one a probe accepts: a wall's settings are read by that version's rules.
	Package = "@anthropic-ai/sandbox-runtime"
	Version = "0.0.83"
	// Node is the oldest Node.js srt runs on (its package's engines field).
	Node = "22.12.0"
)

// Levels are the walls this build can put around a turn. None yet: the copy
// wall is step 2.
var Levels []string

// Builds says this build can put a wall of the level around a turn.
func Builds(level string) bool {
	for _, l := range Levels {
		if l == level {
			return true
		}
	}
	return false
}

// Result is one probe: the srt version found, and the levels the runner can
// build now, or the problem that leaves it none. Problem is fixed wording.
type Result struct {
	Version string
	Levels  []string
	Problem string
}

// The problems a probe reports.
const (
	install       = "install it with install.sh, or: npm install -g " + Package + "@" + Version + " (Node.js " + Node + " or later)"
	NotInstalled  = "srt is not installed on this machine: " + install
	noRun         = "srt did not run: it needs Node.js " + Node + " or later on PATH; " + install
	noStartDarwin = "srt could not start a wall on this machine"
	noStartLinux  = "srt could not start a wall on this machine: it needs bwrap, socat and ripgrep, and user namespaces (install.sh installs them)"
	Leaky         = "srt ran, but a write outside its wall was not refused"
)

// probeTime bounds each srt the probe starts.
const probeTime = 30 * time.Second

var versionShape = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]{1,32})?$`)

// Probe checks that srt holds a wall here, not only that it is installed: it
// runs a shell under srt whose settings allow writes to one folder only, and
// the shell writes there and next to it. The probe passes when the first
// write is made and the second refused. look finds srt on PATH; env is the
// environment srt runs with (its PATH finds node).
func Probe(ctx context.Context, look func(string) (string, error), env []string) Result {
	bin, err := look(Engine)
	if err != nil {
		return Result{Problem: NotInstalled}
	}
	dir, err := os.MkdirTemp("", "agentbus-wall-")
	if err != nil {
		return Result{Problem: noStart()}
	}
	defer os.RemoveAll(dir)
	// srt reads paths as the kernel sees them: on macOS the temp folder is
	// under /var, a link to /private/var.
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d
	}
	out, ok := run(ctx, bin, dir, env, "--version")
	if !ok {
		return Result{Problem: noRun}
	}
	v := strings.TrimSpace(out)
	if !versionShape.MatchString(v) {
		return Result{Problem: noRun}
	}
	if v != Version {
		return Result{Version: v, Problem: "srt " + v + " is installed; AgentBus needs " + Version + ": " + install}
	}
	in, outside := filepath.Join(dir, "in"), filepath.Join(dir, "out")
	for _, d := range []string{in, outside} {
		if os.Mkdir(d, 0o700) != nil {
			return Result{Version: v, Problem: noStart()}
		}
	}
	settings, err := json.Marshal(probeSettings(in))
	if err != nil {
		return Result{Version: v, Problem: noStart()}
	}
	file := filepath.Join(dir, "settings.json")
	if os.WriteFile(file, settings, 0o600) != nil {
		return Result{Version: v, Problem: noStart()}
	}
	run(ctx, bin, dir, env, "--settings", file, "-c", ": > in/held; : > out/leaked")
	if _, err := os.Stat(filepath.Join(in, "held")); err != nil {
		return Result{Version: v, Problem: noStart()}
	}
	if _, err := os.Stat(filepath.Join(outside, "leaked")); !errors.Is(err, os.ErrNotExist) {
		return Result{Version: v, Problem: Leaky}
	}
	return Result{Version: v, Levels: Levels}
}

// probeSettings are the narrowest settings: writes to in only, no network.
// srt 0.0.83's settings file must name both domain lists.
func probeSettings(in string) map[string]any {
	return map[string]any{
		"network":    map[string]any{"allowedDomains": []string{}, "deniedDomains": []string{}},
		"filesystem": map[string]any{"denyRead": []string{}, "allowWrite": []string{in}, "denyWrite": []string{}},
	}
}

// run starts srt with args in dir and answers its output and whether it
// exited 0 within probeTime.
func run(ctx context.Context, bin, dir string, env []string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTime)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, env
	var out bytes.Buffer
	cmd.Stdout = &limited{b: &out, n: 4096}
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return out.String(), err == nil
}

func noStart() string {
	if runtime.GOOS == "linux" {
		return noStartLinux
	}
	return noStartDarwin
}

// limited keeps the first n bytes written to it.
type limited struct {
	b *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
