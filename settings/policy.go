package settings

// The policy says what a turn may do without a person to ask: folders it may
// read beyond its own, commands it may run, commands it may never run, where
// it may edit, whether it may start subagents, the limits on work agents ask
// for, and what a turn's own token may call. The file store keeps it in
// permissions.toml. A key left out holds its default. run/permissions.go says
// what each list becomes for each agent.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// PolicyStarter is what the file store writes when permissions.toml is
// missing. Its lists are empty, and every other value is written out at its
// default, so a new file changes nothing. A key left out of a file reads as
// its default too. FormatPolicy writes a policy in this same layout.
const PolicyStarter = `# What AgentBus turns may do without asking. Read at the start of every turn,
# and by the API for each request a turn's token sends: an edit applies at once.
# A key left out reads as the default written here. Folders may start with ~.
#
# read_folders: folders claude may read and search beyond its own folder. Reads only in the
# default and acceptEdits modes; under your own "auto" mode, Claude Code may still write there.
# allow: commands claude may run without asking (a command and its arguments).
# forbid: commands claude and codex may never run. The gateway refuses them.
# It reads the command text, so it cannot see a command built at run time or
# run from a script. For a hard rule, also leave out what the command needs.
# codex can read every folder already, and has no allow list yet.

read_folders = []   # e.g. ["~/.agents", "~/Sites/agentbus-notes"]
allow = []          # e.g. ["go test", "make test", "git commit"]
forbid = []         # e.g. ["git push", "git checkout"]

# edit: where a turn may create and change files without asking.
#   "session"  its own session's folder: the session's worktree, or the folder
#              itself when it is not in a git repo. claude gets Edit and Write
#              there; codex runs with --sandbox workspace-write.
#   "none"     nowhere. claude is denied Edit, Write and NotebookEdit, whatever
#              your own settings allow; codex runs with --sandbox read-only,
#              which also has no network.
# A read-only turn never edits. forbid still wins. No turn may edit the files
# of the data folder (this file, instructions.md, agents.json, the tokens)
# with its edit tools. A command in allow that runs any code (python3, sh)
# can still write them: allow only commands you would let change this file.
edit = "session"

# isolation: where a new session's turns run.
#   "auto"     the fastest copy this machine can make: on macOS "copy" (one
#              APFS clone of the folder); on Linux "overlay" where the runner
#              can mount one, else "copy".
#   "overlay"  Linux: an overlay mount over a read-only copy of the folder,
#              shared by every session made from it. Needs root or
#              CAP_SYS_ADMIN; without it, "copy", and the log says why.
#   "copy"     a copy of the session's folder, exactly as it is now,
#              uncommitted edits included. Copy-on-write where the disk has it
#              (APFS, btrfs, xfs); a plain copy elsewhere, such as ext4, is
#              slow for a big folder.
#   "git"      in a git repo, a git worktree at HEAD with the folder's
#              uncommitted edits carried in (ignored files are not);
#              elsewhere, a copy.
#   "none"     the folder itself: turns change your files directly.
# Your folder is not changed until a change is applied. A session started
# with isolate false always uses the folder itself.
isolation = "auto"

# share_read_only: folders at the top of a session's folder that a "copy"
# shares instead of copying, e.g. ["node_modules"]. Every session made from
# the folder as it is links to one read-only copy of each, which makes a copy
# of a project with a big node_modules much faster on macOS. A turn that
# writes there (npm install) fails with "permission denied"; leave the list
# empty for agents that install packages. Not used by "overlay", which copies
# nothing per session anyway.
share_read_only = []

# subagents: whether a turn may start the agent's own subagents. false turns
# them off on the command line: claude is denied its Task, Agent and Workflow
# tools; codex runs with its multi_agent features disabled. Your own settings
# files are never changed.
subagents = true

# Work agents ask other agents for. A request tree is the person's session and
# every session work asked for from it, at any depth. A limit reached refuses
# the request and says which limit.
[limits]
max_depth = 2                # levels of asking below the person's turn
max_jobs_per_turn = 10       # turns and reviews one turn may ask for
max_jobs_per_tree = 30       # turns and reviews one request tree may ask for in all
max_cost_usd_per_tree = 0.0  # USD one request tree may cost before asking stops; 0 is off

# What a turn's own token (AGENTBUS_TOKEN, used by agentbus-api) may do.
# No setting here lets it change this file, the instructions, saved agents or
# tokens: those take the main token, always.
[agent_api]
scope = "tree"       # whose sessions it reads and reviews:
                     # "tree" (its request tree) or "session" (its own only)
read = true          # read sessions, turns and calls in its scope
list_agents = true   # list saved agents and harnesses
start = true         # start a new session on a saved agent
review = true        # ask for a review of a turn in its scope
cancel = true        # stop turns its tree asked for
rename = true        # rename sessions in its scope
apply = false        # apply a turn's changes to the person's folder, for a turn in its scope;
                     # off by default: only the main token applies
tools = false        # true: give each claude and codex turn the agentbus MCP tools (MCP), which
                     # call this API as its token may, and hide claude's own Task, Agent,
                     # ListAgents and SendMessage. false: agents call it with agentbus-api

# What a turn's agent may reach on the network. Linux only: macOS has no
# network control, and its agents reach any host.
# contain: whether each agent runs closed off from the network.
#   "off"   the default: agents reach any host.
#   "on"    an agent reaches only its runner's gateway, the server's API,
#           and the hosts in allow, through a proxy. Anything else is
#           refused and recorded (network.refused). If the runner cannot
#           close the network off, such as on macOS, the turn is refused.
# allow: host names an agent may reach, on any port, such as "github.com".
# "*.example.com" also allows every name under example.com.
[network]
contain = "off"
allow = []           # e.g. ["github.com", "registry.npmjs.org"]

# A wall around each session, built by the runner around the agent's whole
# process. A proof of concept: off by default.
# mode: "off" (the default) or "on". On, a session reads and writes only its
# own folder and its own home; it reads the system folders and the agent's
# own program; nothing else of your home and no other session. Its network
# goes only through the runner's proxy: the public internet when contain is
# "off" (this machine and its networks only by [network] allow), the allow
# list when "on". macOS uses Seatbelt,
# Linux bubblewrap inside the network namespace. A runner that cannot build
# the wall refuses the turn, and a runner with no home per session (agentbus
# up) cannot build it.
# read: files or folders under your home, such as "~/.aws/config", copied
# into the session's home at the start of each turn, at the same place.
[sandbox]
mode = "off"
read = []            # e.g. ["~/.aws/config"]
`

// Edit values: where a turn may edit files without asking.
const (
	EditSession = "session"
	EditNone    = "none"
)

// Isolation values: where a new session's turns run.
const (
	IsolationAuto    = "auto"
	IsolationOverlay = "overlay"
	IsolationCopy    = "copy"
	IsolationGit     = "git"
	IsolationNone    = "none"
)

// Scope values of [agent_api]: the sessions a turn token reaches.
const (
	ScopeTree    = "tree"
	ScopeSession = "session"
)

// Contain values of [network]: whether a turn's agent runs closed off from
// the network.
const (
	ContainOff = "off"
	ContainOn  = "on"
)

// Mode values of [sandbox]: whether each session runs inside a wall.
const (
	SandboxOff = "off"
	SandboxOn  = "on"
)

// The defaults, as the starter file writes them.
const (
	DefaultMaxDepth       = 2
	DefaultMaxJobsPerTurn = 10
	DefaultMaxJobsPerTree = 30
)

// Policy is the policy as written: folders as the person wrote them, ~ and
// all, since they name folders on the machine that runs the turn (Resolve).
// Once read from a store every value is filled: a key left out holds its
// default, and a list is empty, never null.
type Policy struct {
	ReadFolders   []string `toml:"read_folders" json:"read_folders" doc:"Folders claude may read and search beyond its own folder: absolute, or starting with ~/. Each must exist on the machine that runs the turn."`
	Allow         []string `toml:"allow" json:"allow" doc:"Commands claude may run without asking: a command and its leading arguments, such as \"go test\"."`
	Forbid        []string `toml:"forbid" json:"forbid" doc:"Commands claude and codex may never run. The gateway refuses them. It matches each simple command by its leading words and the program's base name, through pipes, sh -c, sudo and env. It reads the command text, so it is not a sandbox: a command built at run time is not seen."`
	Edit          string   `toml:"edit" json:"edit,omitempty" doc:"Where a turn may edit files without asking: session (its own session's folder) or none. Absent in a PUT: session."`
	Isolation     string   `toml:"isolation" json:"isolation,omitempty" doc:"Where a new session's turns run: auto (copy on macOS; overlay on Linux where the runner can mount one, else copy), overlay, copy (a copy of its folder as it is now), git (a git worktree with the folder's uncommitted edits, in a repo) or none (the folder itself). On ext4, copy and the overlay's snapshot copy every byte: use xfs, btrfs, git or none for a big folder. Absent in a PUT: auto."`
	ShareReadOnly []string `toml:"share_read_only" json:"share_read_only" doc:"Folders at the top of a session's folder, such as node_modules, that a copy links to one shared read-only copy of instead of copying. A turn cannot write there. Absent in a PUT: none."`
	Subagents     *bool    `toml:"subagents" json:"subagents,omitempty" doc:"Whether a turn may start the agent's own subagents. Absent in a PUT: true."`
	Limits        Limits   `toml:"limits" json:"limits" doc:"The limits on work agents ask for."`
	AgentAPI      AgentAPI `toml:"agent_api" json:"agent_api" doc:"What a turn's own token may call."`
	Network       Network  `toml:"network" json:"network" doc:"What a turn's agent may reach on the network (Linux runners, and any runner with [sandbox] on)."`
	Sandbox       Sandbox  `toml:"sandbox" json:"sandbox" doc:"A wall around each session's agent (a proof of concept)."`

	// where and fix say, in a turn's refusal, where the policy came from and
	// what to do: set by the store that read it (Error).
	where, fix string
}

// Limits are the caps on work agents ask for ([limits]). A pointer is a key
// left out; once read from a store each is set.
type Limits struct {
	MaxDepth          *int     `toml:"max_depth" json:"max_depth,omitempty" doc:"Levels of asking below the person's turn. Absent in a PUT: 2."`
	MaxJobsPerTurn    *int     `toml:"max_jobs_per_turn" json:"max_jobs_per_turn,omitempty" doc:"Turns and reviews one turn may ask for. Absent in a PUT: 10."`
	MaxJobsPerTree    *int     `toml:"max_jobs_per_tree" json:"max_jobs_per_tree,omitempty" doc:"Turns and reviews one request tree may ask for in all. Absent in a PUT: 30."`
	MaxCostUSDPerTree *float64 `toml:"max_cost_usd_per_tree" json:"max_cost_usd_per_tree,omitempty" doc:"USD one request tree may cost before asking stops; 0 is off. Absent in a PUT: 0."`
}

// AgentAPI says what a turn token may call ([agent_api]). A pointer is a key
// left out; once read from a store each is set.
type AgentAPI struct {
	Scope      string `toml:"scope" json:"scope,omitempty" doc:"Whose sessions a turn token reaches: tree (its request tree) or session (its own only). Absent in a PUT: tree."`
	Read       *bool  `toml:"read" json:"read,omitempty" doc:"Read sessions, turns and calls in its scope. Absent in a PUT: true."`
	ListAgents *bool  `toml:"list_agents" json:"list_agents,omitempty" doc:"List saved agents and harnesses. Absent in a PUT: true."`
	Start      *bool  `toml:"start" json:"start,omitempty" doc:"Start a new session on a saved agent. Absent in a PUT: true."`
	Review     *bool  `toml:"review" json:"review,omitempty" doc:"Ask for a review of a turn in its scope. Absent in a PUT: true."`
	Cancel     *bool  `toml:"cancel" json:"cancel,omitempty" doc:"Stop turns its tree asked for. Absent in a PUT: true."`
	Rename     *bool  `toml:"rename" json:"rename,omitempty" doc:"Rename sessions in its scope. Absent in a PUT: true."`
	Apply      *bool  `toml:"apply" json:"apply,omitempty" doc:"Apply a turn's changes to the person's folder, for a turn of a session in its scope. Off by default: only the main token applies. Absent in a PUT: false."`
	Tools      *bool  `toml:"tools" json:"tools,omitempty" doc:"Give each turn the agentbus MCP tools over MCP (POST /api/v0/mcp), which call this API as its token may. claude's own Task, Agent, ListAgents and SendMessage tools are hidden then. false: agents call the API with agentbus-api only. Absent in a PUT: false."`
}

// Network says what a turn's agent may reach ([network]). Read by the
// runner that starts the agent (netjail).
type Network struct {
	Contain string   `toml:"contain" json:"contain,omitempty" doc:"Whether each agent runs closed off from the network: off (the default; agents reach any host) or on. On, it reaches only its runner's gateway, the server's API and the hosts in allow; anything else is refused and recorded as network.refused. Linux only: a runner that cannot close an agent off refuses the turn, so on a Mac on refuses every turn unless [sandbox] is on. Host names are checked, not addresses. Absent in a PUT: off."`
	Allow   []string `toml:"allow" json:"allow" doc:"Host names a closed-off agent may reach, on any port, such as github.com. *.example.com also allows every name under example.com. Absent in a PUT: none."`
}

// Sandbox says whether each session runs inside a wall ([sandbox]). Read by
// the runner that starts the agent (package sandbox).
type Sandbox struct {
	Mode string   `toml:"mode" json:"mode,omitempty" doc:"off (the default) or on. On, a session's agent reads and writes only the session's folder and home, read the system folders and the agent's program, and reach the network only through the runner's proxy. A runner that cannot build it refuses the turn. macOS uses sandbox-exec and Linux bwrap. A runner with -gateway local cannot build it (a session has no home of its own there). The copy's .git stays writable. Absent in a PUT: off."`
	Read []string `toml:"read" json:"read" doc:"Files or folders under the home folder, each starting with ~/, copied into the session's home at the same place at the start of each turn, such as ~/.aws/config. Absent in a PUT: none."`
}

// DefaultPolicy is the policy of a missing or empty file.
func DefaultPolicy() Policy {
	var p Policy
	p.fill()
	return p
}

// fill sets every value left out to its default.
func (p *Policy) fill() {
	for _, l := range []*[]string{&p.ReadFolders, &p.Allow, &p.Forbid, &p.ShareReadOnly, &p.Network.Allow, &p.Sandbox.Read} {
		if *l == nil {
			*l = []string{}
		}
	}
	if p.Edit == "" {
		p.Edit = EditSession
	}
	if p.Isolation == "" {
		p.Isolation = IsolationAuto
	}
	if p.Subagents == nil {
		on := true
		p.Subagents = &on
	}
	setInt := func(v **int, d int) {
		if *v == nil {
			*v = &d
		}
	}
	setInt(&p.Limits.MaxDepth, DefaultMaxDepth)
	setInt(&p.Limits.MaxJobsPerTurn, DefaultMaxJobsPerTurn)
	setInt(&p.Limits.MaxJobsPerTree, DefaultMaxJobsPerTree)
	if p.Limits.MaxCostUSDPerTree == nil {
		off := 0.0
		p.Limits.MaxCostUSDPerTree = &off
	}
	if p.AgentAPI.Scope == "" {
		p.AgentAPI.Scope = ScopeTree
	}
	if p.Network.Contain == "" {
		p.Network.Contain = ContainOff
	}
	if p.Sandbox.Mode == "" {
		p.Sandbox.Mode = SandboxOff
	}
	for _, b := range []**bool{&p.AgentAPI.Read, &p.AgentAPI.ListAgents, &p.AgentAPI.Start,
		&p.AgentAPI.Review, &p.AgentAPI.Cancel, &p.AgentAPI.Rename} {
		if *b == nil {
			on := true
			*b = &on
		}
	}
	// Only the main token applies unless the person says so (defaults
	// version 2). Version 1 writes its own value out (defaultsV1).
	if p.AgentAPI.Apply == nil {
		off := false
		p.AgentAPI.Apply = &off
	}
	// Off until it is shown to help: the fresh-agent test of 2026-11-11
	// found no gain over agentbus-api.
	if p.AgentAPI.Tools == nil {
		off := false
		p.AgentAPI.Tools = &off
	}
}

// Check refuses a value outside its range, in words that name the key. It
// checks the folders' shape only: whether each exists is for the machine
// that runs the turn (Resolve).
func (p *Policy) Check() error {
	if p.Edit != "" && p.Edit != EditSession && p.Edit != EditNone {
		return fmt.Errorf("edit must be %q or %q", EditSession, EditNone)
	}
	switch p.Isolation {
	case "", IsolationAuto, IsolationOverlay, IsolationCopy, IsolationGit, IsolationNone:
	default:
		return fmt.Errorf("isolation must be %q, %q, %q, %q or %q", IsolationAuto, IsolationOverlay, IsolationCopy, IsolationGit, IsolationNone)
	}
	for _, f := range p.ShareReadOnly {
		if f == "" || f == "." || f == ".." || f == ".git" || strings.ContainsAny(f, "/\\") || strings.ContainsFunc(f, isControl) {
			return fmt.Errorf("share_read_only entry %s must be one folder name at the top of the session's folder, such as \"node_modules\"", strconv.Quote(f))
		}
	}
	for _, l := range []struct {
		name string
		v    *int
	}{{"max_depth", p.Limits.MaxDepth}, {"max_jobs_per_turn", p.Limits.MaxJobsPerTurn}, {"max_jobs_per_tree", p.Limits.MaxJobsPerTree}} {
		if l.v != nil && *l.v < 0 {
			return fmt.Errorf("[limits] %s must be 0 or more", l.name)
		}
	}
	if c := p.Limits.MaxCostUSDPerTree; c != nil && (*c < 0 || *c != *c) {
		return errors.New("[limits] max_cost_usd_per_tree must be 0 (off) or more")
	}
	if s := p.AgentAPI.Scope; s != "" && s != ScopeTree && s != ScopeSession {
		return fmt.Errorf("[agent_api] scope must be %q or %q", ScopeTree, ScopeSession)
	}
	switch p.Network.Contain {
	case "", ContainOff, ContainOn:
	default:
		return fmt.Errorf("[network] contain must be %q or %q", ContainOff, ContainOn)
	}
	switch p.Sandbox.Mode {
	case "", SandboxOff, SandboxOn:
	default:
		return fmt.Errorf("[sandbox] mode must be %q or %q", SandboxOff, SandboxOn)
	}
	for _, f := range p.Sandbox.Read {
		if err := grantShape(f); err != nil {
			return fmt.Errorf("[sandbox] read entry %s %v", strconv.Quote(f), err)
		}
	}
	for _, h := range p.Network.Allow {
		if !hostShape(h) {
			return fmt.Errorf("[network] allow entry %s must be a host name, such as \"github.com\" or \"*.github.com\": no scheme, port or path", strconv.Quote(h))
		}
	}
	for _, f := range p.ReadFolders {
		if err := folderShape(f); err != nil {
			return fmt.Errorf("read_folders entry %s %v", strconv.Quote(f), err)
		}
	}
	for _, list := range []struct {
		name string
		cmds []string
	}{{"allow", p.Allow}, {"forbid", p.Forbid}} {
		for _, c := range list.cmds {
			if err := checkCommand(c); err != nil {
				return fmt.Errorf("%s entry %s %v", list.name, strconv.Quote(c), err)
			}
		}
	}
	return nil
}

// ParsePolicy reads a permissions.toml. Empty text is every default. The
// error says what is wrong in words of its own and the line when the parser
// gives one.
func ParsePolicy(b []byte) (Policy, error) {
	p, err := parsePolicy(b)
	if err != nil {
		return Policy{}, err
	}
	p.fill()
	return p, nil
}

// parsePolicy is ParsePolicy with each key left out still unset.
func parsePolicy(b []byte) (Policy, error) {
	var p Policy
	md, err := toml.Decode(string(b), &p)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return Policy{}, fmt.Errorf("line %d: %s", pe.Position.Line, pe.Message)
		}
		// Not a parse error: a value of the wrong type, such as a string
		// where a list belongs.
		return Policy{}, errors.New("a value has the wrong type: read_folders, allow, forbid and share_read_only are lists of strings, such as [\"go test\"]; " +
			"edit, isolation, scope, contain and mode are strings; [network] allow and [sandbox] read are lists of strings; subagents is true or false; the [limits] are numbers; the [agent_api] switches are true or false")
	}
	extra := slices.DeleteFunc(md.Undecoded(), func(k toml.Key) bool { return slices.Contains(retiredKeys, strings.Join(k, ".")) })
	if len(extra) > 0 {
		return Policy{}, fmt.Errorf("unknown key %s; the keys are read_folders, allow, forbid, edit, isolation, share_read_only, subagents, [limits], [agent_api], [network] and [sandbox] (see the README)", strconv.Quote(extra[0].String()))
	}
	if err := p.Check(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// over is p, read with the keys it leaves out unset, on top of base: each key
// p leaves out takes base's value, and the result is filled.
func (p Policy) over(base Policy) (Policy, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return Policy{}, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return Policy{}, err
	}
	set := flat{}
	for k, v := range top {
		var sub map[string]json.RawMessage
		if bytes.HasPrefix(v, []byte("{")) && json.Unmarshal(v, &sub) == nil {
			for sk, sv := range sub {
				if string(sv) != "null" {
					set[k+"."+sk] = sv
				}
			}
			continue
		}
		if string(v) != "null" {
			set[k] = v
		}
	}
	return applyOverlay(base, set)
}

// withoutRetired is the JSON object b with its retiredKeys left out, or b
// itself when it holds none or is not an object (the decoder then says
// what is wrong).
func withoutRetired(b []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return b
	}
	changed := false
	for _, k := range retiredKeys {
		// A key in a table, such as agent_api.continue, is left out of it.
		if t, sub, ok := strings.Cut(k, "."); ok {
			var inner map[string]json.RawMessage
			if raw, found := top[t]; found && json.Unmarshal(raw, &inner) == nil {
				if _, found := inner[sub]; found {
					delete(inner, sub)
					if out, err := json.Marshal(inner); err == nil {
						top[t], changed = out, true
					}
				}
			}
			continue
		}
		if _, found := top[k]; found {
			delete(top, k)
			changed = true
		}
	}
	if !changed {
		return b
	}
	out, err := json.Marshal(top)
	if err != nil {
		return b
	}
	return out
}

// DecodePolicy reads a policy sent as JSON, as PUT
// /api/v0/settings/permissions takes it. A key left out holds its default.
// The error names the key.
func DecodePolicy(b []byte) (Policy, error) {
	var p Policy
	// One object: a bare null would decode as every default.
	if t := bytes.TrimLeft(b, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		return Policy{}, errors.New("the body must be one JSON object: the policy")
	}
	b = withoutRetired(b)
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		var te *json.UnmarshalTypeError
		var se *json.SyntaxError
		switch {
		case errors.As(err, &te) && te.Field != "":
			return Policy{}, fmt.Errorf("%s has the wrong type: read_folders, allow, forbid and share_read_only are lists of strings; "+
				"edit, isolation, scope, contain and mode are strings; network.allow and sandbox.read are lists of strings; subagents and the agent_api switches are true or false; the limits are numbers", te.Field)
		case errors.As(err, &se):
			return Policy{}, errors.New("the body is not valid JSON")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return Policy{}, fmt.Errorf("unknown key %s; the keys are read_folders, allow, forbid, edit, isolation, share_read_only, subagents, limits, agent_api, network and sandbox",
				strings.TrimPrefix(err.Error(), "json: unknown field "))
		}
		return Policy{}, errors.New("the body must be one JSON object: the policy")
	}
	if d.More() {
		return Policy{}, errors.New("the body must be one JSON object: the policy")
	}
	if err := p.Check(); err != nil {
		return Policy{}, err
	}
	p.fill()
	return p, nil
}

// starterLine is a key's line of PolicyStarter: the key, the value and an
// optional comment.
var starterLine = regexp.MustCompile(`^([a-z_]+) = (\S+)( +#.*)?$`)

// FormatPolicy is p as permissions.toml, in PolicyStarter's layout with its
// comments, and each key's value set to p's. The person's own comments in a
// file are not kept. FormatPolicy(DefaultPolicy()) is PolicyStarter.
func FormatPolicy(p Policy) []byte {
	p.fill()
	values := map[string]any{
		"read_folders": p.ReadFolders, "allow": p.Allow, "forbid": p.Forbid, "edit": p.Edit, "isolation": p.Isolation,
		"share_read_only": p.ShareReadOnly, "subagents": *p.Subagents,
		"limits.max_depth": *p.Limits.MaxDepth, "limits.max_jobs_per_turn": *p.Limits.MaxJobsPerTurn,
		"limits.max_jobs_per_tree": *p.Limits.MaxJobsPerTree, "limits.max_cost_usd_per_tree": *p.Limits.MaxCostUSDPerTree,
		"agent_api.scope": p.AgentAPI.Scope, "agent_api.read": *p.AgentAPI.Read, "agent_api.list_agents": *p.AgentAPI.ListAgents,
		"agent_api.start": *p.AgentAPI.Start, "agent_api.review": *p.AgentAPI.Review,
		"agent_api.cancel": *p.AgentAPI.Cancel, "agent_api.rename": *p.AgentAPI.Rename,
		"agent_api.apply": *p.AgentAPI.Apply, "agent_api.tools": *p.AgentAPI.Tools,
		"network.contain": p.Network.Contain, "network.allow": p.Network.Allow,
		"sandbox.mode": p.Sandbox.Mode, "sandbox.read": p.Sandbox.Read,
	}
	var out bytes.Buffer
	section := ""
	sc := bufio.NewScanner(strings.NewReader(PolicyStarter))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]") + "."
		}
		m := starterLine.FindStringSubmatch(line)
		if m == nil {
			out.WriteString(line + "\n")
			continue
		}
		key := m[1]
		if section != "" {
			key = section + m[1]
		}
		lead := m[1] + " = " + tomlValue(values[key])
		if m[3] != "" {
			// The comment stays in its column where the value leaves room.
			col := len(m[1]) + 3 + len(m[2]) + len(m[3]) - len(strings.TrimLeft(m[3], " "))
			pad := max(1, col-len(lead))
			lead += strings.Repeat(" ", pad) + strings.TrimLeft(m[3], " ")
		}
		out.WriteString(lead + "\n")
	}
	return out.Bytes()
}

// tomlValue is v as a TOML value, as the encoder writes it.
func tomlValue(v any) string {
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(map[string]any{"v": v}); err != nil {
		panic("settings: a policy value could not be written as TOML")
	}
	s := strings.TrimSpace(b.String())
	s = strings.TrimPrefix(s, "v = ")
	if _, isFloat := v.(float64); isFloat {
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			s += ".0"
		}
	}
	return s
}

// Resolve is p with each folder absolute and clean, ~ alone or ~/ at the
// start read as home, on the machine that runs the turn. A folder that is
// not there is an Error.
func (p Policy) Resolve(home string) (Policy, error) {
	folders := make([]string, len(p.ReadFolders))
	for i, f := range p.ReadFolders {
		abs, err := resolveFolder(f, home)
		if err != nil {
			return Policy{}, p.error(fmt.Sprintf("read_folders entry %s %v", strconv.Quote(f), err))
		}
		folders[i] = abs
	}
	p.ReadFolders = folders
	return p, nil
}

// error is a problem with p, as an Error from where p was read.
func (p Policy) error(why string) *Error {
	where, fix := p.where, p.fix
	if where == "" {
		where, fix = "the permissions", "Fix them"
	}
	return &Error{Where: where, Why: why, Fix: fix}
}

// folderShape refuses a folder that could never be one: anything relative,
// so no entry can be read as a flag. The path goes inside Claude Code's rules
// (readRules), so a comma or a parenthesis, which would end the rule, and a
// glob character, which would widen it, are refused.
func folderShape(f string) error {
	if f == "" {
		return errors.New("is empty")
	}
	if strings.ContainsFunc(f, isControl) {
		return errors.New("has a control character")
	}
	if strings.ContainsAny(f, ",()*?[]{}") {
		return errors.New("has a comma, a parenthesis or a glob character")
	}
	if f != "~" && !strings.HasPrefix(f, "~/") && !filepath.IsAbs(f) {
		return errors.New("must be an absolute path or start with ~/")
	}
	return nil
}

// grantShape refuses a [sandbox] read entry that is not a path under the
// home folder: it starts with ~/, names something below it, and never
// climbs out with "..".
func grantShape(f string) error {
	if strings.ContainsFunc(f, isControl) {
		return errors.New("has a control character")
	}
	rest, ok := strings.CutPrefix(f, "~/")
	if !ok {
		return errors.New("must start with ~/")
	}
	if c := filepath.Clean(rest); c == "." || c == ".." || strings.HasPrefix(c, "../") || filepath.IsAbs(c) {
		return errors.New("must name a file or folder under ~, not ~ itself or above it")
	}
	return nil
}

// resolveFolder is f, of a good shape, as an absolute, clean path that is an
// existing folder.
func resolveFolder(f, home string) (string, error) {
	if err := folderShape(f); err != nil {
		return "", err
	}
	if f == "~" || strings.HasPrefix(f, "~/") {
		if home == "" {
			return "", errors.New("starts with ~, and the home folder is not known")
		}
		f = filepath.Join(home, f[1:])
	}
	f = filepath.Clean(f)
	if !isDir(f) {
		return "", errors.New("is not an existing folder")
	}
	return f, nil
}

// checkCommand accepts a command and its leading arguments, such as
// "go test". Claude Code's rule is Bash(<command>:*), and its lists are
// comma-joined, so a comma or a parenthesis would change the rule. A star, as
// in "go test:*", or two spaces in a row would make a rule that never matches
// (found in review), so they are refused rather than passed on. A colon is
// fine: the rule ends at the last ":*", so "curl http://localhost:3000" works
// (Claude Code 2.1.319, 2026-11-07, checked live in review).
func checkCommand(c string) error {
	switch {
	case strings.TrimSpace(c) == "":
		return errors.New("is empty")
	case c != strings.TrimSpace(c):
		return errors.New("starts or ends with a space")
	case strings.HasPrefix(c, "-"):
		return errors.New("starts with -")
	case strings.ContainsAny(c, ",()"):
		return errors.New("has a comma or a parenthesis")
	case strings.Contains(c, "*"):
		return errors.New("has a star; write the command alone, such as \"go test\"")
	case strings.Contains(c, "  "):
		return errors.New("has two spaces in a row")
	case strings.ContainsFunc(c, isControl):
		return errors.New("has a control character")
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// hostShape accepts a host name or an IPv4 address, optionally starting
// with "*." for every name under it: letters, digits, dots and dashes, each
// label 1 to 63 characters.
func hostShape(h string) bool {
	h = strings.TrimPrefix(h, "*.")
	if h == "" || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
