//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the fake agent. With AGENTBUS_FAKE_AGENT set it
// prints the base URL it sees and its arguments, marks that it ran, waits for
// AGENTBUS_FAKE_WAIT to exist if that is set, and exits with
// AGENTBUS_FAKE_EXIT. It prints AGENTBUS_FAKE_STDOUT to stdout, as an agent
// prints its result, and AGENTBUS_FAKE_STDERR to stderr. Asked --version it prints AGENTBUS_FAKE_VERSION; asked
// `auth status` (claude) or `login status` (codex) it exits with
// AGENTBUS_FAKE_AUTH_EXIT or AGENTBUS_FAKE_LOGIN_EXIT, 1 when unset.
func TestMain(m *testing.M) {
	if os.Getenv("AGENTBUS_FAKE_AGENT") == "1" {
		fakeAgent()
	}
	os.Exit(m.Run())
}

func fakeAgent() {
	if a := os.Args[1:]; len(a) == 1 && a[0] == "--version" {
		fmt.Println(os.Getenv("AGENTBUS_FAKE_VERSION"))
		os.Exit(0)
	} else if len(a) == 2 && a[1] == "status" && (a[0] == "auth" || a[0] == "login") {
		code, err := strconv.Atoi(os.Getenv("AGENTBUS_FAKE_" + strings.ToUpper(a[0]) + "_EXIT"))
		if err != nil {
			code = 1
		}
		fmt.Println(`{"email":"someone@example.com"}`) // must never reach a reply
		os.Exit(code)
	}
	// First, so a test that sees the report knows SIGTERM is already ignored.
	if os.Getenv("AGENTBUS_FAKE_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	if p := os.Getenv("AGENTBUS_FAKE_MARK"); p != "" {
		os.WriteFile(p, []byte("ran"), 0o600)
	}
	fmt.Printf("url=%s\n", os.Getenv("ANTHROPIC_BASE_URL"))
	fmt.Printf("args=%s\n", strings.Join(os.Args[1:], " "))
	fmt.Print(os.Getenv("AGENTBUS_FAKE_STDOUT"))
	// With AGENTBUS_FAKE_CALL=1, one model call through the base URL, as
	// Claude Code makes it. With 2, a second call whose conversation differs
	// from the first, as one after a compaction does.
	calls := []string{`{"model":"claude-x","messages":[{"role":"user","content":"fake turn"}]}`,
		`{"model":"claude-x","messages":[{"role":"user","content":"summary of the turn so far"}]}`}
	n, _ := strconv.Atoi(os.Getenv("AGENTBUS_FAKE_CALL"))
	calls = calls[:min(n, len(calls))]
	// With AGENTBUS_FAKE_BODY, that one model call instead.
	if b := os.Getenv("AGENTBUS_FAKE_BODY"); b != "" {
		calls = []string{b}
	}
	for _, body := range calls {
		resp, err := http.Post(os.Getenv("ANTHROPIC_BASE_URL")+"/v1/messages", "application/json",
			strings.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	// With AGENTBUS_FAKE_CODEX_BODY, one model call as codex makes it: to the
	// openai_base_url its -c argument names, with AGENTBUS_FAKE_THREAD as its
	// Thread-Id header.
	if b := os.Getenv("AGENTBUS_FAKE_CODEX_BODY"); b != "" {
		for _, a := range os.Args[1:] {
			if base, ok := strings.CutPrefix(a, `openai_base_url="`); ok {
				req, _ := http.NewRequest("POST", strings.TrimSuffix(base, `"`)+"/responses", strings.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Thread-Id", os.Getenv("AGENTBUS_FAKE_THREAD"))
				if resp, err := http.DefaultClient.Do(req); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}
	}
	// AGENTBUS_FAKE_STDERR goes to stderr, AGENTBUS_FAKE_STDERR_REPEAT times
	// (once when unset), as an agent reports a failure.
	if e := os.Getenv("AGENTBUS_FAKE_STDERR"); e != "" {
		n, err := strconv.Atoi(os.Getenv("AGENTBUS_FAKE_STDERR_REPEAT"))
		if err != nil {
			n = 1
		}
		fmt.Fprint(os.Stderr, strings.Repeat(e, n))
	}
	// With AGENTBUS_FAKE_TOOL, a tool that outlives its agent: `sleep 30`,
	// in a session and process group of its own when AGENTBUS_FAKE_TOOL_SETSID
	// is 1 (codex-cli 0.164.0 gives each tool its own group), else in a group
	// of its own only. "<agent pid> <tool pid>" goes to the file named.
	if p := os.Getenv("AGENTBUS_FAKE_TOOL"); p != "" {
		tool := exec.Command("sleep", "30")
		if os.Getenv("AGENTBUS_FAKE_TOOL_SETSID") == "1" {
			tool.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		} else {
			tool.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		if err := tool.Start(); err == nil {
			os.WriteFile(p+".tmp", []byte(fmt.Sprintf("%d %d", os.Getpid(), tool.Process.Pid)), 0o600)
			os.Rename(p+".tmp", p)
		}
	}
	// A run keeps the child's output out of every reply, so it reports through a file: its
	// arguments, environment and folder.
	if p := os.Getenv("AGENTBUS_FAKE_OUT"); p != "" {
		cwd, _ := os.Getwd()
		b, _ := json.Marshal(fakeReport{Args: os.Args[1:], Env: os.Environ(), Cwd: cwd})
		os.WriteFile(p+".tmp", b, 0o600)
		os.Rename(p+".tmp", p) // whole or absent, never half written
	}
	if p := os.Getenv("AGENTBUS_FAKE_WAIT"); p != "" {
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(p); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	code, _ := strconv.Atoi(os.Getenv("AGENTBUS_FAKE_EXIT"))
	os.Exit(code)
}

type fakeReport struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
}
