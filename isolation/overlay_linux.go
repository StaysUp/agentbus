package isolation

// The overlay backend: a session's copy is an overlayfs mount, read-only
// layers of the person's folder below and an empty folder of its own above
// (the upper layer) that takes every write. Making a copy writes no file of
// the folder, so ten copies cost ten mounts.
//
// The layers are kept once per runner, under the space records:
//
//	.isolation/.l/<n>/          a layer: a copy of the person's folder (a
//	                            base), or what changed in it since the
//	                            layer below (a delta), with whiteouts for
//	                            what was removed
//	.isolation/.l/<n>.snap.json the folder as the stack up to <n> shows it
//	.isolation/.l/<n>.json      the layer's record (layerRec), written last
//	.isolation/<id>/u, <id>/w   a space's upper layer and overlay work folder
//
// The person's live folder is never a layer: a write there would change
// every copy at once. The first copy of a folder makes a base, a copy of
// it (a reflink per file where the disk has them). A later copy compares
// the folder with the newest stack by size, modification time and mode, and
// reuses that stack when nothing changed, or puts the changes in a new
// delta layer above it. Past maxLayers the next copy makes a new base
// instead: the folder itself is the stack merged, so the base is a copy of
// it. Layers no space uses are removed when a space closes.
//
// A checkpoint walks the upper layer only: every change since the copy was
// made is there, a removed file as a whiteout. The checkpoint's record is a
// manifest and saved files, as the copy backend keeps them, so Diff and
// Apply are the same code. Nothing is unmounted at a checkpoint, so a
// process the agent left running cannot stop one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/staysup/agentbus/workspace"
)

const (
	// MethodOverlay is how an overlay space's copy was made.
	MethodOverlay = "overlay"
	// maxLayers: a copy past it makes a new base rather than one more
	// delta. The kernel allows 500; mounts were measured to 200.
	maxLayers = 16
	// maxOptions bounds the mount options. Before Linux 6.5 the kernel
	// copies them into one 4 KiB page and cuts a longer string without an
	// error, so a long lowerdir list mounts the wrong folders. Layer names
	// are short and relative, so a stack of maxLayers needs a few hundred
	// bytes; past this the mount is refused, never cut.
	maxOptions = 3072
	layerDir   = ".l"
)

// overlayBackend makes each copy an overlay mount. It needs a runner that
// can mount: root, or CAP_SYS_ADMIN. Open answers Unavailable, with why,
// where it cannot, and For falls back to copy.
type overlayBackend struct{}

func (overlayBackend) Name() string { return Overlay }

// storeMu keeps the layer store to one change at a time: choosing or making
// a stack and saving the space that uses it are one step, so the clean-up
// after a Close never removes a layer a new space is about to use.
var storeMu sync.Mutex

// layerRec is a layer's record.
type layerRec struct {
	Seq    int    `json:"seq"`
	Source string `json:"source"`
	// Stack: the layers this one tops, the lowest (the base) first, ending
	// with this one.
	Stack []int `json:"stack"`
	// Dirs: every folder the stack shows, with its mode as the person's
	// folder had it, so a folder whose mode changed gets a new layer.
	Dirs map[string]fs.FileMode `json:"dirs"`
	// Where: a file a delta layer holds, and that layer. Any other file is
	// in the base.
	Where map[string]int `json:"where,omitempty"`
}

func storeDir(workdir string) string { return filepath.Join(workdir, stateFolder) }
func layerPath(workdir string, n int) string {
	return filepath.Join(storeDir(workdir), layerDir, strconv.Itoa(n))
}

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
	storeMu.Lock()
	rec, err := stackFor(r.Workdir, s.Source)
	if err == nil {
		s.Layers = rec.Stack
		err = os.MkdirAll(s.state(), 0o700)
	}
	if err == nil {
		// The upper layer's own folder is the copy's top folder: it has
		// the person's folder's mode.
		err = os.Mkdir(filepath.Join(s.state(), "u"), rootInfo.Mode().Perm())
	}
	if err == nil {
		err = os.Chmod(filepath.Join(s.state(), "u"), rootInfo.Mode().Perm())
	}
	if err == nil {
		err = os.Mkdir(filepath.Join(s.state(), "w"), 0o700)
	}
	if err == nil {
		err = s.save()
	}
	storeMu.Unlock()
	if err != nil {
		removeState(s)
		storeMu.Lock()
		gcLayers(r.Workdir)
		storeMu.Unlock()
		return nil, err
	}
	if err := mountSpace(s); err != nil {
		overlayBackend{}.Close(s)
		return nil, err
	}
	if _, err := checkpoint(s, Base, overlayBackend{}); err != nil {
		overlayBackend{}.Close(s)
		return nil, err
	}
	return s, nil
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
	storeMu.Lock()
	defer storeMu.Unlock()
	return gcLayers(s.Workdir)
}

// take is the copy's files: the Base is the stack's own manifest, and a
// later checkpoint is that manifest with the upper layer laid over it.
func (overlayBackend) take(s *Space, prev *workspace.Snapshot, save workspace.Hasher) (*workspace.Snapshot, error) {
	rec, err := readLayer(s.Workdir, s.Layers[len(s.Layers)-1])
	if err != nil {
		return nil, err
	}
	view, err := readView(s.Workdir, rec.Seq)
	if err != nil {
		return nil, err
	}
	// Git's own state is read through the mount, as Walk reads it.
	if prev == nil {
		view.ReadGit(s.Dir)
		return view, nil
	}
	view.SetTaken(time.Now())
	defer view.ReadGit(s.Dir)
	dirs := rec.Dirs
	upper := filepath.Join(s.state(), "u")
	fromUpper := map[string]bool{}
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
			// A folder made over a removed one hides all the lower held.
			if opaque(p) {
				view.DeleteUnder(rel)
			}
			view.Delete(rel)
		case mode.IsRegular() || mode&fs.ModeSymlink != 0:
			if _, ok := dirs[rel]; ok {
				view.DeleteUnder(rel)
			}
			f := workspace.File{Size: info.Size(), Mode: mode, ModTime: info.ModTime().UnixNano()}
			if old, ok := prev.File(rel); ok && old.Size == f.Size && old.ModTime == f.ModTime && old.Mode == f.Mode && !prev.Racy(old.ModTime) {
				f.Hash = old.Hash
			} else {
				h, err := save(rel, p, info)
				if err != nil {
					view.Delete(rel)
					return nil
				}
				f.Hash = h
			}
			view.Set(rel, f)
			fromUpper[rel] = true
		default:
			// A whiteout (a removed file), or a device, a socket or a pipe
			// the turn made: either hides what the lower held there, and
			// neither is a file of the work.
			view.Delete(rel)
			if _, ok := dirs[rel]; ok {
				view.DeleteUnder(rel)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A file the turns never wrote keeps the hash an earlier checkpoint
	// learned for it.
	for _, rel := range view.Paths() {
		if fromUpper[rel] {
			continue
		}
		f, _ := view.File(rel)
		if old, ok := prev.File(rel); ok && f.Hash == "" && old.Size == f.Size && old.ModTime == f.ModTime && old.Mode == f.Mode {
			view.Set(rel, old)
		}
	}
	return view, nil
}

// learn reads rel's Base content from the layer that holds it: layers never
// change, so it is always there.
func (overlayBackend) learn(s *Space, rel string, f workspace.File) (string, bool, bool) {
	rec, err := readLayer(s.Workdir, s.Layers[len(s.Layers)-1])
	if err != nil {
		return "", false, false
	}
	n, ok := rec.Where[rel]
	if !ok {
		n = rec.Stack[0]
	}
	return learnFrom(filepath.Join(layerPath(s.Workdir, n), filepath.FromSlash(rel)), f, s.objectDir())
}

// opaque says whether the upper folder p hides what the layers below hold.
func opaque(p string) bool {
	b := make([]byte, 4)
	n, err := unix.Lgetxattr(p, "trusted.overlay.opaque", b)
	return err == nil && n == 1 && b[0] == 'y'
}

// mountSpace mounts s's stack and upper layer at s.Root, unless they are.
func mountSpace(s *Space) error {
	if mounted(s.Root) {
		return nil
	}
	if len(s.Layers) == 0 || len(s.Layers) > maxLayers {
		return errors.New("isolation: the copy's layers cannot be mounted: too many")
	}
	lower := make([]string, len(s.Layers))
	for i, n := range s.Layers {
		lower[len(s.Layers)-1-i] = layerDir + "/" + strconv.Itoa(n) // the top first
	}
	// Paths are relative to the store, so the options stay short.
	opts := "lowerdir=" + strings.Join(lower, ":") + ",upperdir=" + s.ID + "/u,workdir=" + s.ID + "/w," + overlayOptions
	if len(opts) > maxOptions {
		return fmt.Errorf("isolation: the copy's mount options are %d bytes, past the %d this runner allows", len(opts), maxOptions)
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	return mountIn(storeDir(s.Workdir), s.Root, opts)
}

// overlayOptions keep the upper layer plain: a renamed folder is copied, not
// redirected, and a changed file is copied whole, so the upper layer alone
// says what changed.
const overlayOptions = "redirect_dir=off,index=off,metacopy=off"

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
	if err := os.WriteFile(filepath.Join(dir, "l", "f"), []byte("f"), 0o600); err != nil {
		return "the workdir cannot be written"
	}
	err := mountIn(dir, m, "lowerdir=l,upperdir=u,workdir=w,"+overlayOptions)
	switch {
	case err == unix.EPERM || err == unix.EACCES:
		return fmt.Sprintf("mounting an overlay needs root or CAP_SYS_ADMIN, and this runner runs as uid %d "+
			"(a user namespace would hide the mount from the agent's processes)", os.Geteuid())
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
	return ""
}

// isWhiteout: a character device 0/0, overlayfs's mark of a removed file.
func isWhiteout(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode()&fs.ModeCharDevice != 0 && st.Rdev == 0
}

// scan is a folder's files, by size, modification time, mode and what hash
// answers, and its folders, .git's included. A .git file at the top is left
// out, as the copy backend leaves it out.
func scan(root string, hash workspace.Hasher) (*workspace.Snapshot, map[string]fs.FileMode, error) {
	files := workspace.Empty()
	dirs := map[string]fs.FileMode{}
	list, truncated, err := workspace.List(root, false, workspace.MaxSnapshotFiles)
	if err != nil {
		return nil, nil, err
	}
	if truncated {
		files.SetTruncated()
	}
	for _, it := range list {
		info := it.Info
		if info.IsDir() {
			dirs[it.Rel] = info.Mode()
			continue
		}
		if it.Rel == ".git" || (info.Mode()&fs.ModeSymlink == 0 && !info.Mode().IsRegular()) {
			continue
		}
		h, err := hash(it.Rel, it.Path, info)
		if err != nil {
			continue
		}
		files.Set(it.Rel, workspace.File{Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime().UnixNano(), Hash: h})
	}
	return files, dirs, nil
}

// stackFor is the stack a new copy of source mounts: the newest one of
// source when the folder still is as it shows, a new delta on it when not,
// or a new base. storeMu is held.
func stackFor(workdir, source string) (*layerRec, error) {
	start := time.Now()
	top := newestStack(workdir, source)
	if top == nil || len(top.Stack) >= maxLayers {
		return newBase(workdir, source, start)
	}
	view, err := readManifest(workdir, top.Seq)
	if err != nil {
		return newBase(workdir, source, start)
	}
	// Compared by size, time and mode only: links are hashed, being cheap.
	cur, dirs, err := scan(source, linksOnly)
	if err != nil {
		return nil, err
	}
	if cur.Truncated() || view.Truncated() {
		return newBase(workdir, source, start)
	}
	var changed, removed []string
	for _, rel := range cur.Paths() {
		f, _ := cur.File(rel)
		if old, ok := view.File(rel); !ok || old.Size != f.Size || old.ModTime != f.ModTime || old.Mode != f.Mode {
			changed = append(changed, rel)
		}
	}
	for _, rel := range view.Paths() {
		if _, ok := cur.File(rel); !ok {
			removed = append(removed, rel)
		}
	}
	for d := range top.Dirs {
		if _, ok := dirs[d]; !ok {
			removed = append(removed, d)
		}
	}
	// A new folder, or one whose mode changed: the delta holds it, with
	// the mode the person's folder has now.
	var added []string
	for d, mode := range dirs {
		if old, ok := top.Dirs[d]; !ok || old != mode {
			added = append(added, d)
		}
	}
	if len(changed) == 0 && len(removed) == 0 && len(added) == 0 {
		return top, nil
	}
	return newDelta(workdir, source, top, view, cur, dirs, changed, removed, added, start)
}

// newestStack is source's stack with the highest number, or nil.
func newestStack(workdir, source string) *layerRec {
	var top *layerRec
	for _, n := range layerNumbers(workdir) {
		rec, err := readLayer(workdir, n)
		if err != nil || rec.Source != source {
			continue
		}
		if top == nil || rec.Seq > top.Seq {
			top = rec
		}
	}
	return top
}

// layerNumbers are the layers that have a record, ascending.
func layerNumbers(workdir string) []int {
	entries, _ := os.ReadDir(filepath.Join(storeDir(workdir), layerDir))
	var out []int
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasSuffix(name, ".snap") {
			continue
		}
		if n, err := strconv.Atoi(name); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// nextLayer is a number no layer, record or not, has.
func nextLayer(workdir string) int {
	entries, _ := os.ReadDir(filepath.Join(storeDir(workdir), layerDir))
	n := 0
	for _, e := range entries {
		name, _, _ := strings.Cut(e.Name(), ".")
		if v, err := strconv.Atoi(name); err == nil && v > n {
			n = v
		}
	}
	return n + 1
}

// layerCache caches the layer records read: a layer never changes once its
// record is written. Never change a record read from it.
var layerCache sync.Map // layerPath -> *layerRec

func readLayer(workdir string, n int) (*layerRec, error) {
	if v, ok := layerCache.Load(layerPath(workdir, n)); ok {
		return v.(*layerRec), nil
	}
	b, err := os.ReadFile(layerPath(workdir, n) + ".json")
	if err != nil {
		return nil, err
	}
	var rec layerRec
	if err := json.Unmarshal(b, &rec); err != nil || rec.Seq != n || len(rec.Stack) == 0 {
		return nil, errors.New("isolation: a layer's record could not be read")
	}
	layerCache.Store(layerPath(workdir, n), &rec)
	return &rec, nil
}

// linksOnly hashes links, and records every other file unhashed.
func linksOnly(rel, p string, info fs.FileInfo) (string, error) {
	if info.Mode()&fs.ModeSymlink == 0 {
		return "", nil
	}
	return workspace.Hash(rel, p, info)
}

// readView is the files the stack up to layer n shows, but .git's, as
// workspace.Take leaves .git out: what the copy's Base checkpoint holds.
func readView(workdir string, n int) (*workspace.Snapshot, error) {
	snap, err := readManifest(workdir, n)
	if err != nil {
		return nil, err
	}
	snap.Delete(".git")
	snap.DeleteUnder(".git")
	return snap, nil
}

// readManifest is the files the stack up to layer n shows.
func readManifest(workdir string, n int) (*workspace.Snapshot, error) {
	b, err := os.ReadFile(layerPath(workdir, n) + ".snap.json")
	if err != nil {
		return nil, errors.New("isolation: a layer's files could not be read")
	}
	var snap workspace.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, errors.New("isolation: a layer's files could not be read")
	}
	return &snap, nil
}

// writeLayer writes rec's files and then its record: a layer exists once
// its record does.
func writeLayer(workdir string, rec *layerRec, view *workspace.Snapshot) error {
	b, _ := json.Marshal(view)
	if err := writeFileAtomic(layerPath(workdir, rec.Seq)+".snap.json", b); err != nil {
		return err
	}
	b, _ = json.Marshal(rec)
	return writeFileAtomic(layerPath(workdir, rec.Seq)+".json", b)
}

// newBase copies source into a new layer: a reflink per file where the
// disk has them. Its manifest is read from the copy, so it says what the
// layer holds, whatever changed in the folder meanwhile.
func newBase(workdir, source string, start time.Time) (*layerRec, error) {
	n := nextLayer(workdir)
	dir := layerPath(workdir, n)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	if _, err := copyTree(source, dir); err != nil {
		removeAll(dir)
		return nil, err
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && !fi.IsDir() {
		os.Remove(filepath.Join(dir, ".git"))
	}
	view, dirs, err := scan(dir, baseHasher(start))
	if err != nil {
		removeAll(dir)
		return nil, err
	}
	rec := &layerRec{Seq: n, Source: source, Stack: []int{n}, Dirs: dirs}
	if err := writeLayer(workdir, rec, view); err != nil {
		removeAll(dir)
		return nil, err
	}
	return rec, nil
}

// newDelta puts what changed in source since top into a new layer above it:
// each changed file copied (a reflink where the disk has them), each
// removed file or folder a whiteout. removed holds files and folders.
func newDelta(workdir, source string, top *layerRec, view, cur *workspace.Snapshot, dirs map[string]fs.FileMode,
	changed, removed, added []string, start time.Time) (*layerRec, error) {
	n := nextLayer(workdir)
	dir := layerPath(workdir, n)
	fail := func(err error) (*layerRec, error) {
		removeAll(dir)
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	// Folders the layer holds take the person's folder's mode and time once
	// filled: in an overlay the highest layer's folder is the one shown.
	made := map[string]bool{"": true}
	mkdirs := func(rel string) error {
		if rel == "." || rel == "" || made[rel] {
			return nil
		}
		made[rel] = true
		for p := filepath.Dir(rel); p != "." && !made[filepath.ToSlash(p)]; p = filepath.Dir(p) {
			made[filepath.ToSlash(p)] = true
		}
		return os.MkdirAll(filepath.Join(dir, filepath.FromSlash(rel)), 0o700)
	}
	sort.Strings(removed)
	gone := map[string]bool{} // removed folders: what was under them is gone with them
	under := func(rel string) bool {
		for p := filepath.Dir(rel); p != "."; p = filepath.Dir(p) {
			if gone[filepath.ToSlash(p)] {
				return true
			}
		}
		return false
	}
	for _, rel := range removed {
		if under(rel) {
			continue
		}
		// A removed folder's files go with it, even when a file now has
		// its name: no whiteout under it may make it a folder again.
		if _, wasFile := view.File(rel); !wasFile {
			gone[rel] = true
		}
		_, isFile := cur.File(rel)
		if _, isDir := dirs[rel]; isFile || isDir {
			continue // made again as a file or a folder: that hides the old one
		}
		if err := mkdirs(filepath.ToSlash(filepath.Dir(rel))); err != nil {
			return fail(err)
		}
		if err := unix.Mknod(filepath.Join(dir, filepath.FromSlash(rel)), unix.S_IFCHR, 0); err != nil {
			return fail(err)
		}
	}
	for _, d := range added {
		if err := mkdirs(d); err != nil {
			return fail(err)
		}
	}
	since := start.Add(-workspace.Racy).UnixNano()
	where := map[string]int{}
	for rel, l := range top.Where {
		where[rel] = l
	}
	for _, rel := range removed {
		delete(where, rel)
		view.Delete(rel)
		view.DeleteUnder(rel)
	}
	for rel := range where {
		if _, ok := cur.File(rel); !ok {
			delete(where, rel)
		}
	}
	for _, rel := range changed {
		f, _ := cur.File(rel)
		src := filepath.Join(source, filepath.FromSlash(rel))
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := mkdirs(filepath.ToSlash(filepath.Dir(rel))); err != nil {
			return fail(err)
		}
		if f.Mode&fs.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err != nil {
				continue // gone since the scan: the next copy sees it
			}
			if err := os.Symlink(target, dst); err != nil {
				return fail(err)
			}
		} else {
			if _, err := copyFile(src, dst, f.Mode.Perm(), true); err != nil {
				if errors.Is(err, errUnreadable) {
					continue
				}
				return fail(err)
			}
			mt := time.Unix(0, f.ModTime)
			_ = os.Chtimes(dst, mt, mt)
			// What the layer holds is what the manifest says: read again.
			fi, err := os.Lstat(dst)
			if err != nil {
				return fail(err)
			}
			f = workspace.File{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime().UnixNano()}
			if f.ModTime >= since {
				if f.Hash, _, err = workspace.HashOf(dst); err != nil {
					return fail(err)
				}
			}
		}
		view.Set(rel, f)
		where[rel] = n
	}
	// The folders made, deepest first, with the person's folders' modes.
	var mk []string
	for rel := range made {
		mk = append(mk, rel)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(mk)))
	for _, rel := range mk {
		info, err := os.Stat(filepath.Join(source, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.Chmod(p, info.Mode().Perm())
		_ = os.Chtimes(p, info.ModTime(), info.ModTime())
	}
	rec := &layerRec{Seq: n, Source: source, Stack: append(append([]int{}, top.Stack...), n), Dirs: dirs, Where: where}
	if err := writeLayer(workdir, rec, view); err != nil {
		return fail(err)
	}
	return rec, nil
}

// gcLayers removes the layers no space's stack holds. storeMu is held.
func gcLayers(workdir string) error {
	used := map[int]bool{}
	entries, _ := os.ReadDir(storeDir(workdir))
	for _, e := range entries {
		if !e.IsDir() || !spaceName.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(storeDir(workdir), e.Name(), "space.json"))
		if err != nil {
			continue
		}
		var s Space
		if json.Unmarshal(b, &s) == nil {
			for _, n := range s.Layers {
				used[n] = true
			}
		}
	}
	layers, _ := os.ReadDir(filepath.Join(storeDir(workdir), layerDir))
	for _, e := range layers {
		name, _, _ := strings.Cut(e.Name(), ".")
		n, err := strconv.Atoi(name)
		if err != nil || used[n] {
			continue
		}
		// The record first: a layer without one is never used again.
		layerCache.Delete(layerPath(workdir, n))
		os.Remove(layerPath(workdir, n) + ".json")
		if err := removeAll(filepath.Join(storeDir(workdir), layerDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
