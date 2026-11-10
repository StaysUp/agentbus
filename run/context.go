package run

// What every turn is given besides the person's words, whatever its harness:
// the folder's AGENTS.md, the instructions, and for an asked agent who asked
// it. Each goes through the harness's own instruction flag (harness.Turn.Hint),
// never into the prompt. A file the harness reads by itself is not given
// again (harness.Harness.Native).
//
// And where it runs: with Config.Homes set, each session's turns run in a
// home of their own, made empty at the session's first turn and kept for its
// next ones, so a turn sees nothing of the runner user's home or of another
// session's. A session started before homes were on keeps the service's own.

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/staysup/agentbus/harness"
)

// FolderDoc is the file of a turn's folder every turn is given.
const FolderDoc = "AGENTS.md"

// MaxFolderDoc bounds what a turn is given of FolderDoc, in bytes. The rest
// stays in the file, and the turn is told so.
const MaxFolderDoc = 32 << 10

// folderDoc is the folder's AGENTS.md as text for a turn of h, or "" when the
// folder has none or h reads it by itself. Only the folder is read: a symlink
// that leads out of it gives nothing. why, for the service log, says why a
// file that is there was left out.
func folderDoc(h harness.Harness, folder string) (text, why string) {
	root, err := os.OpenRoot(folder)
	if err != nil {
		return "", ""
	}
	defer root.Close()
	if _, err := root.Stat(FolderDoc); err != nil {
		return "", ""
	}
	// Never a wait: a named pipe there is refused, not read.
	f, err := harness.OpenFile(root, FolderDoc)
	if err != nil {
		return "", "not_a_file"
	}
	defer f.Close()
	if h.Native(root, FolderDoc) {
		return "", ""
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFolderDoc+1))
	if err != nil {
		return "", "unreadable"
	}
	cut := len(b) > MaxFolderDoc
	if cut {
		b = b[:MaxFolderDoc]
	}
	// It goes on a command line: no NUL byte, and UTF-8 only, which also
	// drops a character the cut split.
	text = strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(string(b), ""), "\x00", ""))
	if text == "" {
		return "", ""
	}
	text = "The " + FolderDoc + " of your working folder:\n\n" + text
	if cut {
		text += "\n\n(" + FolderDoc + " goes on past 32 KiB; read the file for the rest.)"
	}
	return text, ""
}

// joinText joins the parts that are not empty, with a blank line between.
func joinText(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "\n\n")
}

// homeVars are the variables that point a program at a folder of the
// runner user's home. A turn in a home of its own goes without them, so each
// falls back under its HOME.
var homeVars = []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"}

// home returns env with a turn of h pointed at session's home: HOME, and h's
// own config variable at its config folder there (harness.Info.ConfigEnv).
// A session's first turn makes the home (first); made says this call made
// it, so a turn that then does not start removes it. A next turn of a session
// that has no home, one started before homes were on, gets env as it is: its
// vendor's transcript is in the service's own home, and its resume needs it.
func (e *Engine) home(session string, first bool, h harness.Harness, env []string) (out []string, made bool, err error) {
	dir := filepath.Join(e.cfg.Homes, session)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if !first {
			return env, false, nil
		}
		made = true
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, made, err
	}
	set := []string{"HOME=" + dir}
	if i := h.Info(); i.ConfigEnv != "" {
		conf := filepath.Join(dir, i.ConfigDir)
		if err := os.MkdirAll(conf, 0o700); err != nil {
			return nil, made, err
		}
		set = append(set, i.ConfigEnv+"="+conf)
	}
	out = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(homeVars, k)
	})
	return setEnv(out, set), made, nil
}

// RemoveHome removes session's home, with everything its turns left in it.
// Call it when the session is removed. A service without homes, or a session
// that has none, has nothing to remove.
func (e *Engine) RemoveHome(session string) error {
	if e.cfg.Homes == "" || !SessionShape.MatchString(session) {
		return nil
	}
	dir := filepath.Join(e.cfg.Homes, session)
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	// A read-only folder, as a module cache a turn filled has, keeps what
	// it holds until it may be written.
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
