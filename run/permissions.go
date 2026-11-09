package run

// permissions.toml, in the data folder, says what a turn may do without a
// person to ask: folders it may read beyond its own, commands it may run, and
// commands it may never run. It is read at the start of every turn, so an edit
// applies to the next turn without a restart. A missing file is empty lists,
// which change nothing. A broken file stops the turn: a turn never runs
// without the policy the person wrote. docs/CLIENT_API.md says what each list
// becomes for each agent.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/staysup/agentbus/harness"
)

// PermissionsFile is the file's name in the data folder.
const PermissionsFile = "permissions.toml"

// PermissionsStarter is what `agentbus up` writes when the file is missing.
// Its lists are empty, and every other value is written out at its default,
// so a new file changes nothing. A key left out of a file reads as its
// default too.
const PermissionsStarter = `# What AgentBus turns may do without asking. Read at the start of every turn,
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
max_jobs_per_turn = 10       # jobs one turn may ask for
max_jobs_per_tree = 30       # jobs one request tree may ask for in all
max_cost_usd_per_tree = 0.0  # USD one request tree may cost before asking stops; 0 is off

# What a turn's own token (AGENTBUS_TOKEN, used by agentbus-api) may do.
# No setting here lets it change this file, the instructions, saved agents or
# tokens: those take the main token, always.
[agent_api]
scope = "tree"       # whose sessions it reads, continues and reviews:
                     # "tree" (its request tree) or "session" (its own only)
read = true          # read sessions, turns, calls and jobs in its scope
list_agents = true   # list saved agents and harnesses
start = true         # start a new session on a saved agent
continue = true      # continue a session its tree started (never its own or the person's)
review = true        # ask for a review of a job in its scope
cancel = true        # cancel jobs and stop turns its tree asked for
`

// Edit values: where a turn may edit files without asking.
const (
	EditSession = "session"
	EditNone    = "none"
)

// Scope values of [agent_api]: the sessions a turn token reaches.
const (
	ScopeTree    = "tree"
	ScopeSession = "session"
)

// Permissions are the lists of permissions.toml, the edit rule, the limits
// on asked work and what a turn token may call. Folders are absolute and
// clean once loaded, and every value is filled: a key the file leaves out
// holds its default.
type Permissions struct {
	ReadFolders []string `toml:"read_folders"`
	Allow       []string `toml:"allow"`
	Forbid      []string `toml:"forbid"`
	Edit        string   `toml:"edit"`
	Subagents   *bool    `toml:"subagents"`
	Limits      Limits   `toml:"limits"`
	AgentAPI    AgentAPI `toml:"agent_api"`
}

// Limits are the caps on work agents ask for ([limits]). A pointer is a key
// the file may leave out; once loaded each is set.
type Limits struct {
	MaxDepth          *int     `toml:"max_depth"`
	MaxJobsPerTurn    *int     `toml:"max_jobs_per_turn"`
	MaxJobsPerTree    *int     `toml:"max_jobs_per_tree"`
	MaxCostUSDPerTree *float64 `toml:"max_cost_usd_per_tree"`
}

// AgentAPI says what a turn token may call ([agent_api]). A pointer is a key
// the file may leave out; once loaded each is set.
type AgentAPI struct {
	Scope      string `toml:"scope"`
	Read       *bool  `toml:"read"`
	ListAgents *bool  `toml:"list_agents"`
	Start      *bool  `toml:"start"`
	Continue   *bool  `toml:"continue"`
	Review     *bool  `toml:"review"`
	Cancel     *bool  `toml:"cancel"`
}

// The defaults, as the starter file writes them.
const (
	DefaultMaxDepth       = 2
	DefaultMaxJobsPerTurn = 10
	DefaultMaxJobsPerTree = 30
)

// DefaultPermissions are the values of a missing or empty file.
func DefaultPermissions() Permissions {
	var p Permissions
	p.fill()
	return p
}

// fill sets every value the file left out to its default.
func (p *Permissions) fill() {
	if p.Edit == "" {
		p.Edit = EditSession
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
	for _, b := range []**bool{&p.AgentAPI.Read, &p.AgentAPI.ListAgents, &p.AgentAPI.Start,
		&p.AgentAPI.Continue, &p.AgentAPI.Review, &p.AgentAPI.Cancel} {
		if *b == nil {
			on := true
			*b = &on
		}
	}
}

// check refuses a value outside its range, in words of its own.
func (p *Permissions) check() error {
	if p.Edit != "" && p.Edit != EditSession && p.Edit != EditNone {
		return fmt.Errorf("edit must be %q or %q", EditSession, EditNone)
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
	return nil
}

// LoadPermissions reads permissions.toml at path for one request, with ~ as
// the service's home folder. "" is no file: every default. The error says
// what is wrong; it never names the path.
func LoadPermissions(path string) (Permissions, error) {
	if path == "" {
		return DefaultPermissions(), nil
	}
	home, _ := os.UserHomeDir()
	return loadPermissions(path, home)
}

// permissions reads the service's permissions.toml for one turn, with ~ as
// the service's home folder.
func (e *Engine) permissions() (Permissions, error) {
	return LoadPermissions(e.cfg.Permissions)
}

// loadPermissions reads the file at path, with ~ in a folder read as home.
// A missing file is every default. The error says what is wrong in words of
// its own and the line when the parser gives one; it never names the path,
// which the caller adds where it may.
func loadPermissions(path, home string) (Permissions, error) {
	var p Permissions
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A symlink whose target is gone is not a missing file: the person
		// meant a policy, and it is not there (found in review).
		if _, lerr := os.Lstat(path); lerr == nil {
			return Permissions{}, errors.New("it is a link to a file that does not exist")
		}
		return DefaultPermissions(), nil
	}
	if err != nil {
		return Permissions{}, errors.New("it could not be read")
	}
	md, err := toml.Decode(string(b), &p)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return Permissions{}, fmt.Errorf("line %d: %s", pe.Position.Line, pe.Message)
		}
		// Not a parse error: a value of the wrong type, such as a string
		// where a list belongs.
		return Permissions{}, errors.New("a value has the wrong type: read_folders, allow and forbid are lists of strings, such as [\"go test\"]; " +
			"edit and scope are strings; subagents is true or false; the [limits] are numbers; the [agent_api] switches are true or false")
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		return Permissions{}, fmt.Errorf("unknown key %s; the keys are read_folders, allow, forbid, edit, subagents, [limits] and [agent_api] (see the README)", strconv.Quote(extra[0].String()))
	}
	if err := p.check(); err != nil {
		return Permissions{}, err
	}
	p.fill()
	for i, f := range p.ReadFolders {
		abs, err := permissionFolder(f, home)
		if err != nil {
			return Permissions{}, fmt.Errorf("read_folders entry %s %v", strconv.Quote(f), err)
		}
		p.ReadFolders[i] = abs
	}
	for _, list := range []struct {
		name string
		cmds []string
	}{{"allow", p.Allow}, {"forbid", p.Forbid}} {
		for _, c := range list.cmds {
			if err := permissionCommand(c); err != nil {
				return Permissions{}, fmt.Errorf("%s entry %s %v", list.name, strconv.Quote(c), err)
			}
		}
	}
	return p, nil
}

// permissionFolder is f as an absolute, clean path: ~ alone or ~/ at the start
// is home. Anything relative is refused, so no entry can be read as a flag.
// The path goes inside Claude Code's rules (readRules), so a comma or a
// parenthesis, which would end the rule, and a glob character, which would
// widen it, are refused.
func permissionFolder(f, home string) (string, error) {
	if f == "" {
		return "", errors.New("is empty")
	}
	if strings.ContainsFunc(f, isControl) {
		return "", errors.New("has a control character")
	}
	if strings.ContainsAny(f, ",()*?[]{}") {
		return "", errors.New("has a comma, a parenthesis or a glob character")
	}
	if f == "~" || strings.HasPrefix(f, "~/") {
		if home == "" {
			return "", errors.New("starts with ~, and the home folder is not known")
		}
		f = filepath.Join(home, f[1:])
	}
	if !filepath.IsAbs(f) {
		return "", errors.New("must be an absolute path or start with ~/")
	}
	f = filepath.Clean(f)
	if !isDir(f) {
		return "", errors.New("is not an existing folder")
	}
	return f, nil
}

// permissionCommand accepts a command and its leading arguments, such as
// "go test". Claude Code's rule is Bash(<command>:*), and its lists are
// comma-joined, so a comma or a parenthesis would change the rule. A star, as
// in "go test:*", or two spaces in a row would make a rule that never matches
// (found in review), so they are refused rather than passed on. A colon is
// fine: the rule ends at the last ":*", so "curl http://localhost:3000" works
// (Claude Code 2.1.319, 2026-11-07, checked live in review).
func permissionCommand(c string) error {
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

// Writes is what one turn may change (harness.Writes).
type Writes = harness.Writes

// writesFor is what a turn of h may change in folder under perm. The harness
// decides which edit folders its flags can hold (harness.Harness.Edit): a
// folder Claude Code's rules cannot hold gets no edit rule, and codex, which
// cannot keep one file of its folder out of its sandbox, runs read-only in a
// folder holding a protected file. why says, for the service log, why edits
// were left out.
func writesFor(h harness.Harness, folder string, readOnly bool, perm Permissions, protect []string) (w Writes, why string) {
	w.ReadOnly = readOnly
	if readOnly {
		return w, ""
	}
	for _, p := range protect {
		w.Protect = appendReal(w.Protect, p)
	}
	if perm.Edit != EditSession {
		return w, ""
	}
	w.Edit, why = h.Edit(appendReal(nil, folder), w.Protect)
	return w, why
}

// appendReal appends p, absolute and clean, and its real path when that
// differs, as a folder reached through a symlink (/tmp on macOS) has two
// names. A file that does not exist yet gets its folder's real path.
func appendReal(list []string, p string) []string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	list = append(list, p)
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		if d, derr := filepath.EvalSymlinks(filepath.Dir(p)); derr == nil {
			r, err = filepath.Join(d, filepath.Base(p)), nil
		}
	}
	if err == nil && r != p {
		list = append(list, r)
	}
	return list
}
