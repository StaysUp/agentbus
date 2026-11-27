package isolation

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// A space is on disk only while its record is: Load finds a space by its
// space.json and nothing else, so the record says the copy is whole. Every
// backend's Open writes the record last, once the copy and its Base are made,
// and every Close takes the record away first, before any file of the copy
// goes. A copy half made (a stop, a crash, a full disk) or half removed (a file
// the person locked in Finder, which the clone keeps) has no record, so no turn
// runs in it and no changes are read from it: a turn there would see the
// missing files as deleted, and an apply would delete them from the person's
// folder (#96). Go's module cache holds the same rule with its .partial file,
// and containerd's snapshotter drops a snapshot's metadata before its folder.
//
// What such a copy leaves is moved aside, under the state folder, and removed
// from there: <workdir>/.isolation/.removing never holds a space and never
// names a job, so neither Load nor Open reads it. A file that cannot be
// removed stays there, out of every copy's way, and SweepRemoved tries again
// when a runner starts.

// record writes the record of a space Open has made, as every Open's last
// step, and only while the stop has not come: a stopped Open leaves no
// record, whatever step it was in when the stop came. A stop that comes once
// the record is written reaches the turn instead.
func (s *Space) record() error {
	if beforeRecord != nil {
		beforeRecord(s)
	}
	if err := s.stopCtx().Err(); err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	s.stop = nil
	return nil
}

// beforeRecord, set by a test, runs just before an Open writes its record.
var beforeRecord func(*Space)

// renameCopy moves a copy aside (os.Rename); a test makes it fail.
var renameCopy = os.Rename

// removingFolder holds what closed spaces left, under the state folder. Its
// name starts with a dot, so it is never a space id.
const removingFolder = ".removing"

func removingDir(workdir string) string { return filepath.Join(workdir, stateFolder, removingFolder) }

// retire takes s's record away, then moves its copy out of s.Root, then
// removes both. It fails only while s's copy is still whole at s.Root, and
// then the record is put back: a copy is never left in its place with no
// record. What is left once both are moved is removed as far as it can be,
// and the rest waits for SweepRemoved. With nothing on disk it makes nothing.
func retire(s *Space) error {
	if !exists(s.state()) && !exists(s.Root) {
		return nil
	}
	aside, err := newAside(s)
	if err != nil {
		return err
	}
	// The record first: from this rename on, no Load finds the space.
	if err := moveAside(s.state(), aside, "state"); err != nil {
		_ = os.Remove(aside)
		return err
	}
	// Then the copy, out of the place the next Open makes one. A copy whose
	// top folder the person locked keeps the lock; it is the copy's own. A
	// folder moves to another only when its owner may write it (its ".."
	// changes), so its owner's rights come back first. s.Root is in the
	// workdir, which no turn writes, so it is the runner's own folder.
	unlock(s.Root)
	if fi, err := os.Lstat(s.Root); err == nil && fi.IsDir() {
		_ = os.Chmod(s.Root, fi.Mode().Perm()|0o700)
	}
	if err := renameCopy(s.Root, filepath.Join(aside, "copy")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// The copy is where it was, whole: its record goes back with it. A
		// record that cannot go back stays aside, and the copy has none, so
		// the next close or Open of the id retires it again.
		if exists(filepath.Join(aside, "state")) {
			if rerr := os.Rename(filepath.Join(aside, "state"), s.state()); rerr != nil {
				return err
			}
		}
		_ = removeAll(aside)
		return err
	}
	_ = removeAll(aside)
	return nil
}

// dropRecord takes s's record away, its whole state folder in one rename, so
// from then on no Load finds s, and removes it. Removing the record in place
// could stop part way, leaving space.json with some of its checkpoints.
func dropRecord(s *Space) error {
	if !exists(s.state()) {
		return nil
	}
	aside, err := newAside(s)
	if err != nil {
		return err
	}
	if err := moveAside(s.state(), aside, "state"); err != nil {
		_ = os.Remove(aside)
		return err
	}
	_ = removeAll(aside)
	return nil
}

// newAside makes a fresh folder for what s leaves, under removingFolder.
func newAside(s *Space) (string, error) {
	if err := os.MkdirAll(removingDir(s.Workdir), 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(removingDir(s.Workdir), s.ID+"-")
}

// moveAside renames p into aside as name. A p that is not there is moved.
func moveAside(p, aside, name string) error {
	if err := os.Rename(p, filepath.Join(aside, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// exists says whether p is there, whatever it is.
func exists(p string) bool {
	_, err := os.Lstat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

// RemoveLeft removes what a space with id left under workdir with no record:
// a copy an Open stopped making, or one whose close stopped after its record
// went (a crash between the two). A close calls it when it finds no record,
// so a workspace never ends closed with a copy still in its place. A space
// with a record is not touched: Close removes that one.
func RemoveLeft(workdir, id string) error {
	if !spaceName.MatchString(id) {
		return ErrOutside
	}
	s := &Space{ID: id, Workdir: workdir, Root: filepath.Join(workdir, id)}
	if exists(filepath.Join(s.state(), "space.json")) {
		return nil
	}
	if err := unmountLeft(s.Root); err != nil {
		return err
	}
	return retire(s)
}

// SweepRemoved removes what closed spaces left under workdir and could not
// be removed then. A runner calls it when it starts.
func SweepRemoved(workdir string) error {
	dir := removingDir(workdir)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return removeAll(dir)
}
