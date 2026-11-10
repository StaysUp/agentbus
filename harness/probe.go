package harness

// The probe: which harnesses a machine has, their versions, and whether each
// is signed in. A runner runs it on its own machine and reports the answer
// to its control plane (queue.Capabilities), which GET /api/v0/harnesses
// shows; control never runs an agent's program itself.

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ProbeTimeout bounds each --version or status command.
const ProbeTimeout = 5 * time.Second

// ProbeEvery is how often a runner probes again.
const ProbeEvery = time.Minute

// Report is what one machine says of one harness.
type Report struct {
	Name     string `json:"name"`
	Found    bool   `json:"found"`
	Version  string `json:"version,omitempty"`
	SignedIn *bool  `json:"signed_in,omitempty"`
}

// ProbeAll probes every runnable harness, in registry order. lookPath finds a
// program on PATH; nil reports every harness as not found. environ is the
// environment the commands run with; nil is the process's own.
func ProbeAll(ctx context.Context, lookPath func(string) (string, error), environ func() []string) []Report {
	out := make([]Report, 0, len(registry))
	for _, h := range registry {
		out = append(out, Probe(ctx, h.Info(), lookPath, environ))
	}
	return out
}

// Probe finds one harness's program and asks it its version and sign-in
// status: its own offline status command, whose exit code is the answer, 0
// signed in and 1 not. That command's output is discarded, since it can name
// the account. It never returns the path.
func Probe(ctx context.Context, info Info, lookPath func(string) (string, error), environ func() []string) Report {
	rep := Report{Name: info.Name}
	if lookPath == nil {
		return rep
	}
	bin, err := lookPath(info.Name)
	if err != nil {
		return rep
	}
	rep.Found = true
	out, code := runProbe(ctx, environ, bin, "--version")
	if code == 0 {
		rep.Version = VersionToken(out)
	}
	if args := info.StatusArgs; len(args) > 0 {
		switch _, code := runProbe(ctx, environ, bin, args...); code {
		case 0:
			t := true
			rep.SignedIn = &t
		case 1:
			f := false
			rep.SignedIn = &f
		}
	}
	return rep
}

// runProbe runs bin with args, an empty stdin and environ, within
// ProbeTimeout. It returns at most 4 KiB of stdout and the exit code, or -1
// when the command did not end on its own.
func runProbe(ctx context.Context, environ func() []string, bin string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if environ != nil {
		cmd.Env = environ()
	}
	out := &capped{max: 4 << 10}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", -1
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ee) && ee.Exited():
		return out.String(), ee.ExitCode()
	}
	return "", -1
}

var versionShape = regexp.MustCompile(`^v?([0-9]+\.[0-9]+(\.[0-9]+)?([-+][0-9A-Za-z.-]+)?)$`)

// VersionToken is the first word of out shaped like a version: "2.1.318" from
// "2.1.318 (Claude Code)", "0.164.0" from "codex-cli 0.164.0".
func VersionToken(out string) string {
	for _, f := range strings.Fields(out) {
		if m := versionShape.FindStringSubmatch(f); m != nil {
			return m[1]
		}
	}
	return ""
}

// capped keeps the first max bytes written to it.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
}
