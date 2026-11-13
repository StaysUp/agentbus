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
// renames of the walled folders, Landlock on Linux, and the browser bridge. Every error is fixed wording,
// safe to show, and New fails before anything starts: a session that asks
// for the wall never runs without it.
package sandbox

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"strings"

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
// to show.
type Error struct{ Why string }

func (e *Error) Error() string { return "the session's sandbox could not be built: " + e.Why }

// Box is one wall: New builds it, Start starts a command in it, Close ends
// its proxy once the command exited.
type Box struct {
	copy, home, tmp string
	readOnly        bool
	read            []path
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
			return nil, &Error{Why: "a path the agent needs to read is not there: " + filepath.Base(r)}
		}
		b.read = append(b.read, p)
	}
	if err := b.build(s.Net); err != nil {
		return nil, err
	}
	return b, nil
}

// Env is what the agent's environment needs: its tmp folder in its home,
// and the proxy.
func (b *Box) Env() []string {
	env := []string{"TMPDIR=" + b.tmp, "CLAUDE_CODE_TMPDIR=" + b.tmp}
	return append(env, b.jail.Env()...)
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

// MaxGrant bounds what CopyGrants copies in all, in bytes.
const MaxGrant = 16 << 20

// CopyGrants copies each of grants (the policy's home_files), a path under userHome written "~/x",
// into home at the same place ("x"): a file, or a folder's files at their
// places in it (anything but a regular file or a folder is left out). Run
// at the start of each turn.
//
// One rule decides what it may touch: record lists the files it wrote, and
// those, and only those, are the runner's. It removes them first, so a
// grant taken away is gone at the next turn (a copy the session made of it
// itself stays), writes the grants' files, and writes record anew. It never
// removes a folder, and never replaces a file the session has that it did
// not write, so what the session keeps beside a grant (its own config, a
// login cache) stays. record is a file the session cannot write: beside its
// home, never in it.
//
// Every grant is checked first: its shape, that it is there and a file or
// a folder, that none of its files would replace one of the session's own,
// MaxGrant in all. An Error then leaves the home as it was.
func CopyGrants(home, record, userHome string, grants []string) (ret error) {
	b, err := os.ReadFile(record)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &Error{Why: "the record of the session's earlier home_files could not be read"}
	}
	old := map[string]bool{}
	for _, rel := range strings.Split(string(b), "\n") {
		if rel == "" {
			continue
		}
		if !underHome(rel) || filepath.Clean(rel) != rel {
			return &Error{Why: "the record of the session's earlier home_files is not one this runner wrote"}
		}
		old[rel] = true
	}
	if len(old) == 0 && len(grants) == 0 {
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

	// Check every grant, file by file, before anything changes.
	type file struct {
		src, rel string
		size     int64
	}
	var files []file
	seen := map[string]bool{}
	var total int64
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
		add := func(src, rel string, size int64) error {
			if seen[rel] {
				return nil
			}
			if _, err := root.Lstat(rel); err == nil && !old[rel] {
				return &Error{Why: "a home_files entry would replace a file the session has in its home: ~/" + rel}
			}
			if total += size; total > MaxGrant {
				return &Error{Why: "the home_files are more than 16 MiB in all"}
			}
			seen[rel] = true
			files = append(files, file{src, rel, size})
			return nil
		}
		switch {
		case info.Mode().IsRegular():
			err = add(src, rel, info.Size())
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
				return add(p, filepath.Join(rel, strings.TrimPrefix(p, src)), i.Size())
			})
		default:
			err = &Error{Why: "a home_files entry is not a file or a folder: " + g}
		}
		if err != nil {
			return err
		}
	}

	for rel := range old {
		if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return &Error{Why: "an earlier home_files entry could not be removed from the session's home"}
		}
	}
	var wrote []string
	// The record is written whatever happens below: what was written, so
	// the next call removes it.
	defer func() {
		if err := os.WriteFile(record, []byte(strings.Join(wrote, "\n")), 0o600); err != nil && ret == nil {
			ret = &Error{Why: "the record of the session's home_files could not be written"}
		}
	}()
	for _, f := range files {
		if err := root.MkdirAll(filepath.Dir(f.rel), 0o700); err != nil {
			return &Error{Why: "a home_files entry could not be copied into the session's home: ~/" + f.rel}
		}
		wrote = append(wrote, f.rel)
		if err := copyFile(root, f.src, f.rel, f.size); err != nil {
			return &Error{Why: "a home_files entry could not be copied into the session's home: ~/" + f.rel}
		}
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
