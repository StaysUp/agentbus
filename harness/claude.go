package harness

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// claude is Claude Code.
type claude struct{}

func (claude) Info() Info {
	return Info{
		Name:    "claude",
		Label:   "Claude Code",
		Product: "Claude Code",
		// Claude Code wraps each shell command in its own srt sandbox, set per
		// run through --settings, so a Copy turn needs no whole-process wall.
		CopySandbox: true,
		// Answers with network access denied (Claude Code 2.1.318,
		// 2026-11-05, run under a sandbox profile that denies the network).
		StatusArgs:         []string{"auth", "status"},
		OwnEndpointEnv:     "ANTHROPIC_BASE_URL",
		Access:             standardAccess("Claude subscription", []string{"sonnet", "opus", "haiku"}, "Anthropic API key"),
		Replay:             FormatAnthropic,
		ReplayOnOpenRouter: true,
		// Claude Code keeps its settings, memory, plugins, transcripts and
		// .claude.json under CLAUDE_CONFIG_DIR when it is set.
		ConfigEnv: "CLAUDE_CONFIG_DIR", ConfigDir: ".claude",
		ToolServer: true,
		// With the tool server, Claude Code's own agents are hidden, so
		// AgentBus agents are the only agents the turn sees (his call,
		// 2026-11-10). Claude Code 2.1.321 (2026-11-11, read from the tool
		// list a haiku -p turn prints): Task starts a subagent (Agent in
		// newer versions), ListAgents lists other Claude sessions on the
		// machine and SendMessage writes to them. A turn asked "list other
		// agents" used ListAgents and answered there were none (2026-11-10).
		ToolsHidden: slices.Clone(claudeHidden),
		// claude setup-token prints a long-lived OAuth token "for CI
		// pipelines, scripts, or other environments", valid one year, which
		// Claude Code reads from CLAUDE_CODE_OAUTH_TOKEN
		// (code.claude.com/docs/en/authentication, read 2026-11-08). The
		// token's shape is not yet checked against a live login: the
		// person's first connect is that check.
		//
		// A wrong code (2.1.322, 2026-11-10, in a terminal) prints "OAuth
		// error: That code wasn't accepted. ..." and "Press Enter to
		// retry.", and waits: it does not exit.
		Login: &Login{Args: []string{"setup-token"}, Prefix: "sk-ant-oat", Token: `sk-ant-oat[0-9]{2}-[A-Za-z0-9_-]{20,}`,
			ValidDays: 365, Refused: `OAuth\s*error`},
		// Anthropic's API takes a key in x-api-key
		// (docs.anthropic.com/en/api/overview, read 2026-11-13).
		APIKey: &APIKey{Name: "anthropic", Label: "Anthropic API key", URL: "https://api.anthropic.com", Header: "X-Api-Key", Format: FormatAnthropic},
	}
}

// Wire: Claude Code's gateway address goes in --settings, not the environment.
// A process env var ANTHROPIC_BASE_URL loses to a user's own settings.json
// "env" block — or a cloned project's .claude/settings.json — but --settings
// wins over both, so a user config cannot silently redirect model calls off our
// gateway and escape recording and review (VERIFY_copy_level.md check 2 and agy
// review evt_768459feb737cd6f, both live, measured 2026-11-22; Claude Code
// 2.1.318 and 2.1.330).
func (claude) Wire(baseURL string) (env, args []string) {
	return nil, claudeSettings(map[string]string{"ANTHROPIC_BASE_URL": baseURL})
}

// WireRelay: a runner that holds no secret. The gateway, the Connect stand-in
// (placeholder in CLAUDE_CODE_OAUTH_TOKEN, where Claude Code reads a
// setup-token's token from, code.claude.com/docs/en/authentication, read
// 2026-11-08), and empty ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN go through
// --settings. So a settings.json can neither redirect the call nor let its own
// key preempt the stand-in: a settings.json ANTHROPIC_API_KEY or
// ANTHROPIC_AUTH_TOKEN makes Claude Code drop the OAuth token and send that key
// instead (agy review, live on claude 2.1.330, 2026-11-22). env clears a host
// CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN so none
// linger in cmd.Env (/proc/self/environ) — the --settings values are what Claude
// Code reads (measured live on claude 2.1.330, agy review 2026-11-22; the
// stand-in path was an inference until then).
func (claude) WireRelay(baseURL, placeholder string) (env, args []string) {
	return []string{"CLAUDE_CODE_OAUTH_TOKEN=", "ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN="},
		claudeSettings(map[string]string{
			"ANTHROPIC_BASE_URL":      baseURL,
			"CLAUDE_CODE_OAUTH_TOKEN": placeholder,
			"ANTHROPIC_API_KEY":       "",
			"ANTHROPIC_AUTH_TOKEN":    "",
		})
}

// WireOpenRouter: ANTHROPIC_BASE_URL at the service, every model alias set to
// the turn's model so a subagent or a small fast call uses it too, no
// experimental betas (a non-Anthropic upstream refuses them), and
// ANTHROPIC_API_KEY empty (OpenRouter's Claude Code guide says it must be) all
// pinned in --settings, so a settings.json cannot redirect the call and carry
// the OpenRouter token off to another host.
// The token (ANTHROPIC_AUTH_TOKEN) depends on w.StandIn. On every relayed turn
// (agentbus up and runner) it is a per-turn stand-in (abs_...): argv-safe, so it
// is pinned in --settings too — otherwise a settings.json ANTHROPIC_AUTH_TOKEN
// preempts it and the relay rejects the call with HTTP 401 (agy review, live on
// claude 2.1.330, 2026-11-22). Only the person's own real key (non-relay, from
// KeyEnv) stays in env, which argv (ps) must not hold; the pinned gateway keeps
// it reaching only our service even if a settings.json swaps the env token
// (which then only breaks that turn, never exfiltrates).
func (claude) WireOpenRouter(w OpenRouterWire) (env, args []string) {
	set := map[string]string{
		"ANTHROPIC_BASE_URL":                     w.Base,
		"ANTHROPIC_API_KEY":                      "",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
	}
	if w.Model != "" {
		for _, v := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"} {
			set[v] = w.Model
		}
	}
	if w.StandIn {
		set["ANTHROPIC_AUTH_TOKEN"] = w.Key
		return []string{"ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_API_KEY="}, claudeSettings(set)
	}
	return []string{"ANTHROPIC_AUTH_TOKEN=" + w.Key, "ANTHROPIC_API_KEY="}, claudeSettings(set)
}

// WireAPI: ANTHROPIC_BASE_URL at the service, the placeholder in
// ANTHROPIC_AUTH_TOKEN, which Claude Code sends as "Authorization: Bearer" (as
// for OpenRouter), and ANTHROPIC_API_KEY empty, so no key of the machine's is
// sent — all in --settings, so a settings.json cannot redirect the call or send
// a machine's own key. The placeholder is a stand-in (abs_...), safe in argv;
// control's relay puts the key set in Settings in x-api-key in its place. env
// clears any host ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY from cmd.Env. Claude
// Code names Anthropic's own models, so no alias is set: --model is enough.
func (claude) WireAPI(w APIWire) (env, args []string) {
	return []string{"ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_API_KEY="},
		claudeSettings(map[string]string{
			"ANTHROPIC_BASE_URL":   w.Base,
			"ANTHROPIC_AUTH_TOKEN": w.Placeholder,
			"ANTHROPIC_API_KEY":    "",
		})
}

// Edit: Claude Code's rules hold paths; a name that cannot go in one gets no
// edit rule, and a protected file that cannot be denied leaves edits out.
func (claude) Edit(folders, protect []string) ([]string, string) {
	for _, f := range folders {
		if !ruleSafe(f) {
			return nil, "folder_shape"
		}
	}
	for _, p := range protect {
		if !ruleSafe(p) {
			return nil, "protected_file_shape"
		}
	}
	return folders, ""
}

// OwnInstructions: --append-system-prompt adds to the person's own system
// prompt, never replaces it, so the hint always goes in.
// Refuse: every setting of permissions.toml has a flag on claude, or the
// gateway's guard.
func (claude) Refuse(Turn) string { return "" }

func (claude) OwnInstructions([]string, string) string { return "" }

// OwnTools says the person's own MCP servers (OwnServers) name one as the
// tool server, so the turn gets no tool server and its instructions name
// agentbus-api: their entry wins, as on codex. An edit turn loads their
// servers beside the --mcp-config one, and two of one name would leave which
// one Claude Code keeps to it.
func (c claude) OwnTools(env []string, _ string) string {
	if slices.Contains(c.OwnServers(env), ToolServer) {
		return "claude_mcp_server_set"
	}
	return ""
}

// OwnServers are the names in the mcpServers of the .claude.json Claude Code
// reads: $CLAUDE_CONFIG_DIR/.claude.json, which a session's home sets (Home
// files merges ~/.claude.json's mcpServers there), else $HOME/.claude.json
// (Claude Code 2.1.323). Names only: a server's settings are never kept. A
// missing file, or one that does not parse, names none: Claude Code then
// loads none of it either.
func (claude) OwnServers(env []string) []string {
	path := ""
	if d := envValue(env, "CLAUDE_CONFIG_DIR"); d != "" {
		path = filepath.Join(d, ".claude.json")
	} else if h := envValue(env, "HOME"); h != "" {
		path = filepath.Join(h, ".claude.json")
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	names := make([]string, 0, len(v.MCPServers))
	for n := range v.MCPServers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Native: Claude Code reads the folder's CLAUDE.md and .claude/CLAUDE.md,
// and a file such a CLAUDE.md imports with a line "@<file>". With neither
// CLAUDE.md, it reads the folder's AGENTS.md instead; with one, it does not
// (Claude Code 2.1.318 and 2.1.321, 2026-11-10, checked live: a folder with
// AGENTS.md alone, with a CLAUDE.md beside it, with .claude/CLAUDE.md beside
// it, and with a CLAUDE.md that imports @AGENTS.md). So it reads name when no
// CLAUDE.md is there and name is AGENTS.md, when a CLAUDE.md is name itself
// (a symlink to it), or when the folder's CLAUDE.md imports it by a line
// "@name" or "@./name". Any other way of pointing at it is not seen, and the
// file is then given again.
func (claude) Native(folder *os.Root, name string) bool {
	want, err := folder.Stat(name)
	if err != nil {
		return false
	}
	own := false
	for _, f := range []string{"CLAUDE.md", ".claude/CLAUDE.md"} {
		if _, err := folder.Lstat(f); err != nil {
			continue
		}
		own = true
		if fi, err := folder.Stat(f); err == nil && os.SameFile(fi, want) {
			return true
		}
	}
	if !own {
		return name == "AGENTS.md"
	}
	f, err := OpenFile(folder, "CLAUDE.md")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l == "@"+name || l == "@./"+name {
			return true
		}
	}
	return false
}

// ResumeID is the session_id of the "type":"result" object that
// --output-format json prints.
func (claude) ResumeID(line []byte, current string) string {
	if v, ok := decodeLine(line); ok && v.Type == "result" && resumeIDShape.MatchString(v.SessionID) {
		return v.SessionID
	}
	return current
}

func (claude) Args(t Turn) []string {
	// --output-format json prints one result object with the session_id.
	// (No --setting-sources: it would also stop Claude Code reading the folder's
	// CLAUDE.md / AGENTS.md, losing the project's instructions. A project
	// settings.json cannot bypass the gateway anyway — claudeSettings pins every
	// bypass key in --settings, which wins over the project and user settings.json
	// alike by command-line precedence.)
	a := []string{"-p", "--output-format", "json"}
	// Copy mode: Claude Code's own per-command sandbox, as a second --settings
	// the engine folds into the gateway one (CombineSettings). Writes fenced to
	// the copy, the login file hidden, the network a broad allow-list.
	if t.Copy.Dir != "" {
		sb, _ := json.Marshal(map[string]any{"sandbox": claudeSandboxBlock(t.Copy.Dir, t.Copy.Write, t.Copy.DenyRead, t.Copy.Hosts)})
		a = append(a, "--settings", string(sb))
	}
	// The hint goes at the end of Claude Code's system prompt. Own replay
	// keeps each request's system prompt as the agent sent it, and the
	// text is the same on every turn of the session, so the main thread
	// is still recognised and the prompt cache still holds.
	if t.Hint != "" {
		a = append(a, "--append-system-prompt", t.Hint)
	}
	if t.ResumeID != "" {
		a = append(a, "--resume", t.ResumeID)
	}
	// One --allowedTools, comma-joined (Claude Code 2.1.318,
	// 2026-11-05): what the tools may do without asking.
	var allowed []string
	// edit: Edit(<folder>/**) lets Edit and Write change files there
	// without asking; one Edit rule covers every built-in tool that
	// edits files. Without it a -p turn cannot edit at all: nobody
	// is there to approve.
	allowed = append(allowed, editRules(t.Writes.Edit)...)
	// The AgentBus gateway decides every shell command of the turn — allow,
	// ask, forbid, and a command on no list — acting on the model's reply
	// before the agent runs the call (gateway/guard.go). So the whole Bash
	// tool is allowed here and the gateway is the gate: Claude Code's own
	// literal Bash check must not refuse a command first. That check split
	// a chain at ";" and did not match an env prefix, so it refused an
	// allowed command with nowhere to approve it (his finding, turn
	// t_f088dacca881acf7, 2026-11-16). The person's config is never edited;
	// this is a per-turn flag. File edit rules above are unchanged.
	allowed = append(allowed, "Bash")
	allowed = append(allowed, claudeTools(t.Tools)...)
	// The person's own MCP servers load (no --strict-mcp-config), and
	// a -p turn has nobody to approve a call: mcp__<server> lets every
	// tool of each run without asking, as for the tool server. A name
	// of another shape gets no rule: Claude Code rewrites it in its
	// tool names, so a rule could miss or widen; its tools then run
	// only as the person's own settings allow.
	for _, n := range t.OwnServers {
		if r := "mcp__" + n; serverNameShape.MatchString(n) && !slices.Contains(allowed, r) {
			allowed = append(allowed, r)
		}
	}
	if len(allowed) > 0 {
		a = append(a, "--allowedTools", strings.Join(allowed, ","))
	}
	// The tool server comes in by --mcp-config on every turn that has it.
	if t.Tools != "" {
		a = append(a, "--mcp-config", claudeMCPConfig(t.Tools))
	}
	// A review is a normal turn now (read-only retired, B12): it loads the
	// person's own servers like any turn, in a session's home from Home
	// files copied in (his decision, 2026-11-14). Calls are recorded as
	// tool.requested under their mcp__ names.
	// --no-chrome: Claude in Chrome is not an MCP server of the person's
	// settings, so --strict-mcp-config leaves it on (measured 2026-11-14,
	// research/2026-11-14-use-my-own-tools-design.md), and it cannot work
	// with a sign-in AgentBus holds. Off on every turn.
	a = append(a, "--no-chrome")
	// forbid is the gateway's: it renames a forbidden call in the reply before
	// the agent runs it, for claude and codex alike, in the shapes a
	// Bash(<cmd>:*) rule misses — git -C repo push, env prefixes, chains
	// (gateway/guard.go). No Bash deny here: with the whole Bash tool allowed
	// above, the agent decides nothing about shell; the one gate does. The data
	// folder's own files are still never edited: a deny on each, which wins over
	// the edit rule and the person's settings alike.
	var denied []string
	denied = append(denied, protectRules(t.Writes.Protect)...)
	// Temporary glue: mapping tool:edit to Claude Code tools Edit, Write, NotebookEdit.
	// No edit rule: a deny on every tool that edits files, so the
	// person's own settings (an acceptEdits mode, an Edit allow
	// rule) cannot approve one either. A deny wins over any allow.
	if len(t.Writes.Edit) == 0 {
		denied = append(denied, "Edit", "Write", "NotebookEdit")
	}
	// Temporary glue: mapping tool:subagent to Claude Code tools Task, Agent, Workflow.
	// subagents off: a deny on each tool that starts a subagent. Task
	// is its name in Claude Code 2.1.319 (2026-11-09, read from the tool
	// list a haiku turn prints, and gone from it with this deny); Agent is
	// its name in newer versions. Workflow runs scripts that start
	// subagents.
	if t.NoSubagents {
		denied = append(denied, "Task", "Agent", "Workflow")
	}
	// With the tool server, the tools AgentBus tools replace are denied
	// (Info.ToolsHidden).
	if t.Tools != "" {
		for _, n := range claudeHidden {
			if !slices.Contains(denied, n) {
				denied = append(denied, n)
			}
		}
	}
	if len(denied) > 0 {
		a = append(a, "--disallowedTools", strings.Join(denied, ","))
	}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	return append(a, "--", t.Prompt)
}

// claudeHidden is claude's Info.ToolsHidden.
var claudeHidden = []string{"Task", "Agent", "ListAgents", "SendMessage"}

// claudeMCPConfig is the --mcp-config value of a turn with the tool server at
// url: inline JSON, so nothing is written. Claude Code expands
// ${AGENTBUS_TOKEN} from its own environment when it loads the server
// (2.1.321, 2026-11-11, checked live), so the token is in no argument.
func claudeMCPConfig(url string) string {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{ToolServer: map[string]any{
		"type": "http", "url": url,
		"headers": map[string]string{"Authorization": "Bearer ${AGENTBUS_TOKEN}"}}}})
	return string(b)
}

// claudeCopyDomains is the default broad network allow-list for a Copy turn on
// the srt-backed sandbox. srt refuses "*" ("overly broad... not allowed") and
// requires a network block, so "open" is this list plus allowLocalBinding for
// localhost (measured 2026-11-23 against srt v0.0.83; DESIGN_contain_mode "Copy
// network: the honest limit"). The gateway host is added per run. Widen from
// real use.
var claudeCopyDomains = []string{
	"registry.npmjs.org",
	"pypi.org", "files.pythonhosted.org",
	"proxy.golang.org", "sum.golang.org",
	"crates.io", "static.crates.io",
	"github.com", "codeload.github.com", "*.githubusercontent.com",
}

// claudeSandboxBlock is Claude Code's "sandbox" settings for a Copy turn: writes
// fenced to the copy plus the extra writable paths (the per-run temp and tool
// caches), reads open across the disk except denyRead (the CLI's own login
// file, so Connect's stand-in is used), and the network a broad allow-list
// (claudeCopyDomains plus hosts, e.g. the gateway) with localhost reachable
// through allowLocalBinding. deniedDomains, denyWrite and allowRead are present
// and empty because srt's config requires them. Reads are open by default, so
// allowRead stays empty and denyRead carves out the login file.
func claudeSandboxBlock(copyDir string, write, denyRead, hosts []string) map[string]any {
	allowWrite := append([]string{copyDir}, write...)
	domains := append(append([]string{}, hosts...), claudeCopyDomains...)
	if denyRead == nil {
		denyRead = []string{} // a nil marshals as JSON null, which Claude Code rejects
	}
	return map[string]any{
		"enabled": true,
		// A cloned project's .claude/settings.json can add to allowWrite (and
		// run commands unsandboxed) unless this is false — measured live on
		// claude 2.1.330 (agy review 2026-11-23): without it a repo settings.json
		// allowWrite escaped the copy; with it the write was refused.
		"allowUnsandboxedCommands": false,
		// Without the whole-process wall behind it (perCommand), a sandbox
		// backend that cannot start must fail the turn, not run it unconfined:
		// Claude Code's default here is false (silent fail-open).
		"failIfUnavailable": true,
		"filesystem": map[string]any{
			"allowRead":  []string{},
			"denyRead":   denyRead,
			"allowWrite": allowWrite,
			"denyWrite":  []string{},
		},
		"network": map[string]any{
			"allowedDomains":    domains,
			"deniedDomains":     []string{},
			"allowLocalBinding": true,
		},
	}
}

// claudeSettings is the --settings argument pair pinning environment values
// Claude Code must not let a settings.json "env" override — a user's own, or a
// cloned project's .claude/settings.json. Claude Code's settings "env" wins
// over settings.json and over a process env var of the same name (measured live
// on claude 2.1.330, agy review 2026-11-22). The gateway URL above all: a
// settings.json base URL redirects every model call off our gateway, carrying
// the turn's credential with it. Values forced to "" (ANTHROPIC_API_KEY,
// ANTHROPIC_AUTH_TOKEN) stop a settings.json key preempting the intended
// sign-in. A REAL secret never goes here: argv is readable by ps, so a real key
// stays in the process env, where the pinned gateway keeps it reaching only our
// service. json.Marshal sorts the keys, so the value is stable for the launch
// golden.
func claudeSettings(env map[string]string) []string {
	// On the personal runner Claude Code reads the person's real ~/.claude
	// settings AND the folder's project .claude/settings.json (a cloned repo's,
	// untrusted), so --settings pins every setting that could route a call off
	// our gateway or override the sign-in — and --settings (the command line)
	// wins over both the project and the user settings.json by precedence, so no
	// settings.json layer can undo a pin. This is the ONE place that list lives;
	// a new bypass vector is added here (all measured live on claude 2.1.330,
	// agy review 2026-11-23):
	//   - CLAUDE_CODE_USE_BEDROCK/_VERTEX/_FOUNDRY send calls straight to that
	//     backend, past ANTHROPIC_BASE_URL — zero requests reach our gateway.
	//   - ANTHROPIC_CUSTOM_HEADERS overwrites the stand-in Authorization header.
	// --settings "env" wins over settings.json and host env.
	for _, k := range claudePinnedEnv {
		if _, ok := env[k]; !ok {
			env[k] = ""
		}
	}
	// Pin NO_PROXY so a settings.json HTTP_PROXY / ALL_PROXY cannot route the
	// gateway through a proxy (zero calls reached it otherwise; measured live,
	// claude 2.1.330). HTTP_PROXY itself is NOT cleared — that would break an
	// external proxy sets — only the gateway is exempted. There is no
	// whole-process wall any more, so the gateway is reached directly whatever
	// address it listens on (loopback, or a LAN/Tailscale -listen IP), and its
	// exact host:port and bare host are both added (claude's NO_PROXY matches a
	// ":"-bearing token only by equality, so "[::1]:port" needs the host:port).
	noProxy := claudeNoProxy
	if u, err := url.Parse(env["ANTHROPIC_BASE_URL"]); err == nil && u.Host != "" {
		for _, x := range []string{u.Host, u.Hostname()} {
			if x != "" && !strings.Contains(","+noProxy+",", ","+x+",") {
				noProxy += "," + x
			}
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		if _, ok := env[k]; !ok {
			env[k] = noProxy
		}
	}
	// apiKeyHelper (a top-level key) would otherwise run and send its key instead
	// of the Connect stand-in, so the relay rejects the turn and the real key
	// reaches the gateway. "" suppresses it.
	b, _ := json.Marshal(map[string]any{"env": env, "apiKeyHelper": ""})
	return []string{"--settings", string(b)}
}

// claudePinnedEnv are Claude Code env settings forced empty in every turn's
// --settings so a person's real (or a cloned repo's) settings.json cannot route
// a call off our gateway or overwrite the sign-in.
var claudePinnedEnv = []string{
	// Alternate model backends: each skips ANTHROPIC_BASE_URL and sends calls to
	// that provider (all live-verified on claude 2.1.330).
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_USE_GATEWAY",
	// ANTHROPIC_CUSTOM_HEADERS overwrites the stand-in Authorization header;
	// ANTHROPIC_UNIX_SOCKET redirects every Anthropic request to a unix socket.
	"ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_UNIX_SOCKET",
}

// claudeNoProxy exempts the loopback gateway from any proxy a settings.json sets
// (localhost and both IPv4 and IPv6 loopback), matching netjail's own NO_PROXY.
const claudeNoProxy = "localhost,127.0.0.1,::1"

// LoginFiles is Claude Code's own sign-in file, hidden in Copy mode so the turn
// uses Connect's stand-in: $CLAUDE_CONFIG_DIR/.credentials.json, or
// $HOME/.claude/.credentials.json. (On macOS Claude Code may hold the token in
// the Keychain instead; a live run confirms whether a file deny is enough.)
func (claude) LoginFiles(env []string) []string {
	var files []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		if f := filepath.Join(dir, ".credentials.json"); !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	add(envValue(env, "CLAUDE_CONFIG_DIR"))
	if h := envValue(env, "HOME"); h != "" {
		add(filepath.Join(h, ".claude"))
	}
	return files
}

// CombineSettings folds the turn's --settings arguments into one. A Copy turn
// carries two — the gateway env block from Wire and the sandbox block from Args
// — and Claude Code is not relied on to merge two --settings flags. The merged
// object keeps each one's top-level keys (env and sandbox are disjoint) and is
// prepended at index 0, the rest in order. One --settings, or none, is returned
// as given.
//
// The scan walks by flag position so a flag VALUE equal to "--settings" is not
// read as a flag: a value (an instruction, a tool rule) is attacker-reachable,
// and a mis-parse that left two --settings in place made Claude Code drop the
// first and bypass the gateway (agy review, live on claude 2.1.330, 2026-11-23).
func (claude) CombineSettings(args []string) []string {
	var at []int
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break // the prompt and anything after it are positional, not flags
		}
		if args[i] == "--settings" && i+1 < len(args) {
			at = append(at, i)
			i++ // the value is not a flag
			continue
		}
		if claudeValueFlags[args[i]] {
			i++ // skip this flag's value, which is not a flag
		}
	}
	if len(at) < 2 {
		return args
	}
	merged := map[string]any{}
	drop := map[int]bool{}
	for _, i := range at {
		var v map[string]any
		if json.Unmarshal([]byte(args[i+1]), &v) != nil {
			return args // leave a malformed value alone
		}
		for k, val := range v {
			merged[k] = val
		}
		drop[i], drop[i+1] = true, true
	}
	b, _ := json.Marshal(merged)
	out := []string{"--settings", string(b)}
	for i := 0; i < len(args); i++ {
		if !drop[i] {
			out = append(out, args[i])
		}
	}
	return out
}

// claudeValueFlags are the flags claude.Args emits that take a following value,
// so CombineSettings skips that value when scanning for --settings flags.
var claudeValueFlags = map[string]bool{
	"--settings": true, "--append-system-prompt": true, "--output-format": true,
	"--resume": true, "--allowedTools": true, "--mcp-config": true,
	"--disallowedTools": true, "--model": true,
}

// claudeTools is the --allowedTools rule that lets the tool server's tools run
// without asking: a -p turn has nobody to ask. What a call may do is the
// turn token's to say.
func claudeTools(url string) []string {
	if url == "" {
		return nil
	}
	return []string{"mcp__" + ToolServer}
}

// ruleSafe says p can go inside a Claude Code rule as it is: a comma or a
// parenthesis would end the rule, and a glob character would widen it.
func ruleSafe(p string) bool {
	return !strings.ContainsAny(p, ",()*?[]{}") && !strings.ContainsFunc(p, isControl)
}

// editRules is Claude Code's rule that lets the tools that edit files change
// anything under each folder without asking: Edit(//<folder>/**).
func editRules(folders []string) []string {
	var rules []string
	for _, f := range folders {
		rules = append(rules, "Edit(/"+strings.TrimSuffix(f, "/")+"/**)")
	}
	return rules
}

// protectRules is the deny rule for each protected file: Edit(//<file>).
func protectRules(files []string) []string {
	var rules []string
	for _, f := range files {
		rules = append(rules, "Edit(/"+f+")")
	}
	return rules
}
