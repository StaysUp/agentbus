package main

// agentbus api, also run as agentbus-api: the API client a turn's agent uses.
// It reads the service's address from AGENTBUS_URL and the turn's token from
// AGENTBUS_TOKEN, so the command an agent runs holds no variable and no token: Claude Code refuses a
// command with a variable in it unless the person approves it (Claude Code
// 2.1.319, 2026-11-09), and a turn has no person to ask. What a call may do is
// the token's to say (api/tokens.go). It sends the token to AGENTBUS_URL only.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/staysup/agentbus/api"
	engine "github.com/staysup/agentbus/run"
)

const apiUsage = `usage: agentbus-api METHOD PATH [BODY]   (or agentbus api METHOD PATH [BODY])
  Calls this AgentBus service from inside a turn, with $AGENTBUS_URL and $AGENTBUS_TOKEN.
  PATH starts with /api/ or /docs/. BODY is JSON; "-" reads it from stdin.
  Prints the reply; exits 1 when its status is 400 or more.`

// apiTimeout bounds one call. Starting a turn may wait 30 seconds for a runner.
const apiTimeout = 60 * time.Second

// self is this program's path, for agentbus-api to link to. "" when it
// cannot be found, and turns are then told to use curl.
func self() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// apiLink makes <home>/bin/agentbus-api a link to self, replacing an older
// one in one rename, and answers the folder for run.Config.APIDir. "" when
// self is "" or the link cannot be made: turns are then told to use curl.
func apiLink(home, self string, l interface{ Printf(string, ...any) }) string {
	if self == "" {
		return ""
	}
	dir := filepath.Join(home, "bin")
	link := filepath.Join(dir, engine.APIClient)
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.Printf("api_client.link_failed step=mkdir")
		return ""
	}
	if err := os.Symlink(self, tmp); err != nil {
		l.Printf("api_client.link_failed step=symlink")
		return ""
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		l.Printf("api_client.link_failed step=rename")
		return ""
	}
	return dir
}

func apiCall(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 2 || len(args) > 3 {
		fmt.Fprintln(stderr, apiUsage)
		return 2
	}
	method, path := strings.ToUpper(args[0]), args[1]
	// The methods are the route table's, so a route never takes a method
	// this client cannot send.
	if methods := api.Methods(); !slices.Contains(methods, method) {
		fmt.Fprintln(stderr, "agentbus api: METHOD must be one of "+strings.Join(methods, ", "))
		return 2
	}
	if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/docs/") {
		fmt.Fprintln(stderr, "agentbus api: PATH must start with /api/ or /docs/")
		return 2
	}
	base, tok := getenv("AGENTBUS_URL"), getenv("AGENTBUS_TOKEN")
	u, err := url.Parse(base)
	if base == "" || tok == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintln(stderr, "agentbus api: AGENTBUS_URL and AGENTBUS_TOKEN must be set, as they are inside a turn AgentBus started")
		return 2
	}
	var body io.Reader
	if len(args) == 3 {
		b := []byte(args[2])
		if args[2] == "-" {
			if b, err = io.ReadAll(io.LimitReader(stdin, 4<<20)); err != nil {
				fmt.Fprintln(stderr, "agentbus api: the body could not be read from stdin")
				return 1
			}
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		fmt.Fprintln(stderr, "agentbus api: the request could not be made")
		return 2
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: apiTimeout}).Do(req)
	if err != nil {
		// Fixed wording: Go's error can quote the URL.
		fmt.Fprintln(stderr, "agentbus api: the service did not answer")
		return 1
	}
	defer resp.Body.Close()
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		return 1
	}
	if resp.StatusCode >= 400 {
		fmt.Fprintf(stderr, "\nagentbus api: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
