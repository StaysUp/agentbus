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

// applyMu keeps one apply at a time in this process, whatever folder it
// writes to: two sessions copied from one folder, or from a folder and one
// inside it, must not both pass the conflict check and then write over each
// other. An apply is short, so one lock for all of them costs little.
var applyMu sync.Mutex

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
	// A file still unhashed here is as the Base had it: a hash learned for
	// it since is its hash here too.
	for rel, f := range readLearned(s) {
		if cur, ok := snap.File(rel); ok && cur.Hash == "" && cur.Size == f.Size && cur.ModTime == f.ModTime && cur.Mode == f.Mode {
			snap.Set(rel, f)
		}
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

// keeper is how a backend reads its space's files for a checkpoint.
type keeper interface {
	// take is the space's files now: prev is the latest checkpoint, or nil
	// for the Base. A file whose size, modification time and mode match
	// prev's keeps prev's hash; any other is passed to save, which saves it
	// and answers its hash. The Base may leave hashes empty (not hashed).
	take(s *Space, prev *workspace.Snapshot, save workspace.Hasher) (*workspace.Snapshot, error)
	// learn hashes rel as the Base holds it, f, when that content can still
	// be read as it was; save it too when it is small. ok is false when it
	// cannot be read as it was.
	learn(s *Space, rel string, f workspace.File) (hash string, saved, ok bool)
}

// baseHasher hashes, for a Base walk, symbolic links and every file changed
// since a little before start (workspace.Racy: a file written in the same
// clock tick as the copy was made can change again without its time
// changing); any other file is recorded by size, modification time and mode
// only.
func baseHasher(start time.Time) workspace.Hasher {
	since := start.Add(-workspace.Racy).UnixNano()
	return func(rel, p string, info fs.FileInfo) (string, error) {
		if info.Mode()&fs.ModeSymlink == 0 && info.ModTime().UnixNano() < since {
			return "", nil
		}
		return workspace.Hash(rel, p, info)
	}
}

// checkpoint is Backend.Checkpoint, with k reading the files.
//
// What is hashed: the Base hashes nothing but symbolic links and recently
// written files (baseHasher). A later checkpoint hashes a file only when its
// size, modification time or mode changed, and hashes it once: it clones the
// file into the saved files and hashes the clone. When such a file has no
// hash yet (the Base did not hash it), learnBase hashes its Base content
// where that is still to be read, so a file only touched is not a change.
func checkpoint(s *Space, name string, k keeper) (Checkpoint, error) {
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
	var saved []string
	var save workspace.Hasher
	if len(list) > 0 {
		if prev, err = readSnapshot(s, list[len(list)-1]); err != nil {
			return Checkpoint{}, err
		}
		if err := os.MkdirAll(s.objectDir(), 0o700); err != nil {
			return Checkpoint{}, err
		}
		save = func(_, p string, _ fs.FileInfo) (string, error) {
			h, err := saveObject(p, s.objectDir())
			if err == nil {
				saved = append(saved, h)
			}
			return h, err
		}
	}
	snap, err := k.take(s, prev, save)
	if err != nil {
		return Checkpoint{}, err
	}
	if prev != nil {
		learned, err := learnBase(s, k, prev, snap)
		if err != nil {
			return Checkpoint{}, err
		}
		if len(snap.Changed(prev)) == 0 && snap.Head() == prev.Head() {
			// What was learned stays: its saved Base content is the old
			// side of the file's diff once it changes.
			dropUnlisted(s, list, saved, learned)
			return list[len(list)-1].Checkpoint, nil
		}
		saved = append(saved, learned...)
	}
	seq := 1
	if len(list) > 0 {
		seq = list[len(list)-1].Checkpoint.Seq + 1
	}
	h := header{Checkpoint: Checkpoint{Name: name, Seq: seq, Time: time.Now().UTC(), Files: snap.Len()}, Saved: dedupe(saved)}
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

// learnBase hashes the Base content of each file that changed since prev and
// that prev holds unhashed, where it is needed: a file of the same size (it
// may be only touched), or a small one (its diff shows the Base content).
// Each hash learned is set in prev and kept in the space's learned record,
// which readSnapshot reads into every checkpoint holding the file unhashed.
// A big file whose size changed stays unhashed: it changed, and reading it
// would cost a read of the whole file. It answers the saved files it made.
func learnBase(s *Space, k keeper, prev, snap *workspace.Snapshot) ([]string, error) {
	var saved []string
	var learned map[string]workspace.File
	for _, rel := range snap.Changed(prev) {
		p, ok := prev.File(rel)
		if !ok || p.Hash != "" || !p.Mode.IsRegular() {
			continue
		}
		cur, inCur := snap.File(rel)
		if p.Size > MaxFileDiff && !(inCur && cur.Size == p.Size) {
			continue
		}
		h, kept, ok := k.learn(s, rel, p)
		if !ok {
			continue
		}
		if learned == nil {
			learned = readLearned(s)
		}
		p.Hash = h
		prev.Set(rel, p)
		learned[rel] = p
		if kept {
			saved = append(saved, h)
		}
	}
	if learned == nil {
		return nil, nil
	}
	b, _ := json.Marshal(learned)
	return saved, writeFileAtomic(filepath.Join(s.state(), "learned.json"), b)
}

// readLearned is the space's learned Base hashes, by path.
func readLearned(s *Space) map[string]workspace.File {
	out := map[string]workspace.File{}
	if b, err := os.ReadFile(filepath.Join(s.state(), "learned.json")); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// learnFrom is learn for a Base whose content is the file at p while its
// size, modification time and mode are f's: hashed from a clone of it, so
// what is hashed is what is saved, and checked again after.
func learnFrom(p string, f workspace.File, objects string) (string, bool, bool) {
	same := func() bool {
		fi, err := os.Lstat(p)
		return err == nil && fi.Mode() == f.Mode && fi.Size() == f.Size && fi.ModTime().UnixNano() == f.ModTime
	}
	if !same() {
		return "", false, false
	}
	var h string
	var err error
	kept := f.Size <= MaxFileDiff
	if kept {
		if err = os.MkdirAll(objects, 0o700); err == nil {
			h, err = saveObject(p, objects)
		}
	} else {
		h, _, err = workspace.HashOf(p)
	}
	if err != nil || !same() {
		return "", false, false
	}
	return h, kept, true
}

// dropUnlisted removes the saved files a checkpoint that recorded nothing
// saved, unless a checkpoint lists them or they are kept.
func dropUnlisted(s *Space, list []header, saved, kept []string) {
	listed := map[string]bool{}
	for _, o := range kept {
		listed[o] = true
	}
	// A Base content learned before is the old side of a diff to come.
	for _, f := range readLearned(s) {
		listed[f.Hash] = true
	}
	for _, h := range list {
		for _, o := range h.Saved {
			listed[o] = true
		}
	}
	for _, o := range saved {
		if !listed[o] {
			os.Remove(filepath.Join(s.objectDir(), o))
		}
	}
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range in {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// saveObject saves the file at src under objects by its hash, and answers
// the hash: a clone where the disk has them, hashed once. A symbolic link is
// saved as a link to the same target.
func saveObject(src, objects string) (string, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return "", err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return "", err
		}
		h := linkHash(target)
		dst := filepath.Join(objects, h)
		if _, err := os.Lstat(dst); err == nil {
			return h, nil
		}
		tmp := filepath.Join(objects, ".tmp-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := os.Symlink(target, tmp); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return "", err
		}
		return h, nil
	case info.Mode().IsRegular():
		tmp, err := os.CreateTemp(objects, ".tmp-")
		if err != nil {
			return "", err
		}
		tmp.Close()
		os.Remove(tmp.Name()) // a clone needs a name that is free
		if err := cloneFile(src, tmp.Name(), info.Mode().Perm()); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		h, _, err := workspace.HashOf(tmp.Name())
		if err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		dst := filepath.Join(objects, h)
		if _, err := os.Lstat(dst); err == nil {
			os.Remove(tmp.Name())
		} else if err := os.Rename(tmp.Name(), dst); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		return h, nil
	}
	return "", errors.New("isolation: not a file")
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
// diff of an older checkpoint shows that checkpoint, not the folder now. Each
// changed text file also gets its own unified diff (addFileDiffs).
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
	addFileDiffs(s, objects, a, b, &c)
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
	if !checkpointName.MatchString(from) || !checkpointName.MatchString(to) {
		return Applied{}, ErrBadName
	}
	unlock := lock(s)
	defer unlock()
	// The check and the writes are one step for the person's folder.
	applyMu.Lock()
	defer applyMu.Unlock()
	// An apply already done answers as it did, even once its checkpoints
	// were pruned: a caller that lost the answer may ask again.
	if done, ok := readApplied(s, from, to); ok {
		return done, nil
	}
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
		// A file from never hashed is as the Base saw it, by size,
		// modification time and mode: one still so in the person's folder is
		// as at from, without reading it. Any other is hashed and compared.
		if inA && fa.Hash == "" && sameStat(rt, name, fa) {
			ops = append(ops, op{rel: rel, f: fb, remove: !inB})
			continue
		}
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
	prunes := len(list) > 0 && list[0].Checkpoint.Name == from && hb.Checkpoint.Seq > list[0].Checkpoint.Seq
	out.Pruned = prunes
	// The record is written before the prune, so a retry after a lost
	// answer finds it rather than a missing checkpoint.
	if err := writeApplied(s, from, to, out); err != nil {
		return out, err
	}
	if prunes {
		if err := prune(s, list, hb); err != nil {
			return out, err
		}
	}
	return out, nil
}

// appliedFile is where the apply from one checkpoint to another is recorded.
func appliedFile(s *Space, from, to string) string {
	sum := sha256.Sum256([]byte(from + "\x00" + to))
	return filepath.Join(s.state(), "applied", hex.EncodeToString(sum[:])+".json")
}

// readApplied is the recorded apply from from to to, if one was done.
func readApplied(s *Space, from, to string) (Applied, bool) {
	b, err := os.ReadFile(appliedFile(s, from, to))
	if err != nil {
		return Applied{}, false
	}
	var r struct {
		From, To string
		Applied  Applied
	}
	if json.Unmarshal(b, &r) != nil || r.From != from || r.To != to {
		return Applied{}, false
	}
	return r.Applied, true
}

func writeApplied(s *Space, from, to string, done Applied) error {
	p := appliedFile(s, from, to)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(struct {
		From, To string
		Applied  Applied
	}{from, to, done})
	return writeFileAtomic(p, b)
}

// sameStat says whether the file name of rt has f's size, modification time
// and mode.
func sameStat(rt *os.Root, name string, f workspace.File) bool {
	fi, err := rt.Lstat(name)
	return err == nil && fi.Mode() == f.Mode && fi.Size() == f.Size && fi.ModTime().UnixNano() == f.ModTime
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
	tmp := filepath.Join(filepath.Dir(name), ".agentbus-apply-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	// A clone of the saved file where the disk has them: no bytes copied.
	var err error
	if !cloneInto(rt, tmp, src) {
		err = copyInto(rt, tmp, src, f.Mode.Perm())
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

// copyInto writes a new file tmp of rt with src's bytes.
func copyInto(rt *os.Root, tmp, src string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := rt.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
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
