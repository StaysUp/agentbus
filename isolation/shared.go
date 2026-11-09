package isolation

// Shared folders: a big folder at the top of the person's folder, such as
// node_modules, that every copy of the folder links to instead of copying.
// On macOS a clone costs per file, not per byte (about 19 µs and 0.35 KB per
// file), so a 50,000-file node_modules is most of a copy's cost.
//
// The shared folder is one copy (a clone where the disk has them) of the
// person's folder, kept under .isolation/.shared/<key>, with every file and
// folder made read-only: a turn that writes there, such as npm install,
// fails with "permission denied" rather than changing what other sessions
// see. The key names the person's folder and a hash of every file's path,
// size, modification time and mode, so a changed folder gets a new copy and
// copies of the same state share one. The copy holds a symbolic link to it
// where the folder was. A shared copy no space links to is removed when a
// space closes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/workspace"
)

const sharedFolder = ".shared"

// sharedMu keeps the shared copies to one change at a time.
var sharedMu sync.Mutex

// shareable are the names of share that are real folders at the top of
// folder: one name each, not a path, never .git.
func shareable(folder string, share []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range share {
		if name == "" || name == "." || name == ".." || name == ".git" || strings.ContainsAny(name, `/\`) || seen[name] {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(folder, name)); err == nil && fi.IsDir() {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// cloneTreeExcept copies src to dst, which must not exist, as cloneTree
// does, but for the folders at the top named in skip.
func cloneTreeExcept(src, dst string, skip []string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dst, info.Mode().Perm()|0o700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return "", err
	}
	left := map[string]bool{}
	for _, n := range skip {
		left[n] = true
	}
	method := MethodClone
	for _, e := range entries {
		if left[e.Name()] {
			continue
		}
		from, to := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		switch {
		case fi.IsDir():
			m, err := cloneTree(from, to)
			if err != nil {
				return "", err
			}
			if m != MethodClone {
				method = m
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(from)
			if err != nil {
				continue
			}
			if err := os.Symlink(target, to); err != nil {
				return "", err
			}
		case fi.Mode().IsRegular():
			if err := cloneFile(from, to, fi.Mode().Perm()); err != nil {
				continue // unreadable: left out, as a Snapshot leaves it out
			}
			_ = os.Chtimes(to, fi.ModTime(), fi.ModTime())
		}
	}
	_ = os.Chmod(dst, info.Mode().Perm()|0o700)
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return method, nil
}

// sharedCopy is the read-only shared copy of the folder dir as it is now,
// made if no copy of that state is kept yet.
func sharedCopy(workdir, dir string) (string, error) {
	sig, err := signature(dir)
	if err != nil {
		return "", err
	}
	name := sha256.Sum256([]byte(dir))
	out := filepath.Join(workdir, stateFolder, sharedFolder, hex.EncodeToString(name[:8])+"-"+sig)
	if fi, err := os.Lstat(out); err == nil && fi.IsDir() {
		return out, nil
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", err
	}
	tmp := out + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := cloneTree(dir, tmp); err != nil {
		removeAll(tmp)
		return "", err
	}
	// Read-only, files and folders: nothing in it can be added, removed or
	// written by the user the turns run as (root excepted).
	// Its own folder last, once renamed: a folder that cannot be written
	// cannot be renamed either.
	readOnly := func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 || p == tmp {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			_ = os.Chmod(p, fi.Mode().Perm()&^0o222)
		}
		return nil
	}
	_ = filepath.WalkDir(tmp, readOnly)
	if err := os.Rename(tmp, out); err != nil {
		removeAll(tmp)
		return "", err
	}
	if fi, err := os.Stat(out); err == nil {
		_ = os.Chmod(out, fi.Mode().Perm()&^0o222)
	}
	return out, nil
}

// signature is a hash of every path under dir with its size, modification
// time and mode: a stat of each, no file read.
func signature(dir string) (string, error) {
	list, _, err := workspace.List(dir, false, math.MaxInt)
	if err != nil {
		return "", err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Rel < list[j].Rel })
	h := sha256.New()
	for _, it := range list {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\n", it.Rel, it.Info.Size(), it.Info.ModTime().UnixNano(), it.Info.Mode())
	}
	return hex.EncodeToString(h.Sum(nil)[:12]), nil
}

// linkShared puts in the copy at root a link to a shared copy of each folder
// of names, and answers the shared copies. sharedMu is held until the space
// that links them is saved, so gcShared never removes them first.
func linkShared(workdir, source, root string, names []string) ([]string, error) {
	var out []string
	for _, name := range names {
		dir, err := sharedCopy(workdir, filepath.Join(source, name))
		if err != nil {
			return out, err
		}
		out = append(out, dir)
		if err := os.Symlink(dir, filepath.Join(root, name)); err != nil {
			return out, err
		}
	}
	return out, nil
}

// gcShared removes the shared copies no space links to. sharedMu is held.
func gcShared(workdir string) error {
	used := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(workdir, stateFolder))
	for _, e := range entries {
		if !e.IsDir() || !spaceName.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(workdir, stateFolder, e.Name(), "space.json"))
		if err != nil {
			continue
		}
		var s Space
		if json.Unmarshal(b, &s) == nil {
			for _, d := range s.Shared {
				used[d] = true
			}
		}
	}
	dir := filepath.Join(workdir, stateFolder, sharedFolder)
	kept, _ := os.ReadDir(dir)
	for _, e := range kept {
		p := filepath.Join(dir, e.Name())
		if !used[p] {
			if err := removeAll(p); err != nil {
				return err
			}
		}
	}
	return nil
}
