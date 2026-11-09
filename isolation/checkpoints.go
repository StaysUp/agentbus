package isolation

// Checkpoints as the copy and git backends keep them, in the space's record:
//
//	checkpoints/<seq>-<name>.json       the checkpoint, and the files it saved
//	checkpoints/<seq>-<name>.snap.json  every file's size, mode and hash
//	objects/<sha256>                    a saved file, by the hash of its content
//
// A checkpoint saves each file that changed since the one before it, so a
// later Apply can write the file as it was at that checkpoint even after
// later turns changed it again. The Base checkpoint saves nothing: it is the
// person's folder as it was, and Apply only compares against it. Saving is a
// copy-on-write clone where the disk has one.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// checkpointName is a checkpoint's name: a turn id, with .start for a turn's
// start, or Base.
var checkpointName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// header is a checkpoint's own file.
type header struct {
	Checkpoint Checkpoint `json:"checkpoint"`
	// Saved are the hashes of the files this checkpoint saved.
	Saved []string `json:"saved,omitempty"`
	file  string
}

// locks keeps one change of a space at a time, in this process.
var locks sync.Map // state folder -> *sync.Mutex

func lock(s *Space) func() {
	m, _ := locks.LoadOrStore(s.state(), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Space) checkpointDir() string { return filepath.Join(s.state(), "checkpoints") }
func (s *Space) objectDir() string     { return filepath.Join(s.state(), "objects") }

// headers lists s's checkpoints, oldest first.
func headers(s *Space) ([]header, error) {
	entries, err := os.ReadDir(s.checkpointDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []header
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".json") || strings.HasSuffix(n, ".snap.json") || strings.HasPrefix(n, ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.checkpointDir(), n))
		if err != nil {
			return nil, err
		}
		var h header
		if err := json.Unmarshal(b, &h); err != nil {
			return nil, errors.New("isolation: a checkpoint could not be read")
		}
		h.file = strings.TrimSuffix(n, ".json")
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Checkpoint.Seq < out[j].Checkpoint.Seq })
	return out, nil
}

func find(list []header, name string) (header, bool) {
	for _, h := range list {
		if h.Checkpoint.Name == name {
			return h, true
		}
	}
	return header{}, false
}

func readSnapshot(s *Space, h header) (*workspace.Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(s.checkpointDir(), h.file+".snap.json"))
	if err != nil {
		return nil, errors.New("isolation: a checkpoint's files could not be read")
	}
	var snap workspace.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, errors.New("isolation: a checkpoint's files could not be read")
	}
	return &snap, nil
}

// checkpoints is Backend.Checkpoints.
func checkpoints(s *Space) ([]Checkpoint, error) {
	list, err := headers(s)
	if err != nil {
		return nil, err
	}
	out := make([]Checkpoint, len(list))
	for i, h := range list {
		out[i] = h.Checkpoint
	}
	return out, nil
}

// checkpoint is Backend.Checkpoint.
func checkpoint(s *Space, name string) (Checkpoint, error) {
	if !checkpointName.MatchString(name) {
		return Checkpoint{}, ErrBadName
	}
	unlock := lock(s)
	defer unlock()
	list, err := headers(s)
	if err != nil {
		return Checkpoint{}, err
	}
	if h, ok := find(list, name); ok {
		return h.Checkpoint, nil
	}
	var prev *workspace.Snapshot
	if len(list) > 0 {
		if prev, err = readSnapshot(s, list[len(list)-1]); err != nil {
			return Checkpoint{}, err
		}
	}
	snap, err := workspace.Take(s.Dir, prev)
	if err != nil {
		return Checkpoint{}, err
	}
	var saved []string
	if prev != nil {
		changed := snap.Changed(prev)
		if len(changed) == 0 && snap.Head() == prev.Head() {
			return list[len(list)-1].Checkpoint, nil
		}
		if saved, err = saveFiles(s, snap, changed); err != nil {
			return Checkpoint{}, err
		}
	}
	seq := 1
	if len(list) > 0 {
		seq = list[len(list)-1].Checkpoint.Seq + 1
	}
	h := header{Checkpoint: Checkpoint{Name: name, Seq: seq, Time: time.Now().UTC(), Files: snap.Len()}, Saved: saved}
	if err := os.MkdirAll(s.checkpointDir(), 0o700); err != nil {
		return Checkpoint{}, err
	}
	file := fmt.Sprintf("%08d-%s", seq, name)
	sb, _ := json.Marshal(snap)
	if err := writeFileAtomic(filepath.Join(s.checkpointDir(), file+".snap.json"), sb); err != nil {
		return Checkpoint{}, err
	}
	// The header is written last: a checkpoint exists once it does.
	hb, _ := json.Marshal(h)
	if err := writeFileAtomic(filepath.Join(s.checkpointDir(), file+".json"), hb); err != nil {
		return Checkpoint{}, err
	}
	return h.Checkpoint, nil
}

// saveFiles saves each changed file that snap holds, and records each one's
// hash as its saved copy has it, so the checkpoint and its saved files never
// disagree when a file changes during the walk. A file gone by the time it
// is saved is left out of snap.
func saveFiles(s *Space, snap *workspace.Snapshot, changed []string) ([]string, error) {
	if err := os.MkdirAll(s.objectDir(), 0o700); err != nil {
		return nil, err
	}
	var saved []string
	for _, rel := range changed {
		f, ok := snap.File(rel)
		if !ok {
			continue // deleted: nothing to save
		}
		h, size, err := saveObject(filepath.Join(s.Dir, filepath.FromSlash(rel)), s.objectDir())
		if err != nil {
			snap.Delete(rel)
			continue
		}
		if h != f.Hash || size != f.Size {
			f.Hash, f.Size = h, size
			snap.Set(rel, f)
		}
		saved = append(saved, h)
	}
	return saved, nil
}

// saveObject saves the file at src under objects by its hash, and answers
// the hash and size. A symbolic link is saved as a link to the same target.
func saveObject(src, objects string) (string, int64, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return "", 0, err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return "", 0, err
		}
		h := linkHash(target)
		dst := filepath.Join(objects, h)
		if _, err := os.Lstat(dst); err == nil {
			return h, info.Size(), nil
		}
		tmp := filepath.Join(objects, ".tmp-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := os.Symlink(target, tmp); err != nil {
			return "", 0, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return "", 0, err
		}
		return h, info.Size(), nil
	case info.Mode().IsRegular():
		tmp, err := os.CreateTemp(objects, ".tmp-")
		if err != nil {
			return "", 0, err
		}
		tmp.Close()
		os.Remove(tmp.Name()) // a clone needs a name that is free
		if err := cloneFile(src, tmp.Name(), info.Mode().Perm()); err != nil {
			os.Remove(tmp.Name())
			return "", 0, err
		}
		h, _, err := workspace.HashOf(tmp.Name())
		if err != nil {
			os.Remove(tmp.Name())
			return "", 0, err
		}
		st, err := os.Stat(tmp.Name())
		if err != nil {
			os.Remove(tmp.Name())
			return "", 0, err
		}
		dst := filepath.Join(objects, h)
		if _, err := os.Lstat(dst); err == nil {
			os.Remove(tmp.Name())
		} else if err := os.Rename(tmp.Name(), dst); err != nil {
			os.Remove(tmp.Name())
			return "", 0, err
		}
		return h, st.Size(), nil
	}
	return "", 0, errors.New("isolation: not a file")
}

// linkHash is a symbolic link's hash, as workspace.Take makes it.
func linkHash(target string) string {
	sum := sha256.Sum256([]byte("link:" + target))
	return hex.EncodeToString(sum[:])
}

// pair reads the checkpoints from and to, from no later than to.
func pair(s *Space, list []header, from, to string) (a, b *workspace.Snapshot, ha, hb header, err error) {
	var ok bool
	if ha, ok = find(list, from); !ok {
		return nil, nil, ha, hb, ErrNoCheckpoint
	}
	if hb, ok = find(list, to); !ok {
		return nil, nil, ha, hb, ErrNoCheckpoint
	}
	if ha.Checkpoint.Seq > hb.Checkpoint.Seq {
		return nil, nil, ha, hb, ErrOrder
	}
	if a, err = readSnapshot(s, ha); err != nil {
		return nil, nil, ha, hb, err
	}
	if b, err = readSnapshot(s, hb); err != nil {
		return nil, nil, ha, hb, err
	}
	return a, b, ha, hb, nil
}

// diff is Backend.Diff. A file's content comes from its saved copy, so the
// diff of an older checkpoint shows that checkpoint, not the folder now.
func diff(s *Space, from, to string) (workspace.Changes, error) {
	unlock := lock(s)
	defer unlock()
	list, err := headers(s)
	if err != nil {
		return workspace.Changes{}, err
	}
	a, b, ha, hb, err := pair(s, list, from, to)
	if err != nil {
		return workspace.Changes{}, err
	}
	objects, err := os.OpenRoot(s.objectDir())
	if err != nil {
		objects = nil
	} else {
		defer objects.Close()
	}
	read := func(rel string, max int) ([]byte, bool) {
		f, ok := b.File(rel)
		if !ok || objects == nil {
			return nil, false
		}
		return readRegular(objects, f.Hash, max)
	}
	c := workspace.DiffRead(s.Dir, a, b, read)
	c.From, c.To = ha.Checkpoint.Name, hb.Checkpoint.Name
	return c, nil
}

// readRegular reads a regular file of rt, at most max bytes.
func readRegular(rt *os.Root, name string, max int) ([]byte, bool) {
	if fi, err := rt.Lstat(name); err != nil || !fi.Mode().IsRegular() || fi.Size() > int64(max) {
		return nil, false
	}
	f, err := rt.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil || len(b) > max {
		return nil, false
	}
	return b, true
}

// Why a file conflicts, in the refusal.
const (
	whyChanged = "changed in your folder since the checkpoint"
	whyAdded   = "added in your folder since the checkpoint"
	whyRemoved = "removed from your folder since the checkpoint"
	whyNotFile = "is a folder or cannot be read in your folder"
)

// apply is Backend.Apply.
func apply(s *Space, from, to string) (Applied, error) {
	if s.Source == "" {
		return Applied{}, ErrNoSource
	}
	unlock := lock(s)
	defer unlock()
	list, err := headers(s)
	if err != nil {
		return Applied{}, err
	}
	a, b, _, hb, err := pair(s, list, from, to)
	if err != nil {
		return Applied{}, err
	}
	if a.Truncated() || b.Truncated() {
		return Applied{}, ErrTruncated
	}
	// Every write goes through the person's folder as a root, so no
	// symbolic link in it can take a write outside it.
	rt, err := os.OpenRoot(s.Source)
	if err != nil {
		return Applied{}, errors.New("isolation: your folder could not be opened")
	}
	defer rt.Close()
	type op struct {
		rel    string
		f      workspace.File
		remove bool
	}
	var ops []op
	var conflict Conflict
	for _, rel := range b.Changed(a) {
		fa, inA := a.File(rel)
		fb, inB := b.File(rel)
		name := filepath.FromSlash(rel)
		cur, mode, exists, err := hashIn(rt, name)
		switch {
		case err != nil:
			conflict.Files = append(conflict.Files, ConflictFile{rel, whyNotFile})
		case inB && exists && cur == fb.Hash:
			if mode.IsRegular() && mode.Perm() != fb.Mode.Perm() {
				ops = append(ops, op{rel: rel, f: fb})
			}
		case !inB && !exists:
		case inA && exists && cur == fa.Hash, !inA && !exists:
			ops = append(ops, op{rel: rel, f: fb, remove: !inB})
		case !inA:
			conflict.Files = append(conflict.Files, ConflictFile{rel, whyAdded})
		case !exists:
			conflict.Files = append(conflict.Files, ConflictFile{rel, whyRemoved})
		default:
			conflict.Files = append(conflict.Files, ConflictFile{rel, whyChanged})
		}
	}
	if len(conflict.Files) > 0 {
		return Applied{}, &conflict
	}
	out := Applied{Files: []string{}}
	for _, o := range ops {
		var err error
		if o.remove {
			err = rt.Remove(filepath.FromSlash(o.rel))
			if errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		} else {
			err = writeObject(rt, s.objectDir(), filepath.FromSlash(o.rel), o.f)
		}
		if err != nil {
			sort.Strings(out.Files)
			return out, fmt.Errorf("isolation: %s could not be written; the files before it were applied", o.rel)
		}
		out.Files = append(out.Files, o.rel)
	}
	sort.Strings(out.Files)
	if len(list) > 0 && list[0].Checkpoint.Name == from && hb.Checkpoint.Seq > list[0].Checkpoint.Seq {
		if err := prune(s, list, hb); err != nil {
			return out, err
		}
		out.Pruned = true
	}
	return out, nil
}

// hashIn hashes the file name of rt as workspace.Take does. A missing file
// is not an error: exists is false.
func hashIn(rt *os.Root, name string) (hash string, mode fs.FileMode, exists bool, err error) {
	fi, err := rt.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := rt.Readlink(name)
		if err != nil {
			return "", 0, false, err
		}
		return linkHash(target), fi.Mode(), true, nil
	case fi.Mode().IsRegular():
		f, err := rt.Open(name)
		if err != nil {
			return "", 0, false, err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", 0, false, err
		}
		return hex.EncodeToString(h.Sum(nil)), fi.Mode(), true, nil
	}
	return "", 0, false, errors.New("not a file")
}

// writeObject writes the saved file f at name of rt, by rename, so a reader
// never sees half a file.
func writeObject(rt *os.Root, objects, name string, f workspace.File) error {
	if dir := filepath.Dir(name); dir != "." {
		if err := rt.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	src := filepath.Join(objects, f.Hash)
	if f.Mode&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := rt.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return rt.Symlink(target, name)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(name), ".agentbus-apply-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	out, err := rt.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.Mode.Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = rt.Chmod(tmp, f.Mode.Perm()) // the umask narrowed it
	}
	if err == nil {
		err = rt.Rename(tmp, name)
	}
	if err != nil {
		rt.Remove(tmp)
	}
	return err
}

// prune removes the checkpoints before to, and every saved file no later
// checkpoint saved: an apply from the oldest checkpoint makes to the oldest.
func prune(s *Space, list []header, to header) error {
	needed := map[string]bool{}
	for _, h := range list {
		if h.Checkpoint.Seq > to.Checkpoint.Seq {
			for _, o := range h.Saved {
				needed[o] = true
			}
		}
	}
	for _, h := range list {
		if h.Checkpoint.Seq > to.Checkpoint.Seq {
			continue
		}
		for _, o := range h.Saved {
			if !needed[o] {
				if err := os.Remove(filepath.Join(s.objectDir(), o)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
		if h.Checkpoint.Seq == to.Checkpoint.Seq {
			continue
		}
		for _, ext := range []string{".json", ".snap.json"} {
			if err := os.Remove(filepath.Join(s.checkpointDir(), h.file+ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	// to keeps its files' hashes, and no longer its saved files.
	to.Saved = nil
	b, _ := json.Marshal(to)
	return writeFileAtomic(filepath.Join(s.checkpointDir(), to.file+".json"), b)
}

// removeState removes s's record.
func removeState(s *Space) error { return removeAll(s.state()) }

// removeAll is os.RemoveAll that also removes what a read-only folder holds,
// as a copied module cache has.
func removeAll(p string) error {
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(q, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}
