package connector

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/staysup/agentbus/workspace"
)

// lookup finds what the folder holds at the paths of an apply's steps, as a
// checkpoint would list it. A checkpoint never follows a link and takes each
// name as its folder lists it. Opening a path does neither: cur/a is found
// through the link cur, and on a disk that takes names that differ only in
// case for one (a Mac's, by default) Button.tsx finds button.tsx. A file
// found so is not the step's: it must not count as already written, and it
// goes when the changes remove what it was found through.
type lookup struct {
	rt      *os.Root
	steps   []workspace.Step
	removes map[string]bool            // the paths the changes remove
	dirs    map[string]bool            // folders seen to be folders
	names   map[string]map[string]bool // the names a folder lists, by folder
	found   []foundFile                // of steps, once asked for
	asked   bool
}

// foundFile is the file found at a step's path, and whether the folder lists
// it as the path spells it.
type foundFile struct {
	path   string
	info   fs.FileInfo
	listed bool
}

func newLookup(rt *os.Root, steps []workspace.Step) *lookup {
	l := &lookup{rt: rt, steps: steps, removes: map[string]bool{}, dirs: map[string]bool{}, names: map[string]map[string]bool{}}
	for _, st := range steps {
		if st.To == nil {
			l.removes[st.Path] = true
		}
	}
	return l
}

// at says whether something stands at st's path to compare with st. When
// nothing does, the path is free, or the changes remove what is in the way;
// when the step cannot be taken whatever the content, why says so. respelt
// is the path that the changes remove, when it names the same file as st's
// in another spelling.
func (l *lookup) at(st workspace.Step) (there bool, respelt, why string) {
	for i := range len(st.Path) {
		if st.Path[i] != '/' || l.dirs[st.Path[:i]] {
			continue
		}
		p := st.Path[:i]
		fi, err := l.rt.Lstat(filepath.FromSlash(p))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return false, "", ""
		case err != nil:
			return false, "", WhyNotFile
		case fi.IsDir():
			l.dirs[p] = true
		case st.To == nil:
			return false, "", "" // a file or a link, and no file under it
		case fi.Mode()&fs.ModeSymlink != 0 && l.removed(p, fi) != "":
			return false, "", "" // a link, which makes way
		default:
			return false, "", WhyNoFolder
		}
	}
	fi, err := l.rt.Lstat(filepath.FromSlash(st.Path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, "", ""
	case err != nil:
		return false, "", WhyNotFile
	case fi.IsDir() && st.To == nil:
		return false, "", "" // no file to remove
	case fi.IsDir():
		return false, "", WhyNotFile
	case st.To == nil && l.other(st.Path, fi) != "":
		return false, "", "" // removed already: this is the file respelt
	case st.From == nil:
		if p := l.removed(st.Path, fi); p != "" {
			return false, p, ""
		}
	}
	return true, "", ""
}

// removed is the path under which the changes remove fi, the file found at
// p: p itself or another spelling of it, "" when they do not.
func (l *lookup) removed(p string, fi fs.FileInfo) string {
	if q := l.other(p, fi); q != "" {
		p = q
	}
	if l.removes[p] {
		return p
	}
	if l.listed(p) {
		return ""
	}
	// No step spells fi as the folder does: a step that removes it removes
	// it in whatever spelling.
	for _, f := range l.files() {
		if l.removes[f.path] && os.SameFile(f.info, fi) {
			return f.path
		}
	}
	return ""
}

// other is the path of another step that fi, found at p, belongs to: one
// the folder lists as spelled, while it does not list p. Hard links are two
// files, each listed.
func (l *lookup) other(p string, fi fs.FileInfo) string {
	if l.listed(p) {
		return ""
	}
	for _, f := range l.files() {
		if f.listed && os.SameFile(f.info, fi) {
			return f.path
		}
	}
	return ""
}

// files are the files found at the steps' paths, before anything is written.
func (l *lookup) files() []foundFile {
	if !l.asked {
		l.asked = true
		for _, st := range l.steps {
			if fi, err := l.rt.Lstat(filepath.FromSlash(st.Path)); err == nil && !fi.IsDir() {
				l.found = append(l.found, foundFile{st.Path, fi, l.listed(st.Path)})
			}
		}
	}
	return l.found
}

// listed says whether each folder on the way to p lists the next name as p
// spells it.
func (l *lookup) listed(p string) bool {
	dir := "."
	for name := range strings.SplitSeq(p, "/") {
		names, ok := l.names[dir]
		if !ok {
			names = map[string]bool{}
			if f, err := l.rt.Open(filepath.FromSlash(dir)); err == nil {
				list, _ := f.Readdirnames(-1)
				f.Close()
				for _, n := range list {
					names[n] = true
				}
			}
			l.names[dir] = names
		}
		if !names[name] {
			return false
		}
		if dir == "." {
			dir = name
		} else {
			dir += "/" + name
		}
	}
	return true
}

// errHolds: a folder holds a file.
var errHolds = errors.New("connector: the folder holds a file")

// removeEmptied removes the folder dir of rt and the folders in it, when
// they hold no file. It never removes a file.
func removeEmptied(rt *os.Root, dir string) error {
	var dirs []string
	err := fs.WalkDir(rt.FS(), filepath.ToSlash(dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return errHolds
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := rt.Remove(filepath.FromSlash(dirs[i])); err != nil {
			return err
		}
	}
	return nil
}

// respeltFolder is the folder to make anew when the file from is to be to,
// the same path in another spelling: the first that the two spell apart, ""
// when only the file's own name differs.
func respeltFolder(from, to string) string {
	a, b := strings.Split(from, "/"), strings.Split(to, "/")
	for i := range min(len(a), len(b)) - 1 {
		if a[i] != b[i] {
			return strings.Join(a[:i+1], "/")
		}
	}
	return ""
}
