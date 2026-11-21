// Package sandbox builds a wall around one session's agent: the whole
// harness process and everything it starts. Inside it a
// session reads and writes its own folder (its copy) and its own home, reads
// the system folders and the paths it is given (the agent's own program),
// and reaches the network only through the runner's proxy. Nothing else of
// the person's home, and no other session.
//
// One Spec, an adapter per OS:
//
//   - macOS: Seatbelt, /usr/bin/sandbox-exec with a deny-by-default profile.
//     The proxy listens on this machine's loopback (netjail.NewHost), and
//     the profile lets the agent connect only to it and to the forwarded
//     loopback ports (the gateway, the local API).
//   - Linux: bubblewrap, a new mount view with an empty root and only the
//     paths above bound in, run inside netjail's network namespace.
//
// A proof of concept, not a hardened wall. It leaves open: file metadata
// on macOS (a session can learn a file exists), the copy's .git (writable),
// renames of the walled folders, Landlock on Linux, and the browser bridge.
// It hides the person's secret folders (secretPaths) and any .env outside
// the copy even inside a Read grant on macOS; on Linux an ungranted secret
// is invisible (nothing binds it in), but one that a Read grant DOES bind in
// is not yet hidden there -- a known gap, left to a later step (TODO.md).
// Every error is fixed wording,
// safe to show, and New fails before anything starts: a session that asks
// for the wall never runs without it.
package sandbox

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/staysup/agentbus/netjail"
)

// Spec is what one session may reach.
type Spec struct {
	// Copy is the session's folder, read and write. The agent starts there.
	Copy string
	// ReadOnly keeps Copy read only.
	ReadOnly bool
	// Home is the session's own home, read and write. Its tmp folder is the
	// agent's TMPDIR.
	Home string
	// Read are files and folders the agent may read and run, such as its
	// own program. A symlink is followed: the link and what it names are
	// both readable.
	Read []string
	// Net is what the agent may reach: Forward are loopback addresses it
	// reaches as they are, and the proxy lets through Pass, Allow, or every
	// host when Open.
	Net netjail.Spec
}

// Error is why a wall could not be built. Its text is fixed wording, safe
// to show. Say, when set, is the same in plain words that say what to do,
// for a failure the person cannot fix on this machine.
type Error struct{ Why, Say string }

func (e *Error) Error() string { return "the session's sandbox could not be built: " + e.Why }

// Box is one wall: New builds it, Start starts a command in it, Close ends
// its proxy once the command exited.
type Box struct {
	copy, home, tmp string
	readOnly        bool
	read            []path
	secret          []string // paths the wall hides even from a Read grant (secretPaths)
	jail            *netjail.Jail
	plat            platform
}

// path is a path the agent may read: real is it with every symlink
// followed, link the path as given when it differs.
type path struct {
	real, link string
	dir        bool
}

// TmpDir is the folder in the session's home that is the agent's TMPDIR.
const TmpDir = "tmp"

// New builds the wall for s, or says why it cannot. Nothing runs yet; a
// check run in the wall (a program that does nothing) has proved it starts.
func New(s Spec) (*Box, error) {
	if !Supported {
		return nil, &Error{Why: "this platform has no sandbox; it is built on macOS and Linux"}
	}
	b := &Box{readOnly: s.ReadOnly}
	var err error
	if b.copy, err = realDir(s.Copy); err != nil {
		return nil, &Error{Why: "the session's folder is not there"}
	}
	if b.home, err = realDir(s.Home); err != nil {
		return nil, &Error{Why: "the session has no home of its own on this runner"}
	}
	b.tmp = filepath.Join(b.home, TmpDir)
	if err := os.MkdirAll(b.tmp, 0o700); err != nil {
		return nil, &Error{Why: "the session's tmp folder could not be made"}
	}
	for _, r := range s.Read {
		p, err := readable(r)
		if err != nil {
			return nil, &Error{Why: "a path the agent needs to read is not there: " + filepath.Base(r) + " — it must exist on this runner"}
		}
		b.read = append(b.read, p)
	}
	// The person's keys and sign-ins, hidden from the agent even inside a
	// Read grant (on macOS; see the package doc for the Linux limit). The
	// default allow-list is added a layer up, in run.wallNet, where an
	// offline turn can be told apart; the git defaults are in Env.
	b.secret = secretPaths()
	if err := b.build(s.Net); err != nil {
		return nil, err
	}
	return b, nil
}

// Env is what the agent's environment needs: its tmp folder in its home,
// the proxy, and the git defaults -- ssh remotes rewritten to HTTPS, ssh
// itself ruled out -- so a fetch goes through the proxy instead of ssh.
func (b *Box) Env() []string {
	env := []string{"TMPDIR=" + b.tmp, "CLAUDE_CODE_TMPDIR=" + b.tmp}
	env = append(env, b.jail.Env()...)
	return append(env, gitDefaultsEnv()...)
}

// Close ends the wall's proxy and what still uses it.
func (b *Box) Close() { b.jail.Close() }

// realDir is dir with every symlink followed; it must be a folder.
func realDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("empty")
	}
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(r)
	if err != nil || !info.IsDir() {
		return "", errors.New("not a folder")
	}
	return r, nil
}

// readable is p as a path the agent may read.
func readable(p string) (path, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return path{}, err
	}
	r, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return path{}, err
	}
	info, err := os.Stat(r)
	if err != nil {
		return path{}, err
	}
	out := path{real: r, dir: info.IsDir()}
	if abs != r {
		out.link = abs
	}
	return out, nil
}

// Program is what to make readable for a program at p: its file, through
// any symlink, and the folder that holds it, where its other files live (as
// codex keeps its helpers beside it). For a program in the bin folder of an
// npm package, the package: its node_modules hold the real program (codex
// from npm, 2026-11-11). Never a folder that holds the person's home: then
// the file alone. A script's interpreter, from its #! line, is a program
// too (node under nvm, for a CLI installed with npm).
func Program(p string) []string {
	return program(p, 0)
}

// program is Program, following interpreters depth levels deep at most.
func program(p string, depth int) []string {
	out := []string{p}
	if depth < 2 {
		if in := interpreter(p); in != "" {
			out = append(out, program(in, depth+1)...)
		}
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return out
	}
	dir := filepath.Dir(r)
	if filepath.Base(dir) == "bin" {
		if info, err := os.Stat(filepath.Join(filepath.Dir(dir), "package.json")); err == nil && info.Mode().IsRegular() {
			dir = filepath.Dir(dir)
		}
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		if h, err := filepath.EvalSymlinks(home); err == nil {
			home = h
		}
		if dir == home || strings.HasPrefix(home+string(filepath.Separator), dir+string(filepath.Separator)) || dir == "/" {
			return out
		}
	}
	return append(out, dir)
}

// interpreter is the program a script at p names on its #! line, found on
// PATH for "#!/usr/bin/env name", or "" when p is not a script or the
// program is not found.
func interpreter(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, 256)
	n, _ := io.ReadFull(f, b)
	line, ok := strings.CutPrefix(string(b[:n]), "#!")
	if !ok {
		return ""
	}
	line, _, _ = strings.Cut(line, "\n")
	f2 := strings.Fields(line)
	if len(f2) == 0 {
		return ""
	}
	if filepath.Base(f2[0]) != "env" {
		return f2[0]
	}
	// env's own flags (-S) are left out: the first word that is not one
	// is the program.
	for _, w := range f2[1:] {
		if !strings.HasPrefix(w, "-") {
			if found, err := exec.LookPath(w); err == nil {
				return found
			}
			return ""
		}
	}
	return ""
}

// MaxGrant bounds the home_files' files in all, in bytes, and MaxGrantFiles
// their number: a folder such as ~/.claude/skills is walked at every turn.
const (
	MaxGrant      = 64 << 20
	MaxGrantFiles = 10000
)

// ClaudeJSON is the grant whose file holds Claude Code's MCP servers beside
// its account and sign-in state. Only its mcpServers part is copied
// (mergeMCP), never the rest.
const ClaudeJSON = "~/.claude.json"

// claudeJSONTo are the places in a session's home the mcpServers part of
// ClaudeJSON goes: the home's own .claude.json, and the one in the claude
// config folder, where Claude Code reads it under CLAUDE_CONFIG_DIR, which
// a session's home sets (harness/claude.go, ConfigEnv).
var claudeJSONTo = []string{".claude.json", filepath.Join(".claude", ".claude.json")}

// mcpServers is the one key of ClaudeJSON copied.
const mcpServers = "mcpServers"

// partMark ends a record line that names a file the runner merged a part
// into (ClaudeJSON), not one it wrote: taken back, the part is removed and
// the file stays.
const partMark = "\t" + mcpServers

// CopyGrants copies each of grants (the policy's home_files), a path under userHome written "~/x",
// into home at the same place ("x"): a file, or a folder's files at their
// places in it, at any depth (anything but a regular file or a folder is
// left out). Run at the start of each turn. ClaudeJSON is the one
// exception: only its mcpServers part goes, merged into the session's own
// .claude.json (mergeMCP).
//
// A file is copied when the session has none at its place, or when the
// original is newer than the session's file: a refreshed sign-in of a tool
// (gh, aws) or changed settings arrive at the next turn. A session's file
// as new as the original, or newer (the agent changed it), stays. A copy
// keeps the original's time, so an unchanged original is not copied again.
//
// One rule decides what it may remove: record lists the files it wrote, and
// those, and only those, are the runner's. A grant taken away has its files
// removed at the next turn (a copy the session made of it itself stays). It
// never removes a folder, and never a file it did not write, so what the
// session keeps beside a grant (its own config, a login cache) stays.
// record is a file the session cannot write: beside its home, never in it.
//
// Every grant is checked first: its shape, that it is there and a file or
// a folder, that nothing but a file is at a file's place in the home,
// MaxGrant and MaxGrantFiles in all. An Error then leaves the home as it was.
//
// signIn says what a path under the home, written x, is when it is a vendor
// sign-in (settings.SignInFile), else "". A sign-in never goes, by its place
// in the session's home or by the file it is in userHome through a link:
// named, the call is refused; inside a folder, it is left out.
func CopyGrants(home, record, userHome string, grants []string, signIn func(rel string) string) (ret error) {
	b, err := os.ReadFile(record)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &Error{Why: "the record of the session's earlier home_files could not be read"}
	}
	old, oldParts := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		rel, part := strings.CutSuffix(line, partMark)
		if !underHome(rel) || filepath.Clean(rel) != rel {
			return &Error{Why: "the record of the session's earlier home_files is not one this runner wrote"}
		}
		if part {
			oldParts[rel] = true
		} else {
			old[rel] = true
		}
	}
	if len(old) == 0 && len(oldParts) == 0 && len(grants) == 0 {
		return nil
	}
	if userHome == "" && len(grants) > 0 {
		return &Error{Why: "home_files names paths under ~, and this runner's home folder is not known"}
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return &Error{Why: "the session has no home of its own on this runner"}
	}
	defer root.Close()
	realUser := userHome
	if r, err := filepath.EvalSymlinks(userHome); err == nil {
		realUser = r
	}

	// Check every grant, file by file, before anything changes.
	type file struct {
		src, rel string
		size     int64
		mod      time.Time
		copy     bool // the session has none, or an older one
	}
	var files []file
	seen := map[string]bool{}
	var total int64
	var servers json.RawMessage // ClaudeJSON's mcpServers, when granted
	mcp := false
	for _, g := range grants {
		rel, ok := strings.CutPrefix(g, "~/")
		rel = filepath.Clean(rel)
		if !ok || !underHome(rel) {
			return &Error{Why: "a home_files entry is not a path under ~: " + g}
		}
		src, err := filepath.EvalSymlinks(filepath.Join(userHome, rel))
		if err != nil {
			return &Error{Why: "a home_files entry is not on this runner: " + g}
		}
		info, err := os.Stat(src)
		if err != nil {
			return &Error{Why: "a home_files entry is not on this runner: " + g}
		}
		top := src
		if "~/"+rel == ClaudeJSON {
			if !info.Mode().IsRegular() {
				return &Error{Why: "a home_files entry is not a file: " + g}
			}
			if servers, err = readMCP(src); err != nil {
				return err
			}
			mcp = true
			continue
		}
		add := func(src, rel string, info fs.FileInfo) error {
			// A vendor sign-in, by its place in the session's home or by
			// the file it is in this user's home (through a link), never
			// goes: named, the call is refused; in a folder, it is left out.
			if what := signInOf(signIn, realUser, src, rel); what != "" {
				if src == top {
					return &Error{Why: "a home_files entry is " + what + "; vendor sign-ins stay with control: " + g}
				}
				return nil
			}
			if seen[rel] {
				return nil
			}
			if total += info.Size(); total > MaxGrant {
				return &Error{Why: "the home_files are more than 64 MiB in all; list fewer, or smaller folders"}
			}
			if len(files) >= MaxGrantFiles {
				return &Error{Why: "the home_files hold more than 10000 files in all; list fewer, or smaller folders"}
			}
			f := file{src: src, rel: rel, size: info.Size(), mod: info.ModTime(), copy: true}
			if have, err := root.Lstat(rel); err == nil {
				if !have.Mode().IsRegular() {
					return &Error{Why: "a home_files entry would replace something in the session's home that is not a file: ~/" + rel}
				}
				f.copy = f.mod.After(have.ModTime())
			}
			seen[rel] = true
			files = append(files, f)
			return nil
		}
		switch {
		case info.Mode().IsRegular():
			err = add(src, rel, info)
		case info.IsDir():
			err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return &Error{Why: "a home_files entry could not be read: " + g}
				}
				if !d.Type().IsRegular() {
					return nil
				}
				i, err := d.Info()
				if err != nil {
					return &Error{Why: "a home_files entry could not be read: " + g}
				}
				return add(p, filepath.Join(rel, strings.TrimPrefix(p, src)), i)
			})
		default:
			err = &Error{Why: "a home_files entry is not a file or a folder: " + g}
		}
		if err != nil {
			return err
		}
	}
	if mcp {
		for _, to := range claudeJSONTo {
			if have, err := root.Lstat(to); err == nil && !have.Mode().IsRegular() {
				return &Error{Why: "a home_files entry would replace something in the session's home that is not a file: ~/" + to}
			}
		}
	}

	// The runner's earlier files that no grant names any more go.
	for rel := range old {
		if !seen[rel] {
			if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return &Error{Why: "an earlier home_files entry could not be removed from the session's home"}
			}
		}
	}
	var wrote []string
	// The record is written whatever happens below: what is the runner's,
	// so the next call removes it.
	defer func() {
		if err := os.WriteFile(record, []byte(strings.Join(wrote, "\n")), 0o600); err != nil && ret == nil {
			ret = &Error{Why: "the record of the session's home_files could not be written"}
		}
	}()
	// The mcpServers part, merged in or, its grant taken away, taken out.
	for _, to := range claudeJSONTo {
		switch {
		case mcp:
			wrote = append(wrote, to+partMark)
			if err := mergeMCP(root, to, servers); err != nil {
				return err
			}
		case oldParts[to]:
			if err := mergeMCP(root, to, nil); err != nil {
				return err
			}
		}
	}
	for _, f := range files {
		// A file the runner wrote stays the runner's, copied now or not.
		if !f.copy {
			if old[f.rel] {
				wrote = append(wrote, f.rel)
			}
			continue
		}
		if err := root.MkdirAll(filepath.Dir(f.rel), 0o700); err != nil {
			return &Error{Why: "a home_files entry could not be copied into the session's home: ~/" + f.rel}
		}
		wrote = append(wrote, f.rel)
		if err := copyFile(root, f.src, f.rel, f.size); err != nil {
			return &Error{Why: "a home_files entry could not be copied into the session's home: ~/" + f.rel}
		}
		if err := root.Chtimes(f.rel, f.mod, f.mod); err != nil {
			return &Error{Why: "a home_files entry could not be copied into the session's home: ~/" + f.rel}
		}
	}
	return nil
}

// signInOf says what vendor sign-in a grant's file is, by signIn: by rel,
// its place in the session's home, or by src, the file it copies, under
// realUser; "" when neither is one.
func signInOf(signIn func(string) string, realUser, src, rel string) string {
	if what := signIn(rel); what != "" {
		return what
	}
	if r, err := filepath.Rel(realUser, src); err == nil && underHome(r) {
		return signIn(r)
	}
	return ""
}

// readMCP is the mcpServers part of the Claude Code file at src (ClaudeJSON),
// nil when it has none. Nothing else of the file is kept.
func readMCP(src string) (json.RawMessage, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, &Error{Why: "a home_files entry could not be read: " + ClaudeJSON}
	}
	defer in.Close()
	b, err := io.ReadAll(io.LimitReader(in, MaxGrant+1))
	if err != nil {
		return nil, &Error{Why: "a home_files entry could not be read: " + ClaudeJSON}
	}
	if len(b) > MaxGrant {
		return nil, &Error{Why: "a home_files entry is more than 64 MiB: " + ClaudeJSON}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, &Error{Why: "a home_files entry is not a JSON object: " + ClaudeJSON}
	}
	s := top[mcpServers]
	if len(s) > 0 && s[0] != '{' {
		return nil, &Error{Why: "the mcpServers of a home_files entry is not a JSON object: " + ClaudeJSON}
	}
	return s, nil
}

// mergeMCP sets the mcpServers of the JSON file to in root to servers, or
// removes it when servers is nil, and keeps every other key the file has
// (the session's own state). A file that is not there is made, holding
// servers alone. A file that is not a JSON object is left as it is: the
// session's agent wrote it, and owns it. It is written whole, by rename.
func mergeMCP(root *os.Root, to string, servers json.RawMessage) error {
	fail := &Error{Why: "the MCP servers could not be copied into the session's home: ~/" + to}
	top := map[string]json.RawMessage{}
	b, err := root.ReadFile(to)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if servers == nil {
			return nil
		}
	case err != nil:
		return fail
	default:
		if json.Unmarshal(b, &top) != nil || top == nil {
			return nil
		}
	}
	if servers == nil {
		if _, ok := top[mcpServers]; !ok {
			return nil
		}
		delete(top, mcpServers)
	} else {
		top[mcpServers] = servers
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return fail
	}
	if err := root.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return fail
	}
	tmp := to + ".agentbus-tmp"
	if err := root.WriteFile(tmp, out, 0o600); err != nil {
		return fail
	}
	if err := root.Rename(tmp, to); err != nil {
		_ = root.Remove(tmp)
		return fail
	}
	return nil
}

// underHome says rel, a cleaned path, names something below a home: not
// the home itself, not above it.
func underHome(rel string) bool {
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// copyFile copies size bytes of src, a regular file when checked, to to in
// root, readable by its owner only. A file that is no longer regular is
// refused before it is read: never a wait on a pipe.
func copyFile(root *os.Root, src, to string, size int64) error {
	if info, err := os.Lstat(src); err != nil || !info.Mode().IsRegular() {
		return errors.New("not a file")
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(in, size)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// runCheck runs cmd, a do-nothing program in the wall, and says whether it
// ran.
func runCheck(cmd *exec.Cmd) bool {
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd.Run() == nil
}

// Can says whether this machine can build a session's wall, and when not,
// why, in a plain sentence: it builds one around an empty folder, which
// starts a program that does nothing in it, and takes it down. A runner
// reports the answer when it starts (queue.SandboxReport), so a turn is not
// refused by surprise.
func Can() (bool, string) { return can(New, systemHint) }

// hint is the system setting that refuses the wall, as a word the reason
// names it by and a sentence to follow the reason; both empty when none.
type hint func() (word, text string)

func can(build func(Spec) (*Box, error), h hint) (bool, string) {
	dir, err := os.MkdirTemp("", "agentbus-sandbox-check-")
	if err != nil {
		return false, "the check could not make a folder to build a wall around"
	}
	defer os.RemoveAll(dir)
	copy, home := filepath.Join(dir, "copy"), filepath.Join(dir, "home")
	if os.Mkdir(copy, 0o700) != nil || os.Mkdir(home, 0o700) != nil {
		return false, "the check could not make a folder to build a wall around"
	}
	b, err := build(Spec{Copy: copy, Home: home})
	if err != nil {
		why := "the sandbox could not be built"
		var se *Error
		if errors.As(err, &se) {
			why = se.Why
		}
		// A reason that names the setting already says it.
		if word, text := h(); word != "" && !strings.Contains(why, word) {
			why += text
		}
		return false, why
	}
	b.Close()
	return true, ""
}
