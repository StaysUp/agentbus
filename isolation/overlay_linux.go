package isolation

// The overlay backend: a session's copy is an overlayfs mount, a read-only
// snapshot of the person's folder below and an empty folder of its own above
// (the upper layer) that takes every write.
//
// Everything is the session's own, under its record:
//
//	.isolation/<id>/l/          the snapshot: a copy of the person's folder as
//	                            it was when the session began (a reflink per
//	                            file where the disk has them)
//	.isolation/<id>/lower.json  the snapshot's files, as a tree (lowerRec)
//	.isolation/<id>/u, <id>/w   the upper layer and overlay's work folder
//
// The person's live folder is never the lower layer: a write there would
// change the copy. Nothing is shared with another session, so closing one
// removes all it made and touches no other.
//
// A checkpoint walks the upper layer only: every change since the copy was
// made is there, a removed file as a whiteout. It is the snapshot's tree
// with those paths edited, so it writes only the trees of the folders on
// them, and no file of the snapshot is read. Its record is as the copy
// backend keeps it, so Diff and Apply are the same code. Nothing is unmounted at a checkpoint, so a
// process the agent left running cannot stop one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/staysup/agentbus/workspace"
)

// MethodOverlay is how an overlay space's copy was made.
const MethodOverlay = "overlay"

// overlayBackend makes each copy an overlay mount. It needs a runner that
// can mount: root, or CAP_SYS_ADMIN. Open answers Unavailable, with why,
// where it cannot, and For falls back to copy.
type overlayBackend struct{}

func (overlayBackend) Name() string { return Overlay }

// lowerRec is the snapshot's record: its files as the Base sees them, .git
// left out, as a tree in the space's trees.
type lowerRec struct {
	Tree  treeState `json:"tree"`
	Files int       `json:"files"`
}

func storeDir(workdir string) string { return filepath.Join(workdir, stateFolder) }
func (s *Space) lowerDir() string    { return filepath.Join(s.state(), "l") }

func (overlayBackend) Open(r Request) (*Space, error) {
	if err := checkFolder(r); err != nil {
		return nil, err
	}
	if s, err := Load(r.Workdir, filepath.Join(r.Workdir, r.ID)); err == nil {
		// A retry of the job that made it, perhaps after a restart that
		// took the mount away.
		if s.Backend == Overlay {
			if err := mountSpace(s); err != nil {
				return nil, err
			}
		}
		return s, nil
	}
	if why := overlayWhy(r.Workdir); why != "" {
		return nil, &Unavailable{Overlay, why}
	}
	s := &Space{ID: r.ID, Backend: Overlay, Source: filepath.Clean(r.Folder), Root: filepath.Join(r.Workdir, r.ID),
		Workdir: r.Workdir, Method: MethodOverlay}
	s.Dir = s.Root
	// What a failed attempt left, with no record, is this job's own.
	if err := unmountAll(s.Root); err != nil {
		return nil, err
	}
	if err := removeAll(s.Root); err != nil {
		return nil, err
	}
	if err := removeState(s); err != nil {
		return nil, err
	}
	rootInfo, err := os.Stat(s.Source)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Space, error) {
		overlayBackend{}.Close(s)
		return nil, err
	}
	if err := os.MkdirAll(s.state(), 0o700); err != nil {
		return fail(err)
	}
	if err := snapshot(s, time.Now()); err != nil {
		return fail(err)
	}
	// The upper layer's own folder is the copy's top folder: it has the
	// person's folder's mode.
	up := filepath.Join(s.state(), "u")
	if err := os.Mkdir(up, rootInfo.Mode().Perm()); err != nil {
		return fail(err)
	}
	if err := os.Chmod(up, rootInfo.Mode().Perm()); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(filepath.Join(s.state(), "w"), 0o700); err != nil {
		return fail(err)
	}
	if err := s.save(); err != nil {
		return fail(err)
	}
	if err := mountSpace(s); err != nil {
		return fail(err)
	}
	if _, err := checkpoint(s, Base, overlayBackend{}); err != nil {
		return fail(err)
	}
	return s, nil
}

// snapshot copies the person's folder into s's lower layer, a reflink per
// file where the disk has them, and records it. Its record is read from the
// copy, so it says what the layer holds, whatever changed in the folder
// meanwhile. A .git file at the top is left out, as the copy backend leaves
// it out.
func snapshot(s *Space, start time.Time) error {
	dir := s.lowerDir()
	if _, err := copyTree(s.Source, dir); err != nil {
		return err
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && !fi.IsDir() {
		if err := os.Remove(filepath.Join(dir, ".git")); err != nil {
			return err
		}
	}
	files, err := scan(dir, baseHasher(start))
	if err != nil {
		return err
	}
	// The Base leaves .git out, as workspace.Take does.
	files.Delete(".git")
	files.DeleteUnder(".git")
	t := s.trees()
	st, n, err := t.fromSnapshot(nil, nil, files)
	if err != nil {
		return err
	}
	if err := t.flush(); err != nil {
		return err
	}
	b, _ := json.Marshal(lowerRec{Tree: st, Files: n})
	return writeFileAtomic(filepath.Join(s.state(), "lower.json"), b)
}

// readLower is s's snapshot record.
func readLower(s *Space) (*lowerRec, error) {
	b, err := os.ReadFile(filepath.Join(s.state(), "lower.json"))
	if err != nil {
		return nil, errors.New("isolation: the copy's snapshot could not be read")
	}
	var rec lowerRec
	if err := json.Unmarshal(b, &rec); err != nil || rec.Tree.Root == "" {
		return nil, errors.New("isolation: the copy's snapshot could not be read")
	}
	return &rec, nil
}

func (b overlayBackend) Checkpoint(s *Space, name string) (Checkpoint, error) {
	// After a restart the mount is gone and the next turn needs it.
	if err := mountSpace(s); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint(s, name, b)
}

func (overlayBackend) Checkpoints(s *Space) ([]Checkpoint, error) { return checkpoints(s) }
func (overlayBackend) Diff(s *Space, from, to string) (workspace.Changes, error) {
	return diff(s, from, to)
}
func (overlayBackend) Apply(s *Space, from, to string) (Applied, error) { return apply(s, from, to) }

func (overlayBackend) Close(s *Space) error {
	if s.Root != filepath.Join(s.Workdir, s.ID) {
		return ErrOutside
	}
	if err := unmountAll(s.Root); err != nil {
		return err
	}
	if err := removeAll(s.Root); err != nil {
		return err
	}
	if err := removeState(s); err != nil {
		return err
	}
	return removeOldLayers(s.Workdir)
}

// oldLayers is where a build before session-owned snapshots kept layers
// that sessions shared.
const oldLayers = ".l"

// removeOldLayers removes the layers such a build left, once no space's
// record names them: a copy that build mounted as root can outlive its
// runner, and reads its files from there until its session closes.
func removeOldLayers(workdir string) error {
	dir := filepath.Join(storeDir(workdir), oldLayers)
	if _, err := os.Lstat(dir); err != nil {
		return nil
	}
	entries, _ := os.ReadDir(storeDir(workdir))
	for _, e := range entries {
		if !e.IsDir() || !spaceName.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(storeDir(workdir), e.Name(), "space.json"))
		if err != nil {
			continue
		}
		var old struct {
			Layers []int `json:"layers"`
		}
		if json.Unmarshal(b, &old) == nil && len(old.Layers) > 0 {
			return nil
		}
	}
	return removeAll(dir)
}

// take is the copy's files: the Base is the snapshot's own tree, and a
// later checkpoint is that tree with each path the upper layer holds laid
// over it.
func (overlayBackend) take(s *Space, t *trees, prev *treeState, save workspace.Hasher) (treeState, int, error) {
	rec, err := readLower(s)
	if err != nil {
		return treeState{}, 0, err
	}
	// Git's own state is read through the mount, as Walk reads it.
	gitState := func(st treeState) treeState {
		g := workspace.Empty()
		g.ReadGit(s.Dir)
		st.Git, st.Head, st.GitTree = g.GitState()
		return st
	}
	if prev == nil {
		return gitState(rec.Tree), rec.Files, nil
	}
	taken := time.Now().UnixNano()
	ed := t.edit(rec.Tree.Root, rec.Files)
	upper := filepath.Join(s.state(), "u")
	err = filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == upper {
				return err
			}
			return nil
		}
		rel, _ := filepath.Rel(upper, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		mode := info.Mode()
		switch {
		case d.IsDir():
			// A folder made over a removed one hides all the snapshot held
			// there; any folder hides a file of that name.
			if opaque(p) {
				return ed.remove(rel)
			}
			return ed.removeFile(rel)
		case mode.IsRegular() || mode&fs.ModeSymlink != 0:
			f := workspace.File{Size: info.Size(), Mode: mode, ModTime: info.ModTime().UnixNano()}
			if old, ok := t.lookup(prev.Root, rel); ok && old.Size == f.Size && old.ModTime == f.ModTime && old.Mode == f.Mode && !racy(prev.Taken, old.ModTime) {
				f.Hash = old.Hash
			} else {
				h, err := save(rel, p, info)
				if err != nil {
					return ed.remove(rel)
				}
				f.Hash = h
			}
			return ed.set(rel, f)
		default:
			// A whiteout (a removed file or folder), or a device, a socket
			// or a pipe the turn made: either hides what the snapshot held
			// there, and neither is a file of the work.
			return ed.remove(rel)
		}
	})
	if err != nil {
		return treeState{}, 0, err
	}
	root, n := ed.finish()
	return gitState(treeState{Root: root, Taken: taken, Truncated: rec.Tree.Truncated}), n, nil
}

// learn reads rel's Base content from the snapshot: it never changes, so it
// is always there.
func (overlayBackend) learn(s *Space, rel string, f workspace.File) (string, bool, bool) {
	return learnFrom(filepath.Join(s.lowerDir(), filepath.FromSlash(rel)), f, s.objectDir())
}

// opaque says whether the upper folder p hides what the snapshot holds
// there. Every mount takes userxattr (overlayOptions), so the mark is under
// user., whether the runner is root or in a user namespace.
func opaque(p string) bool {
	b := make([]byte, 4)
	n, err := unix.Lgetxattr(p, "user.overlay.opaque", b)
	return err == nil && n == 1 && b[0] == 'y'
}

// mountSpace mounts s's snapshot and upper layer at s.Root, unless they are.
// Paths are relative to the store, so the options stay short.
func mountSpace(s *Space) error {
	if mounted(s.Root) {
		return nil
	}
	if _, err := os.Stat(s.lowerDir()); err != nil {
		return errors.New("isolation: the copy's snapshot is missing")
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	opts := "lowerdir=" + s.ID + "/l,upperdir=" + s.ID + "/u,workdir=" + s.ID + "/w," + overlayOptions
	return mountIn(storeDir(s.Workdir), s.Root, opts)
}

// overlayOptions keep the upper layer plain: a renamed folder is copied, not
// redirected, and a changed file is copied whole, so the upper layer alone
// says what changed. userxattr keeps overlay's own marks (an opaque folder)
// under user., which a mount in a user namespace needs, so a root mount
// takes it too: one name to read. redirect_dir=nofollow makes no redirect
// and follows none; off conflicts with userxattr, since a kernel may build
// off as follow.
const overlayOptions = "userxattr,redirect_dir=nofollow,index=off,metacopy=off"

// mountIn mounts an overlay at target with opts, whose paths are relative
// to dir. The working folder is the thread's own (CLONE_FS), and the thread
// is never given back: it ends with the goroutine, so no other code runs in
// dir.
func mountIn(dir, target, opts string) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			errc <- err
			return
		}
		if err := unix.Chdir(dir); err != nil {
			errc <- err
			return
		}
		errc <- unix.Mount("overlay", target, "overlay", 0, opts)
	}()
	return <-errc
}

// mounted says whether p is an overlay mount.
func mounted(p string) bool {
	var st unix.Statfs_t
	if unix.Statfs(p, &st) != nil || st.Type != unix.OVERLAYFS_SUPER_MAGIC {
		return false
	}
	var a, b unix.Stat_t
	return unix.Lstat(p, &a) == nil && unix.Lstat(filepath.Dir(p), &b) == nil && a.Dev != b.Dev
}

// unmountAll unmounts p until no overlay is mounted there. A mount still in
// use, by a process a turn left running, is detached: it goes once that
// process lets go of it.
func unmountAll(p string) error {
	for i := 0; i < 8 && mounted(p); i++ {
		if err := unix.Unmount(p, 0); err == unix.EBUSY {
			if err := unix.Unmount(p, unix.MNT_DETACH); err != nil {
				return err
			}
		} else if err != nil && err != unix.EINVAL {
			return err
		}
	}
	if mounted(p) {
		return errors.New("isolation: the copy could not be unmounted")
	}
	return nil
}

// probes caches, per workdir, why an overlay cannot be mounted there ("":
// it can).
var probes sync.Map

// noNamespace is why this runner runs outside a user namespace of its own,
// as the service found when it started (NoNamespace); "" when it is inside
// one, is root, or never tried.
var noNamespace atomic.Value // string

// NoNamespace records why the service, not root, could not run in its own
// user and mount namespace, where it could mount overlays. An overlay's
// refusal then says it, and so does the fallback a session records.
func NoNamespace(why string) { noNamespace.Store(why) }

// OverlayWhy says why this runner cannot mount an overlay in workdir, in
// words for the person, or "" when it can.
func OverlayWhy(workdir string) string { return overlayWhy(workdir) }

// overlayWhy says why this runner cannot mount an overlay in workdir, in
// words for the person, or "" when it can. It mounts a small one to find
// out, once per workdir.
func overlayWhy(workdir string) string {
	if v, ok := probes.Load(workdir); ok {
		return v.(string)
	}
	why := probe(workdir)
	probes.Store(workdir, why)
	return why
}

func probe(workdir string) string {
	dir := filepath.Join(storeDir(workdir), ".probe")
	m := filepath.Join(dir, "m")
	_ = unmountAll(m)
	_ = removeAll(dir)
	defer func() {
		_ = unmountAll(m)
		_ = removeAll(dir)
	}()
	for _, d := range []string{"l", "u", "w", "m"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return "the workdir cannot be written"
		}
	}
	if os.WriteFile(filepath.Join(dir, "l", "f"), []byte("f"), 0o600) != nil ||
		os.MkdirAll(filepath.Join(dir, "l", "d"), 0o700) != nil ||
		os.WriteFile(filepath.Join(dir, "l", "d", "g"), []byte("g"), 0o600) != nil {
		return "the workdir cannot be written"
	}
	err := mountIn(dir, m, "lowerdir=l,upperdir=u,workdir=w,"+overlayOptions)
	switch {
	case err == unix.EPERM || err == unix.EACCES:
		why := fmt.Sprintf("mounting an overlay needs root or CAP_SYS_ADMIN, and this runner runs as uid %d", os.Geteuid())
		if no, _ := noNamespace.Load().(string); no != "" {
			why += " outside a user namespace of its own: " + no
		}
		return why
	case err == unix.ENODEV:
		return "this kernel has no overlay filesystem"
	case err == unix.EINVAL:
		var st unix.Statfs_t
		_ = unix.Statfs(workdir, &st)
		return fmt.Sprintf("the workdir's disk (filesystem type 0x%x) cannot hold an overlay's upper layer", st.Type)
	case err != nil:
		return "mounting an overlay failed: " + err.Error()
	}
	// It mounted: a read, a write and a removal must land where they belong.
	if b, err := os.ReadFile(filepath.Join(m, "f")); err != nil || string(b) != "f" {
		return "an overlay mounted but did not show its lower layer"
	}
	if os.WriteFile(filepath.Join(m, "g"), []byte("g"), 0o600) != nil || os.Remove(filepath.Join(m, "f")) != nil {
		return "an overlay mounted but could not be written"
	}
	if _, err := os.Stat(filepath.Join(dir, "u", "g")); err != nil {
		return "an overlay mounted but its writes did not reach its upper layer"
	}
	if fi, err := os.Lstat(filepath.Join(dir, "u", "f")); err != nil || !isWhiteout(fi) {
		return "an overlay mounted but a removal did not leave a whiteout"
	}
	if os.RemoveAll(filepath.Join(m, "d")) != nil || os.Mkdir(filepath.Join(m, "d"), 0o700) != nil ||
		!opaque(filepath.Join(dir, "u", "d")) {
		return "an overlay mounted but a folder removed and made again was not marked opaque"
	}
	return ""
}

// isWhiteout: a character device 0/0, overlayfs's mark of a removed file.
func isWhiteout(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode()&fs.ModeCharDevice != 0 && st.Rdev == 0
}

// scan is a folder's files, by size, modification time, mode and what hash
// answers, .git's included. A .git file at the top is left out, as the copy
// backend leaves it out.
func scan(root string, hash workspace.Hasher) (*workspace.Snapshot, error) {
	files := workspace.Empty()
	list, truncated, err := workspace.List(root, false, workspace.MaxSnapshotFiles)
	if err != nil {
		return nil, err
	}
	if truncated {
		files.SetTruncated()
	}
	for _, it := range list {
		info := it.Info
		if info.IsDir() || it.Rel == ".git" || (info.Mode()&fs.ModeSymlink == 0 && !info.Mode().IsRegular()) {
			continue
		}
		h, err := hash(it.Rel, it.Path, info)
		if err != nil {
			continue
		}
		files.Set(it.Rel, workspace.File{Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime().UnixNano(), Hash: h})
	}
	return files, nil
}
