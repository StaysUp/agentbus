package api

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
)

// PermissionsFile is the file's name in the data folder.
const PermissionsFile = "permissions.toml"

// PermissionsStarter is what `agentbus up` writes when the file is missing.
// Its lists are empty, so a new file changes nothing.
const PermissionsStarter = `# What AgentBus turns may do without asking. Read at the start of every turn.
# Empty lists change nothing. Folders may start with ~ for your home folder.
#
# read_folders: folders claude may read and search beyond its own folder, never write.
# allow: commands claude may run without asking (a command and its arguments).
# forbid: commands claude may never run.
# codex can read every folder already, and has no command lists yet.

read_folders = []   # e.g. ["~/.agents", "~/Sites/agentbus-notes"]
allow = []          # e.g. ["go test", "make test", "git commit"]
forbid = []         # e.g. ["git push", "git checkout"]
`

// Permissions are the three lists of permissions.toml. Folders are absolute
// and clean once loaded.
type Permissions struct {
	ReadFolders []string `toml:"read_folders"`
	Allow       []string `toml:"allow"`
	Forbid      []string `toml:"forbid"`
}

// permissions reads the service's permissions.toml for one turn, with ~ as
// the service's home folder.
func (a *API) permissions() (Permissions, error) {
	if a.cfg.Permissions == "" {
		return Permissions{}, nil
	}
	home, _ := os.UserHomeDir()
	return loadPermissions(a.cfg.Permissions, home)
}

// loadPermissions reads the file at path, with ~ in a folder read as home.
// A missing file is empty lists. The error says what is wrong in words of its
// own and the line when the parser gives one; it never names the path, which
// the caller adds where it may.
func loadPermissions(path, home string) (Permissions, error) {
	var p Permissions
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A symlink whose target is gone is not a missing file: the person
		// meant a policy, and it is not there (found in review).
		if _, lerr := os.Lstat(path); lerr == nil {
			return p, errors.New("it is a link to a file that does not exist")
		}
		return p, nil
	}
	if err != nil {
		return p, errors.New("it could not be read")
	}
	md, err := toml.Decode(string(b), &p)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return Permissions{}, fmt.Errorf("line %d: %s", pe.Position.Line, pe.Message)
		}
		// Not a parse error: a value of the wrong type, such as a string
		// where a list belongs.
		return Permissions{}, errors.New("read_folders, allow and forbid must each be a list of strings, such as [\"go test\"]")
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		return Permissions{}, fmt.Errorf("unknown key %s; the keys are read_folders, allow and forbid", strconv.Quote(extra[0].String()))
	}
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

// readRules is Claude Code's rules that let Read, Grep and Glob reach each
// folder and everything under it without asking: Read(//<folder>/**), with
// "//" marking an absolute path. They grant reading only. --add-dir is not
// used: it makes the folder a working folder, which under the person's
// acceptEdits mode lets a turn write there too (Claude Code 2.1.319,
// 2026-11-07, checked live, found in review). A Read rule alone does not
// cover Grep, so each tool gets its own.
func readRules(folders []string) []string {
	var rules []string
	for _, f := range folders {
		p := "/" + strings.TrimSuffix(f, "/") + "/**"
		rules = append(rules, "Read("+p+")", "Grep("+p+")", "Glob("+p+")")
	}
	return rules
}

// bashRules is Claude Code's rule for each command: Bash(<command>:*), the
// command with any arguments after it, comma-joined into one value.
func bashRules(cmds []string) string {
	rules := make([]string, len(cmds))
	for i, c := range cmds {
		rules[i] = "Bash(" + c + ":*)"
	}
	return strings.Join(rules, ",")
}
