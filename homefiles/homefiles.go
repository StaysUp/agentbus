// Package homefiles copies the policy's home_files (a person's config
// files, such as ~/.gitconfig or ~/.claude/skills/) from their home into a
// session's own home on a runner, at the start of each turn, and takes back
// the ones the policy no longer lists. A vendor sign-in never goes.
//
// Every error is fixed wording, safe to show.
package homefiles

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Error is why the home_files could not be copied. Its text is fixed
// wording, safe to show.
type Error struct{ Why string }

func (e *Error) Error() string { return "the session's home files could not be copied: " + e.Why }

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
