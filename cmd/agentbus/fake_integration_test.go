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
	"path/filepath"
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
	if d := os.Getenv(launchDirEnv); d != "" {
		fakeLaunch(d)
	}
	// With AGENTBUS_FAKE_APP_SERVER set, asked app-server it is codex's
	// model server (models_integration_test.go).
	if a := os.Args[1:]; os.Getenv("AGENTBUS_FAKE_APP_SERVER") == "1" && len(a) > 0 && a[len(a)-1] == "app-server" {
		fakeAppServer()
	}
	// With AGENTBUS_FAKE_AGY_MODELS set, asked models it is agy's listing
	// (models_integration_test.go).
	if a := os.Args[1:]; os.Getenv("AGENTBUS_FAKE_AGY_MODELS") != "" && len(a) > 0 && a[len(a)-1] == "models" {
		fakeAgyModels()
	}
	if os.Getenv("AGENTBUS_FAKE_AGENT") == "1" {
		fakeAgent()
	}
	os.Exit(m.Run())
}

// fakePost sends body to $AGENTBUS_URL+path with the agent's own token and
// writes "<status> <body>" to out, when out is set.
func fakePost(path, body, out string) {
	req, _ := http.NewRequest("POST", os.Getenv("AGENTBUS_URL")+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("AGENTBUS_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if out != "" {
		os.WriteFile(out, []byte(fmt.Sprintf("%d %s", resp.StatusCode, b)), 0o600)
	}
}

// claudeGatewayEnv reads a variable AgentBus now passes to Claude Code inside a
// --settings argument's "env" block (the gateway URL, the Connect stand-in, and
// the stand-in auth token on every relayed path), falling back to a process env
// var for the one case that still sets one: the person's own real OpenRouter key
// (non-relay). The real Claude Code reads its settings "env" the same way; the
// fake mirrors it, as it already parses codex's -c arguments.
func claudeGatewayEnv(name string) string {
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--settings" {
			continue
		}
		var v struct {
			Env map[string]string `json:"env"`
		}
		if json.Unmarshal([]byte(args[i+1]), &v) == nil {
			if val, ok := v.Env[name]; ok {
				return val
			}
		}
	}
	return os.Getenv(name)
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
	fmt.Printf("url=%s\n", claudeGatewayEnv("ANTHROPIC_BASE_URL"))
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
		req, _ := http.NewRequest("POST", claudeGatewayEnv("ANTHROPIC_BASE_URL")+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		// As Claude Code sends a gateway credential, or the OAuth token of
		// claude setup-token. claudeGatewayEnv reads it from --settings on every
		// relayed path (subscription, API, OpenRouter stand-in) and from env only
		// for a real OpenRouter key.
		if tok := claudeGatewayEnv("ANTHROPIC_AUTH_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		} else if tok := claudeGatewayEnv("CLAUDE_CODE_OAUTH_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	// With AGENTBUS_FAKE_NET, a GET of each URL it lists (comma separated),
	// through the proxy the environment names, as a tool's download goes.
	// "net=<url> <status or no>" goes to stdout for each.
	if v := os.Getenv("AGENTBUS_FAKE_NET"); v != "" {
		c := &http.Client{Timeout: 5 * time.Second}
		for _, u := range strings.Split(v, ",") {
			if resp, err := c.Get(u); err == nil {
				resp.Body.Close()
				fmt.Printf("net=%s %d\n", u, resp.StatusCode)
			} else {
				fmt.Printf("net=%s no\n", u)
			}
		}
	}
	// With AGENTBUS_FAKE_CODEX_BODY, one model call as codex makes it: to the
	// openai_base_url its -c argument names, or the base_url of the provider
	// its -c arguments set, with the key its env_key names as the bearer, and
	// AGENTBUS_FAKE_THREAD as its Thread-Id header.
	if b := os.Getenv("AGENTBUS_FAKE_CODEX_BODY"); b != "" {
		var base, key string
		for _, a := range os.Args[1:] {
			if v, ok := strings.CutPrefix(a, `openai_base_url="`); ok {
				base = strings.TrimSuffix(v, `"`)
			}
			if _, v, ok := strings.Cut(a, `.base_url="`); ok && strings.HasPrefix(a, "model_providers.") {
				base = strings.TrimSuffix(v, `"`)
			}
			if _, v, ok := strings.Cut(a, `.env_key="`); ok && strings.HasPrefix(a, "model_providers.") {
				key = os.Getenv(strings.TrimSuffix(v, `"`))
			}
		}
		if base != "" {
			req, _ := http.NewRequest("POST", base+"/responses", strings.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Thread-Id", os.Getenv("AGENTBUS_FAKE_THREAD"))
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}
	// With AGENTBUS_FAKE_OPENCODE_BODY, one model call as opencode makes it
	// through an @ai-sdk/openai-compatible provider: to <baseURL>/chat/completions
	// of the one provider OPENCODE_CONFIG_CONTENT sets, with the key its
	// "{env:NAME}" names as the bearer, as opencode reads it.
	if b := os.Getenv("AGENTBUS_FAKE_OPENCODE_BODY"); b != "" {
		var cfg struct {
			Provider map[string]struct {
				Options struct {
					BaseURL string `json:"baseURL"`
					APIKey  string `json:"apiKey"`
				} `json:"options"`
			} `json:"provider"`
		}
		json.Unmarshal([]byte(os.Getenv("OPENCODE_CONFIG_CONTENT")), &cfg)
		for _, p := range cfg.Provider {
			req, _ := http.NewRequest("POST", p.Options.BaseURL+"/chat/completions", strings.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "opencode/1.19.6 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.4")
			if name, ok := strings.CutPrefix(p.Options.APIKey, "{env:"); ok {
				req.Header.Set("Authorization", "Bearer "+os.Getenv(strings.TrimSuffix(name, "}")))
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}
	// With AGENTBUS_FAKE_AGY_BODY, one model call as agy makes it: to
	// CLOUD_CODE_URL, with the access token of the sign-in file in its HOME
	// as the bearer.
	// On a Gemini API key it calls GOOGLE_GEMINI_BASE_URL instead, with
	// GEMINI_API_KEY in X-Goog-Api-Key and no Authorization, as agy 1.5.8
	// does (harness/agy.go WireAPI).
	if b := os.Getenv("AGENTBUS_FAKE_AGY_BODY"); b != "" && os.Getenv("GEMINI_API_KEY") != "" {
		req, _ := http.NewRequest("POST", os.Getenv("GOOGLE_GEMINI_BASE_URL")+"/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent?alt=sse", strings.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Goog-Api-Key", os.Getenv("GEMINI_API_KEY"))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	} else if b != "" {
		var f struct {
			Token struct {
				Access string `json:"access_token"`
			} `json:"token"`
		}
		raw, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli", "antigravity-oauth-token"))
		json.Unmarshal(raw, &f)
		req, _ := http.NewRequest("POST", os.Getenv("CLOUD_CODE_URL")+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if f.Token.Access != "" {
			req.Header.Set("Authorization", "Bearer "+f.Token.Access)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	// With AGENTBUS_FAKE_MODELS_CACHE, that file as codex's models_cache.json
	// in its CODEX_HOME, as codex keeps the models its sign-in may use.
	if c := os.Getenv("AGENTBUS_FAKE_MODELS_CACHE"); c != "" && os.Getenv("CODEX_HOME") != "" {
		os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "models_cache.json"), []byte(c), 0o600)
	}
	// With AGENTBUS_FAKE_OTHER ("METHOD /path"), one call that is no model
	// call, on the base the agent was given, carrying its credential as that
	// agent does: a turn trying its API key on another of the vendor's
	// endpoints.
	if o := os.Getenv("AGENTBUS_FAKE_OTHER"); o != "" {
		method, path, _ := strings.Cut(o, " ")
		var base string
		hdr := http.Header{}
		switch {
		case os.Getenv("GEMINI_API_KEY") != "":
			base = os.Getenv("GOOGLE_GEMINI_BASE_URL")
			hdr.Set("X-Goog-Api-Key", os.Getenv("GEMINI_API_KEY"))
		case os.Getenv("AGENTBUS_OPENAI_TOKEN") != "":
			for _, a := range os.Args[1:] {
				if _, v, ok := strings.Cut(a, `.base_url="`); ok && strings.HasPrefix(a, "model_providers.") {
					base = strings.TrimSuffix(strings.TrimSuffix(v, `"`), "/v1")
				}
			}
			hdr.Set("Authorization", "Bearer "+os.Getenv("AGENTBUS_OPENAI_TOKEN"))
		default:
			base = claudeGatewayEnv("ANTHROPIC_BASE_URL")
			hdr.Set("Authorization", "Bearer "+claudeGatewayEnv("ANTHROPIC_AUTH_TOKEN"))
		}
		req, _ := http.NewRequest(method, base+path, nil)
		req.Header = hdr
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			fmt.Printf("other=%d\n", resp.StatusCode)
		}
	}
	// With AGENTBUS_FAKE_ASK, a turn whose prompt is "ask someone" asks
	// another agent through the API, with its own token, as the ask_agent app
	// does (B15): create a session in this turn's own workspace (POST
	// /api/v0/sessions; the turn token derives the workspace), start the asked
	// turn in it (POST /api/v0/turns), then register the on that delivers its
	// reply to this session (POST /api/v0/ons).
	if b := os.Getenv("AGENTBUS_FAKE_ASK"); b != "" && len(os.Args) > 1 && os.Args[len(os.Args)-1] == "ask someone" {
		url, tok := os.Getenv("AGENTBUS_URL"), os.Getenv("AGENTBUS_TOKEN")
		sreq, _ := http.NewRequest("POST", url+"/api/v0/sessions", strings.NewReader("{}"))
		sreq.Header.Set("Content-Type", "application/json")
		sreq.Header.Set("Authorization", "Bearer "+tok)
		var sess struct {
			Session string `json:"session"`
		}
		if sresp, err := http.DefaultClient.Do(sreq); err == nil {
			sout, _ := io.ReadAll(sresp.Body)
			sresp.Body.Close()
			json.Unmarshal(sout, &sess)
		}
		var ask map[string]any
		json.Unmarshal([]byte(b), &ask)
		ask["session"] = sess.Session
		bb, _ := json.Marshal(ask)
		// An ask is async: a turn token cannot wait for a turn it starts (it runs
		// in the asker's workspace lane after the asker's turn ends), so wait=false.
		req, _ := http.NewRequest("POST", url+"/api/v0/turns?wait=false", strings.NewReader(string(bb)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var started struct {
				Turn string `json:"turn"`
			}
			if json.Unmarshal(out, &started) == nil && started.Turn != "" {
				on, _ := http.NewRequest("POST", os.Getenv("AGENTBUS_URL")+"/api/v0/ons", strings.NewReader(`{"turn":"`+started.Turn+`"}`))
				on.Header.Set("Content-Type", "application/json")
				on.Header.Set("Authorization", "Bearer "+os.Getenv("AGENTBUS_TOKEN"))
				if r2, err := http.DefaultClient.Do(on); err == nil {
					r2.Body.Close()
				}
			}
			if p := os.Getenv("AGENTBUS_FAKE_ASKED"); p != "" {
				os.WriteFile(p, []byte(fmt.Sprintf("%d %s", resp.StatusCode, out)), 0o600)
			}
		}
	}
	// With AGENTBUS_FAKE_MCP, one tools/call to control's tool server
	// ($AGENTBUS_URL/api/v0/mcp) with its own token, as Claude Code calls an
	// AgentBus tool: the value is the call's params. "<status> <body>" goes to
	// AGENTBUS_FAKE_MCP_OUT.
	if p := os.Getenv("AGENTBUS_FAKE_MCP"); p != "" {
		fakePost("/api/v0/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+p+`}`, os.Getenv("AGENTBUS_FAKE_MCP_OUT"))
	}
	// With AGENTBUS_FAKE_POST ("<path> <JSON body>"), one POST to control's
	// API with its own token, as agentbus-api sends it. "<status> <body>"
	// goes to AGENTBUS_FAKE_POST_OUT.
	if p, b, ok := strings.Cut(os.Getenv("AGENTBUS_FAKE_POST"), " "); ok {
		fakePost(p, b, os.Getenv("AGENTBUS_FAKE_POST_OUT"))
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
	// With AGENTBUS_FAKE_HOME=1 it keeps a transcript in its config folder,
	// as a vendor does: it reports the lines earlier turns left there, then
	// adds its own prompt. It also leaves a file in its HOME.
	var seen []string
	if os.Getenv("AGENTBUS_FAKE_HOME") == "1" {
		conf := os.Getenv("CLAUDE_CONFIG_DIR")
		if conf == "" {
			conf = os.Getenv("CODEX_HOME")
		}
		tr := filepath.Join(conf, "transcript")
		if b, err := os.ReadFile(tr); err == nil {
			seen = strings.Split(strings.TrimSpace(string(b)), "\n")
		}
		if f, err := os.OpenFile(tr, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, os.Args[len(os.Args)-1])
			f.Close()
		}
		os.WriteFile(filepath.Join(os.Getenv("HOME"), "left-by-a-turn"), nil, 0o600)
	}
	// A run keeps the child's output out of every reply, so it reports through a file: its
	// arguments, environment and folder.
	if p := os.Getenv("AGENTBUS_FAKE_OUT"); p != "" {
		cwd, _ := os.Getwd()
		b, _ := json.Marshal(fakeReport{Args: os.Args[1:], Env: os.Environ(), Cwd: cwd, Seen: seen})
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
	Seen []string `json:"seen,omitempty"` // AGENTBUS_FAKE_HOME: what earlier turns left
}
