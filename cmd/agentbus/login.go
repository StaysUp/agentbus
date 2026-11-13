package main

// agentbus login: sign this machine in to a control. The machine asks control
// (POST /api/v0/runner/login), shows the code control gave, and waits while
// the person types that code on the cockpit's Runners page. Once approved it
// saves its runner identity in its data folder (runner.json) and prints the one command that starts the runner. It sets up no
// background service: the runner runs where that command is run. It takes
// agentbus runner's flags, which that command carries.
//
// It is the only way a machine joins a control: control writes no
// enrollment token since 0.5.1. A later build may take no address to mean
// lorebug.art; this one needs one.
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
	"syscall"
	"time"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/runner"
	"github.com/staysup/agentbus/sandbox"
)

const loginUsage = "usage: agentbus login URL [agentbus runner's flags: -name NAME -folder DIR -labels a,b -capacity 4 -gateway server|local]\n" +
	"  URL is control's address, such as http://192.168.1.10:8787"

// loginDeps is what agentbus login reaches outside itself, so a test can
// stand in for it.
type loginDeps struct {
	http *http.Client
	// self is this program's path, which the printed command runs.
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
		http: &http.Client{Timeout: 15 * time.Second},
		self: self,
		caps: func() *queue.Capabilities {
			can, why := sandbox.Can()
			return &queue.Capabilities{OS: runtime.GOOS, Arch: runtime.GOARCH, Sandbox: queue.NewSandboxReport(can, why)}
		},
		signals: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt)
		},
	}
}

func runLogin(args []string, home string, stdout, stderr io.Writer, d loginDeps) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "agentbus login needs control's address, such as agentbus login http://192.168.1.10:8787")
		fmt.Fprintln(stderr, loginUsage)
		return 2
	}
	target := strings.TrimRight(args[0], "/")
	// The runner may start in another folder: the command needs the data
	// folder whole.
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	var flags []string
	for _, a := range args[1:] {
		if a == "-server" || strings.HasPrefix(a, "-server=") || strings.HasPrefix(a, "--server") {
			fmt.Fprintln(stderr, "agentbus login: control's address is its first argument, not -server")
			return 2
		}
		if a == "-enroll-token-file" || strings.HasPrefix(a, "-enroll-token-file=") || strings.HasPrefix(a, "--enroll-token-file") {
			fmt.Fprintln(stderr, "agentbus login: -enroll-token-file was removed in 0.5.1; agentbus login needs none")
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
	if err := os.MkdirAll(home, 0o700); err != nil {
		fmt.Fprintf(stderr, "agentbus login: the data folder %s could not be made\n", home)
		return 1
	}
	// One login at a time on a data folder, for its whole run. An identity
	// already here is never read for its token, sent or asked about: this
	// is always a new sign-in, and once approved the old file is kept
	// beside the new one.
	unlock, err := lockLogin(home)
	if err != nil {
		fmt.Fprintf(stderr, "agentbus login: %v\n", err)
		return 1
	}
	defer unlock()
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
	oldID, kept := "", ""
	if _, err := os.Lstat(statePath); err == nil {
		oldID = runner.SavedID(statePath)
		kept = statePath + ".replaced-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := os.Rename(statePath, kept); err != nil {
			fmt.Fprintf(stderr, "agentbus login: approved, but the sign-in already in %s could not be kept aside; nothing was changed. "+
				"Runner %s, which this one was approved as, will take no work: remove it on the Runners page\n", statePath, st.Runner.ID)
			return 1
		}
	}
	if err := runner.SaveIdentity(statePath, *st.Runner, started.Token, st.Heartbeat, st.Lease); err != nil {
		fmt.Fprintf(stderr, "agentbus login: approved, but %s could not be written. "+
			"Runner %s, which this one was approved as, will take no work: remove it on the Runners page\n", statePath, st.Runner.ID)
		return 1
	}
	fmt.Fprintf(stdout, "Approved: this machine is runner %s of %s. Its credential is in %s (mode 0600).\n", st.Runner.ID, target, statePath)
	if kept != "" {
		fmt.Fprintf(stdout, "The sign-in that was here is kept as %s.\n", kept)
		if oldID != "" {
			fmt.Fprintf(stdout, "The old runner %s may still be listed at control; remove it on the Runners page if so.\n", oldID)
		}
	}

	command := append([]string{d.self}, runArgs...)
	fmt.Fprintf(stdout, "Start the runner with: AGENTBUS_HOME=%s %s\n", shellQuote(home), shellLine(command))
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

// lockLogin holds the data folder's login lock until the answer is called.
// A second login there at once is refused.
func lockLogin(home string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(home, "login.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the lock file in %s could not be opened", home)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another login is running here (%s)", home)
		}
		return nil, fmt.Errorf("the data folder %s could not be locked", home)
	}
	return func() { f.Close() }, nil
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
