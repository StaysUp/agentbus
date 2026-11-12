package main

// agentbus login: sign this machine in to a control. The machine asks control
// (POST /api/v0/runner/login), shows the code control gave, and waits while
// the person types that code on the cockpit's Runners page. Once approved it
// saves its runner identity in its data folder (runner.json), as an enroll
// does, and sets the runner up as a service where it can: a systemd user
// unit on Linux, a launchd agent on macOS. Where it cannot, it prints the
// one command to run. It takes agentbus runner's flags, which the service
// then runs with.
//
// It replaces copying control's enroll_token by hand, which still works for
// the setups made with it (agentbus runner -enroll-token-file). A later
// build may take no address to mean lorebug.art; this one needs one.
//
// Not the cockpit's Connect: that keeps a subscription's sign-in on control.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/runner"
	"github.com/staysup/agentbus/sandbox"
)

const loginUsage = "usage: agentbus login URL [-no-service] [agentbus runner's flags: -name NAME -folder DIR -labels a,b -capacity 4 -gateway server|local]\n" +
	"  URL is control's address, such as http://192.168.1.10:8787"

// loginDeps is what agentbus login reaches outside itself, so a test can
// stand in for it.
type loginDeps struct {
	http    *http.Client
	service serviceSetup
	// self is this program's path, which the service runs.
	self string
	// caps is what the machine reports when it asks: its OS and whether it
	// can build a session's wall, which the Runners page shows.
	caps func() *queue.Capabilities
	// poll, when not zero, is how often to ask, in place of control's.
	poll time.Duration
	// signals ends the wait on Ctrl-C.
	signals func(context.Context) (context.Context, context.CancelFunc)
}

func systemLogin() loginDeps {
	self, _ := os.Executable()
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}
	return loginDeps{
		http:    &http.Client{Timeout: 15 * time.Second},
		service: systemService(),
		self:    self,
		caps: func() *queue.Capabilities {
			can, why := sandbox.Can()
			return &queue.Capabilities{OS: runtime.GOOS, Arch: runtime.GOARCH, Sandbox: &queue.SandboxReport{Can: can, Why: why}}
		},
		signals: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt)
		},
	}
}

func runLogin(args []string, home string, getenv func(string) string, stdout, stderr io.Writer, d loginDeps) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "agentbus login needs control's address, such as agentbus login http://192.168.1.10:8787")
		fmt.Fprintln(stderr, loginUsage)
		return 2
	}
	target := strings.TrimRight(args[0], "/")
	// The service starts in another folder: it needs the data folder whole.
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	var flags []string
	noService := false
	for _, a := range args[1:] {
		switch a {
		case "-no-service", "--no-service":
			noService = true
			continue
		}
		if a == "-server" || strings.HasPrefix(a, "-server=") || strings.HasPrefix(a, "--server") {
			fmt.Fprintln(stderr, "agentbus login: control's address is its first argument, not -server")
			return 2
		}
		if a == "-enroll-token-file" || strings.HasPrefix(a, "-enroll-token-file=") || strings.HasPrefix(a, "--enroll-token-file") {
			fmt.Fprintln(stderr, "agentbus login: -enroll-token-file is agentbus runner's old way to join; agentbus login needs none")
			return 2
		}
		flags = append(flags, a)
	}
	// The runner's own flags, checked as agentbus runner checks them.
	runArgs := append([]string{"runner", "-server", target}, flags...)
	c, ok := parse(runArgs, home, stderr)
	if !ok {
		return 2
	}
	if _, err := os.Stat(filepath.Join(home, "jobs.db")); err == nil {
		fmt.Fprintf(stderr, "agentbus login: %s is a control's data folder (it holds jobs.db); a runner needs one of its own: set AGENTBUS_HOME to a new folder\n", home)
		return 1
	}
	if signedIn(home) {
		fmt.Fprintf(stderr, "agentbus login: this data folder already holds a runner's sign-in (%s); its runner keeps working. "+
			"To sign this machine in again, move that file away first, or set AGENTBUS_HOME to a new folder\n", filepath.Join(home, runnerState))
		return 1
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		fmt.Fprintf(stderr, "agentbus login: the data folder %s could not be made\n", home)
		return 1
	}
	name := c.runnerName
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = "runner"
	}
	labels := c.labels
	if labels == nil {
		labels = []string{"agentbus"}
	}
	if u, _ := url.Parse(target); u != nil && u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		fmt.Fprintln(stdout, "Note: "+target+" is plain http: the runner's token crosses the network unencrypted. Use it only on a network you trust.")
	}

	ctx, cancel := d.signals(context.Background())
	defer cancel()
	var started jobs.LoginStarted
	status, err := loginCall(ctx, d.http, "POST", target+"/api/v0/runner/login", "",
		jobs.EnrollRequest{Name: name, Labels: labels, Capacity: c.maxRuns, Protocol: jobs.ProtocolVersion, Capabilities: d.caps()}, &started)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "agentbus login: %s could not be reached\n", target)
		return 1
	case status == http.StatusTooManyRequests:
		fmt.Fprintln(stderr, "agentbus login: too many machines wait for approval on that control; approve or refuse them on its Runners page, then try again")
		return 1
	case status == http.StatusNotFound:
		fmt.Fprintln(stderr, "agentbus login: that address is not a control of this release, or it keeps no job queue; run the same agentbus release on both")
		return 1
	case status != http.StatusCreated:
		fmt.Fprintf(stderr, "agentbus login: control refused the sign-in (HTTP %d)\n", status)
		return 1
	}
	fmt.Fprintf(stdout, "\nTo let this machine (%s) work for %s, open its cockpit's Runners page and approve it with this code:\n\n    %s\n\n", name, target, started.Code)
	fmt.Fprintf(stdout, "Waiting for approval, up to %s. Ctrl-C stops waiting.\n", started.Expires.Sub(time.Now()).Round(time.Minute))

	poll := time.Duration(started.PollMS) * time.Millisecond
	if poll <= 0 {
		poll = jobs.LoginPoll
	}
	if d.poll > 0 {
		poll = d.poll
	}
	var st jobs.LoginState
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "agentbus login: stopped waiting; the machine is not signed in. Run agentbus login again.")
			return 1
		case <-time.After(poll):
		}
		status, err := loginCall(ctx, d.http, "GET", target+"/api/v0/runner/login", started.Token, nil, &st)
		if err != nil || status != http.StatusOK {
			// Control may be restarting: keep asking until the sign-in expires.
			if time.Now().After(started.Expires) {
				fmt.Fprintln(stderr, "agentbus login: control did not answer before the sign-in expired. Run agentbus login again.")
				return 1
			}
			continue
		}
		if st.State != queue.LoginPending {
			break
		}
	}
	switch st.State {
	case queue.LoginApproved:
	case queue.LoginRefused:
		fmt.Fprintln(stderr, "agentbus login: the sign-in was refused on the Runners page. Nothing was saved.")
		return 1
	case queue.LoginExpired:
		fmt.Fprintln(stderr, "agentbus login: nobody approved the sign-in within 15 minutes. Run agentbus login again.")
		return 1
	default:
		fmt.Fprintf(stderr, "agentbus login: the sign-in ended %s. Run agentbus login again.\n", st.State)
		return 1
	}
	if st.Runner == nil {
		fmt.Fprintln(stderr, "agentbus login: control approved the sign-in but sent no runner. Run agentbus login again.")
		return 1
	}
	statePath := filepath.Join(home, runnerState)
	if err := runner.SaveIdentity(statePath, *st.Runner, started.Token, st.Heartbeat, st.Lease); err != nil {
		if errors.Is(err, runner.ErrIdentityThere) {
			fmt.Fprintf(stderr, "agentbus login: another sign-in saved %s first, and it was kept. "+
				"Runner %s, which this one was approved as, will take no work: remove it on the Runners page\n", statePath, st.Runner.ID)
			return 1
		}
		fmt.Fprintf(stderr, "agentbus login: approved, but %s could not be written\n", statePath)
		return 1
	}
	fmt.Fprintf(stdout, "Approved: this machine is runner %s of %s. Its credential is in %s (mode 0600).\n", st.Runner.ID, target, statePath)

	command := append([]string{d.self}, runArgs...)
	line := shellLine(command)
	envHome := "AGENTBUS_HOME=" + shellQuote(home)
	if noService {
		fmt.Fprintf(stdout, "Start the runner with:\n\n    %s %s\n", envHome, line)
		return 0
	}
	did, err := d.service.install(command, home, getenv("PATH"))
	if err != nil {
		fmt.Fprintf(stdout, "The runner was not set up as a service: %v.\nStart it with:\n\n    %s %s\n", err, envHome, line)
		return 0
	}
	fmt.Fprint(stdout, did)
	return 0
}

// loginCall sends one request of agentbus login, with a token when tok is
// not empty, and reads a JSON answer into out on a 2xx.
func loginCall(ctx context.Context, c *http.Client, method, u, tok string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, errors.New("bad address")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, errors.New("no answer")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 == 2 && out != nil {
		if json.Unmarshal(b, out) != nil {
			return 0, errors.New("not JSON")
		}
	}
	return resp.StatusCode, nil
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// shellLine is args as one line a POSIX shell runs as they are.
func shellLine(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

func shellQuote(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`!*?[]{}()<>|&;#~=%") {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}
