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
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// PolicyStarter is what the file store writes when permissions.toml is
// missing. Its lists are empty, and every other value is written out at its
// default, so a new file changes nothing. A key left out of a file reads as
// its default too. FormatPolicy writes a policy in this same layout.
const PolicyStarter = `# What AgentBus turns may do without asking. Read at the start of every turn:
# everything a turn does is decided by it as it was then, and an edit applies
# from the next turn.
# A key left out reads as the default written here. Folders may start with ~.
#
# allow: commands and tools agents may run without asking.
# ask: commands and tools that wait for a person. The gateway holds
# a matching call and records it; a person answers once (run it this turn only),
# always (move it to allow and continue the session) or refuse. forbid wins over
# ask, ask wins over allow.
# forbid: commands and tools agents may never run. The gateway refuses them.
# It reads the command text, so it cannot see a command built at run time or
# run from a script. For a hard rule, also leave out what the command needs.
# The gateway decides every shell command and tool call this way, for claude
# and codex alike: forbid, then ask, then allow. A command or tool on none of
# the lists is held for a person. The default allow list holds "*" (every
# command) and "tool:*" (every tool), so nothing is held by default; narrow it
# to hold or refuse what you choose.

allow = ["*", "tool:*"]
ask = []
forbid = ["tool:subagent"]

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

# home_files: files or folders under your home, such as "~/.aws/config",
# copied into each session's own home at the start of each turn, at the same
# place. Every session has a home of its own, so it sees nothing else of
# yours there. A folder's files go at any depth. A file the session has is
# replaced when the original is newer. ~/.claude.json gives its MCP servers
# only. Vendor sign-in files never go.
home_files = []      # e.g. ["~/.aws/config"]

# carry_files: files of your folder, such as ".env", copied into a session
# that starts from a branch or commit (a git worktree) instead of the folder
# as it is. Such a session has only what git has at that commit, so a file
# git ignores is missing unless named here. Patterns are relative to the
# folder, with * and ? for any name, such as ".env.*". A file the commit
# already has keeps the commit's content. Nothing under .git is copied.
carry_files = []     # e.g. [".env", ".env.*"]

# Work agents ask other agents for. A request tree is the person's turns in one
# session and every turn work asked for from them, at any depth, in whatever
# session it ran. Each turn is in one tree, and the limits count by turn. A
# limit reached refuses the request, or the reply an on would deliver, and
# says which limit.
[limits]
max_depth = 2                # levels of asking below the person's turn
max_jobs_per_turn = 10       # turns and reviews one turn may ask for, ons it registers included
max_jobs_per_tree = 30       # turns and reviews one request tree may ask for in all, ons included
max_cost_usd_per_tree = 0.0  # USD the turns of one request tree may cost before it gets no more; 0 is off
max_delegated_at_once = 8    # turns agents started that one runner runs at once, 1 to 64; more wait.
                             # Turns you send are not counted: they start at once.

# What a turn's own token may call.
[agent_api]
apply = false        # apply a turn's changes to the person's folder, for a turn in its tree;
                     # off by default: only the main token applies

# A sandbox workspace's copy whose work is all applied or discarded is put away
# by itself once the workspace has been quiet this long. A copy with work not
# applied is never touched. The next turn makes a fresh copy. 0 is off.
[workspaces]
put_away_after_minutes = 30
`

// defaultAllow is the allow list of a person who changed nothing: "*" (every
// command) and "tool:*" (every tool), so nothing is held by default. A person
// narrows it to hold or refuse what they choose. A command or tool on none of
// the lists is still held for a person, so emptying allow holds everything.
// defaultForbid refuses subagents, the one thing off by default (his call,
// 2026-11-23).
var defaultAllow = []string{"*", "tool:*"}
var defaultForbid = []string{"tool:subagent"}

// Isolation values: where a new session's turns run.
const (
	IsolationAuto    = "auto"
	IsolationOverlay = "overlay"
	IsolationCopy    = "copy"
	IsolationGit     = "git"
	IsolationNone    = "none"
)

// The defaults, as the starter file writes them.
const (
	DefaultMaxDepth       = 2
	DefaultMaxJobsPerTurn = 10
	DefaultMaxJobsPerTree = 30
	// DefaultMaxDelegatedAtOnce is how many delegated turns (turns agents
	// started) one runner runs at once (his call, 2026-11-26: measured on a
	// 10-core, 16 GB Mac, an agent process takes ~60 MB, so CPU and memory are
	// not the limit). Turns the person sends are not counted.
	DefaultMaxDelegatedAtOnce = 8
	// MaxDelegatedAtOnce bounds the setting, as a runner's capacity is bounded.
	MaxDelegatedAtOnce = 64
	// DefaultPutAwayAfterMinutes is how long a settled workspace stays quiet
	// before its copy is put away (his call, 2026-11-26).
	DefaultPutAwayAfterMinutes = 30
)

// Policy is the policy as written: folders as the person wrote them, ~ and
// all, since they name folders on the machine that runs the turn (Resolve).
// Once read from a store every value is filled: a key left out holds its
// default, and a list is empty, never null.
type Policy struct {
	Allow      []string   `toml:"allow" json:"allow" doc:"Commands and tools agents may run without asking, for claude and codex alike: a command or tool:<name>. Absent in a PUT: default allow rules."`
	Ask        []string   `toml:"ask" json:"ask" doc:"Commands and tools that wait for a person before an agent runs them, for claude and codex alike. The gateway holds a matching call and records it for a person; an \"always\" moves the entry to allow and continues the session so the agent can run it, \"once\" lets it run that continuation turn only, a refusal records the decision. Forbid wins over ask; ask wins over allow."`
	Forbid     []string   `toml:"forbid" json:"forbid" doc:"Commands and tools agents may never run. The gateway refuses them."`
	Isolation  string     `toml:"isolation" json:"isolation,omitempty" doc:"Where a new session's turns run: auto (copy on macOS; overlay on Linux where the runner can mount one, else copy), overlay, copy (a copy of its folder as it is now), git (a git worktree with the folder's uncommitted edits, in a repo) or none (the folder itself). On ext4, copy and the overlay's snapshot copy every byte: use xfs, btrfs, git or none for a big folder. Absent in a PUT: auto."`
	HomeFiles  []string   `toml:"home_files" json:"home_files" doc:"Files or folders under the home folder, each starting with ~/, copied into each session's own home at the same place at the start of each turn, such as ~/.aws/config."`
	CarryFiles []string `toml:"carry_files" json:"carry_files" doc:"Files of the person's folder, as patterns relative to it with * and ? (such as .env or .env.*), copied into a workspace started from a base (a git worktree at a branch or commit) when it is made, where the commit has nothing at that path. Nothing under .git is copied. Absent in a PUT: none."`
	Limits     Limits     `toml:"limits" json:"limits" doc:"The limits on work agents ask for."`
	AgentAPI   AgentAPI   `toml:"agent_api" json:"agent_api" doc:"What a turn's own token may call."`
	Workspaces Workspaces `toml:"workspaces" json:"workspaces" doc:"How AgentBus looks after sandbox workspaces' copies."`

	// where and fix say, in a turn's refusal, where the policy came from and
	// what to do: set by the store that read it (Error).
	where, fix string

	legacyTools     *bool
	legacySubagents *bool
	legacyEdit      *string
}

// AgentAPI says what a turn token may call ([agent_api]). A pointer is a key
// left out; once read from a store each is set. Within its authority a turn
// token may always read, list agents, start, review, stop and rename (0.5.2
// retired the switches for those: upgradeSteps).
type AgentAPI struct {
	Apply *bool `toml:"apply" json:"apply" doc:"Apply a turn's changes to the person's folder, for a turn token in its own workspace or one its session made. Off by default: only the main token applies. Absent in a PUT: false."`
}

// Workspaces says how AgentBus looks after sandbox workspaces' copies
// ([workspaces]). A pointer is a key left out; once read from a store each is
// set.
type Workspaces struct {
	PutAwayAfterMinutes *int `toml:"put_away_after_minutes" json:"put_away_after_minutes,omitempty" doc:"Minutes a sandbox workspace whose work is all applied or discarded stays quiet before AgentBus puts its copy away by itself. A copy with work not applied is never touched; the next turn makes a fresh copy. 0 is off. Absent in a PUT: 30."`
}

// DelegatedAtOnce is how many delegated turns one runner runs at once
// ([limits] max_delegated_at_once).
func (p Policy) DelegatedAtOnce() int {
	if v := p.Limits.MaxDelegatedAtOnce; v != nil {
		return *v
	}
	return DefaultMaxDelegatedAtOnce
}

// PutAwayAfter is how long a settled workspace stays quiet before its copy is
// put away; false when putting copies away is off.
func (p Policy) PutAwayAfter() (time.Duration, bool) {
	m := DefaultPutAwayAfterMinutes
	if v := p.Workspaces.PutAwayAfterMinutes; v != nil {
		m = *v
	}
	return time.Duration(m) * time.Minute, m > 0
}

// Limits are the caps on work agents ask for ([limits]). A pointer is a key
// left out; once read from a store each is set.
type Limits struct {
	MaxDepth           *int     `toml:"max_depth" json:"max_depth,omitempty" doc:"Levels of asking below the person's turn. Absent in a PUT: 2."`
	MaxJobsPerTurn     *int     `toml:"max_jobs_per_turn" json:"max_jobs_per_turn,omitempty" doc:"Turns and reviews one turn may ask for. An on the turn registers with POST /api/v0/ons counts as one. Absent in a PUT: 10."`
	MaxJobsPerTree     *int     `toml:"max_jobs_per_tree" json:"max_jobs_per_tree,omitempty" doc:"Turns and reviews one request tree may ask for in all. An on a turn of the tree registers with POST /api/v0/ons counts as one. Absent in a PUT: 30."`
	MaxDelegatedAtOnce *int     `toml:"max_delegated_at_once" json:"max_delegated_at_once,omitempty" doc:"Delegated turns one runner runs at once: turns an agent started, and the replies delivered into sessions agents started. More wait in the queue, oldest first. Turns the person sends, and replies delivered into the person's own sessions, are not counted and start at once. A change holds for the next turn a runner takes, with no restart. 1 to 64. Absent in a PUT: 8."`
	MaxCostUSDPerTree  *float64 `toml:"max_cost_usd_per_tree" json:"max_cost_usd_per_tree,omitempty" doc:"USD the turns of one request tree may cost in all before it gets no more turns: no asked work, and no reply an on delivers. Only the tree's own turns count, wherever they ran. 0 is off. Absent in a PUT: 0."`
}

// DefaultPolicy is the policy of a missing or empty file.
func DefaultPolicy() Policy {
	var p Policy
	p.fill()
	return p
}

// fill sets every value left out to its default.
func (p *Policy) fill() {
	if p.Allow == nil {
		p.Allow = append([]string(nil), defaultAllow...)
	}
	if p.Forbid == nil {
		if !slices.Contains(p.Allow, "tool:subagent") {
			p.Forbid = append([]string(nil), defaultForbid...)
		} else {
			p.Forbid = []string{}
		}
	}
	if p.Ask == nil {
		p.Ask = []string{}
	}
	if p.HomeFiles == nil {
		p.HomeFiles = []string{}
	}
	if p.CarryFiles == nil {
		p.CarryFiles = []string{}
	}
	if p.Isolation == "" {
		p.Isolation = IsolationAuto
	}
	setInt := func(v **int, d int) {
		if *v == nil {
			*v = &d
		}
	}
	setInt(&p.Limits.MaxDepth, DefaultMaxDepth)
	setInt(&p.Limits.MaxJobsPerTurn, DefaultMaxJobsPerTurn)
	setInt(&p.Limits.MaxJobsPerTree, DefaultMaxJobsPerTree)
	setInt(&p.Limits.MaxDelegatedAtOnce, DefaultMaxDelegatedAtOnce)
	setInt(&p.Workspaces.PutAwayAfterMinutes, DefaultPutAwayAfterMinutes)
	if p.Limits.MaxCostUSDPerTree == nil {
		off := 0.0
		p.Limits.MaxCostUSDPerTree = &off
	}
	if p.AgentAPI.Apply == nil {
		off := false
		p.AgentAPI.Apply = &off
	}
}

// Check refuses a value outside its range, in words that name the key. It
// checks the folders' shape only: whether each exists is for the machine
// that runs the turn (Resolve).
func (p *Policy) Check() error {
	switch p.Isolation {
	case "", IsolationAuto, IsolationOverlay, IsolationCopy, IsolationGit, IsolationNone:
	default:
		return fmt.Errorf("isolation must be %q, %q, %q, %q or %q", IsolationAuto, IsolationOverlay, IsolationCopy, IsolationGit, IsolationNone)
	}
	for _, l := range []struct {
		name string
		v    *int
	}{{"max_depth", p.Limits.MaxDepth}, {"max_jobs_per_turn", p.Limits.MaxJobsPerTurn}, {"max_jobs_per_tree", p.Limits.MaxJobsPerTree}} {
		if l.v != nil && *l.v < 0 {
			return fmt.Errorf("[limits] %s must be 0 or more", l.name)
		}
	}
	if v := p.Limits.MaxDelegatedAtOnce; v != nil && (*v < 1 || *v > MaxDelegatedAtOnce) {
		return fmt.Errorf("[limits] max_delegated_at_once must be 1 to %d", MaxDelegatedAtOnce)
	}
	if v := p.Workspaces.PutAwayAfterMinutes; v != nil && *v < 0 {
		return errors.New("[workspaces] put_away_after_minutes must be 0 (off) or more")
	}
	if c := p.Limits.MaxCostUSDPerTree; c != nil && (*c < 0 || *c != *c) {
		return errors.New("[limits] max_cost_usd_per_tree must be 0 (off) or more")
	}
	for _, f := range p.HomeFiles {
		if err := grantShape(f); err != nil {
			return fmt.Errorf("home_files entry %s %v", strconv.Quote(f), err)
		}
		if what := SignInFile(f); what != "" {
			return fmt.Errorf("home_files entry %s is %s; vendor sign-ins stay with control and never go into a session's home", strconv.Quote(f), what)
		}
	}
	for _, f := range p.CarryFiles {
		if err := carryShape(f); err != nil {
			return fmt.Errorf("carry_files entry %s %v", strconv.Quote(f), err)
		}
	}
	for _, list := range []struct {
		name string
		cmds []string
	}{{"allow", p.Allow}, {"ask", p.Ask}, {"forbid", p.Forbid}} {
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
		return Policy{}, errors.New(wrongTypeTOML)
	}
	if err := movedOver(&p, b, md); err != nil {
		return Policy{}, err
	}
	extra := slices.DeleteFunc(md.Undecoded(), func(k toml.Key) bool {
		if slices.Contains(retiredKeys, strings.Join(k, ".")) {
			return true
		}
		if len(k) == 1 {
			for _, rk := range retiredKeys {
				if t, _, ok := strings.Cut(rk, "."); ok && t == k[0] {
					return true
				}
			}
		}
		return false
	})
	if len(extra) > 0 {
		return Policy{}, fmt.Errorf("unknown key %s; the keys are allow, ask, forbid, isolation, home_files, carry_files, [limits], [agent_api] and [workspaces] (see the README)", strconv.Quote(extra[0].String()))
	}
	if err := p.Check(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// wrongTypeTOML is a permissions.toml's error for a value of the wrong type.
const wrongTypeTOML = "a value has the wrong type: allow, ask, forbid, home_files and carry_files are lists of strings, such as [\"go test\"]; " +
	"isolation is a string; the [limits] are numbers; the [agent_api] switches are true or false"

// over is p, read with the keys it leaves out unset, on top of base: each key
// p leaves out takes base's value, and the result is filled.
func (p Policy) over(base Policy) (Policy, error) {
	set, err := p.setKeys()
	if err != nil {
		return Policy{}, err
	}
	res, err := applyOverlay(base, set)
	if err != nil {
		return Policy{}, err
	}
	res.applyLegacy(p.legacyTools, p.legacySubagents, p.legacyEdit)
	return res, nil
}

// setKeys are the keys p sets, by dotted name, read with the keys it leaves
// out unset: a key left out is not among them.
func (p Policy) setKeys() (flat, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, err
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
	return set, nil
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
					if len(inner) == 0 {
						delete(top, t)
						changed = true
					} else if out, err := json.Marshal(inner); err == nil {
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
	p, err := DecodePolicyChange(b)
	if err != nil {
		return Policy{}, err
	}
	p.fill()
	p.applyLegacy(p.legacyTools, p.legacySubagents, p.legacyEdit)
	return p, nil
}

func (p *Policy) applyLegacy(tools *bool, subagents *bool, edit *string) {
	if tools != nil {
		if *tools {
			p.Allow = addRule(p.Allow, "tool:*")
			p.Forbid = removeRule(p.Forbid, "tool:*")
		} else {
			// Tools off preserves whatever edit permission there was: if
			// editing was allowed (its own rule, or under tool:*), it stays
			// allowed after tool:* goes, so turning tools off with the legacy
			// switch does not silently take edit away (the old default allowed
			// it) nor silently grant it where it was not allowed. An explicit
			// edit below still wins.
			editWasAllowed := slices.Contains(p.Allow, "tool:edit") || slices.Contains(p.Allow, "tool:*")
			p.Forbid = addRule(p.Forbid, "tool:*")
			p.Allow = removeRule(p.Allow, "tool:*")
			if edit == nil && editWasAllowed {
				p.Allow = addRule(p.Allow, "tool:edit")
			}
		}
	}
	if subagents != nil {
		if *subagents {
			p.Allow = addRule(p.Allow, "tool:subagent")
			p.Forbid = removeRule(p.Forbid, "tool:subagent")
		} else {
			p.Forbid = addRule(p.Forbid, "tool:subagent")
			p.Allow = removeRule(p.Allow, "tool:subagent")
		}
	}
	if edit != nil {
		if *edit == "session" {
			p.Allow = addRule(p.Allow, "tool:edit")
			p.Ask = removeRule(p.Ask, "tool:edit")
			p.Forbid = removeRule(p.Forbid, "tool:edit")
		} else if *edit == "none" {
			p.Ask = addRule(p.Ask, "tool:edit")
			p.Allow = removeRule(p.Allow, "tool:edit")
		}
	}
}

// DecodePolicyChange reads a change of the policy sent as JSON, as PATCH
// /api/v0/settings/permissions takes it: each key it leaves out stays unset,
// and each key it holds is checked. The error names the key.
func DecodePolicyChange(b []byte) (Policy, error) {
	var p Policy
	// One object: a bare null would decode as every default.
	if t := bytes.TrimLeft(b, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		return Policy{}, errors.New("the body must be one JSON object: the policy")
	}
	moved, err := withMoved(b)
	if err != nil {
		return Policy{}, err
	}
	legTools, legSubagents, legEdit, err := extractLegacySwitches(moved)
	if err != nil {
		return Policy{}, err
	}
	b = withoutRetired(moved)
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		var te *json.UnmarshalTypeError
		var se *json.SyntaxError
		switch {
		case errors.As(err, &te) && te.Field != "":
			return Policy{}, fmt.Errorf("%s has the wrong type: allow, ask, forbid, home_files and carry_files are lists of strings; "+
				"isolation is a string; the limits are numbers", te.Field)
		case errors.As(err, &se):
			return Policy{}, errors.New("the body is not valid JSON")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return Policy{}, fmt.Errorf("unknown key %s; the keys are allow, ask, forbid, isolation, home_files, carry_files, limits, agent_api and workspaces",
				strings.TrimPrefix(err.Error(), "json: unknown field "))
		}
		return Policy{}, errors.New("the body must be one JSON object: the policy")
	}
	if d.More() {
		return Policy{}, errors.New("the body must be one JSON object: the policy")
	}
	p.legacyTools = legTools
	p.legacySubagents = legSubagents
	p.legacyEdit = legEdit
	if err := p.Check(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// starterLine is a key's line of PolicyStarter: the key, the value and an
// optional comment.
var starterLine = regexp.MustCompile(`^([a-z_]+) = (.*?)((?: {2,}|\t+)#.*)?$`)

// FormatPolicy is p as permissions.toml, in PolicyStarter's layout with its
// comments, and each key's value set to p's. The person's own comments in a
// file are not kept. FormatPolicy(DefaultPolicy()) is PolicyStarter.
func FormatPolicy(p Policy) []byte {
	p.fill()
	values := map[string]any{
		"allow": p.Allow, "ask": p.Ask, "forbid": p.Forbid, "isolation": p.Isolation,
		"home_files": p.HomeFiles, "carry_files": p.CarryFiles,
		"limits.max_depth": *p.Limits.MaxDepth, "limits.max_jobs_per_turn": *p.Limits.MaxJobsPerTurn,
		"limits.max_jobs_per_tree": *p.Limits.MaxJobsPerTree, "limits.max_cost_usd_per_tree": *p.Limits.MaxCostUSDPerTree,
		"limits.max_delegated_at_once":      *p.Limits.MaxDelegatedAtOnce,
		"agent_api.apply":                   *p.AgentAPI.Apply,
		"workspaces.put_away_after_minutes": *p.Workspaces.PutAwayAfterMinutes,
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

// Resolve is p ready on the machine that runs the turn.
func (p Policy) Resolve(home string) (Policy, error) {
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

// grantShape refuses a home_files entry that is not a path under the
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

// carryShape refuses a carry_files entry that is not a pattern of files
// inside the folder: relative, slash-separated, never climbing out with ".."
// and never naming .git.
func carryShape(f string) error {
	if strings.ContainsFunc(f, isControl) {
		return errors.New("has a control character")
	}
	if f == "" || strings.HasPrefix(f, "/") || strings.HasPrefix(f, "~") || strings.Contains(f, "\\") || filepath.IsAbs(f) {
		return errors.New("must be a path relative to the folder, with / between names")
	}
	for _, part := range strings.Split(f, "/") {
		switch part {
		case "", ".", "..":
			return errors.New("must name files inside the folder: no empty name, . or ..")
		case ".git":
			return errors.New("names .git; a repo's own files are never copied")
		}
	}
	if _, err := path.Match(f, ""); err != nil {
		return errors.New("is not a valid pattern")
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
// "go test", or a tool rule ("tool:*", "tool:edit"). Claude Code's rule is Bash(<command>:*), and its lists are
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
	case strings.ContainsFunc(c, isControl):
		return errors.New("has a control character")
	case strings.ContainsAny(c, ",()"):
		return errors.New("has a comma or a parenthesis")
	case strings.Contains(c, "  "):
		return errors.New("has two spaces in a row")
	}
	if strings.HasPrefix(c, "tool:") {
		name := strings.TrimPrefix(c, "tool:")
		if name == "" {
			return errors.New("is empty")
		}
		if strings.HasPrefix(name, "-") {
			return errors.New("starts with -")
		}
		if name != "*" && strings.Contains(name, "*") {
			return errors.New("has a star; write tool:* or tool:<name>")
		}
		if strings.Contains(name, " ") {
			return errors.New("tool name cannot contain spaces")
		}
		return nil
	}
	if c == "*" {
		return nil // every command
	}
	switch {
	case strings.HasPrefix(c, "-"):
		return errors.New("starts with -")
	case strings.Contains(c, "*"):
		return errors.New("has a star; write * for every command, or the command alone, such as \"go test\"")
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
