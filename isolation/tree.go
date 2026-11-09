package isolation

// Checkpoints as a Merkle tree, in the space's record:
//
//	trees/<n>.pack  the trees one checkpoint made, each by its hash. A tree
//	                is one folder: its files, each with its size,
//	                modification time, mode and hash, and its folders, each
//	                by the hash of its own tree and the count of files
//	                under it
//
// A pack per checkpoint, not a file per tree: the Base of a 50,000-file
// project has some 16,000 folders, and one file each cost more than the
// rest of the checkpoint (measured on APFS, 2026-11-09).
//
// A checkpoint is the hash of its top folder's tree. Equal hashes are equal
// folders, so a checkpoint after a few edits writes only those files'
// folders and the folders above them, and a diff of two checkpoints walks
// only the folders whose hashes differ. A folder with no file under it is
// not in a tree, as a Snapshot holds no folder.
//
// Trees never change once written: a hash names the same tree in any space,
// so this process keeps the trees it read (treeCache).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/staysup/agentbus/workspace"
)

// tentry is one entry of a folder's tree: a file, or a folder by its tree.
type tentry struct {
	name  string
	dir   bool
	hash  string         // a folder's tree
	count int            // a folder: the files under it
	f     workspace.File // a file
}

// encodeTree writes es, sorted by name, one entry each:
//
//	f <mode> <size> <mtime> <sha256 or -> <name>\x00
//	d 0 <count> 0 <tree> <name>\x00
//
// A name holds no NUL and no slash, so it ends the entry.
func encodeTree(es []tentry) []byte {
	var b strings.Builder
	for _, e := range es {
		if e.dir {
			fmt.Fprintf(&b, "d 0 %d 0 %s %s\x00", e.count, e.hash, e.name)
			continue
		}
		h := e.f.Hash
		if h == "" {
			h = "-"
		}
		fmt.Fprintf(&b, "f %d %d %d %s %s\x00", uint32(e.f.Mode), e.f.Size, e.f.ModTime, h, e.name)
	}
	return []byte(b.String())
}

var errTree = errors.New("isolation: a checkpoint's files could not be read")

func decodeTree(b []byte) ([]tentry, error) {
	var out []tentry
	for _, rec := range strings.Split(string(b), "\x00") {
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, " ", 6)
		if len(f) != 6 || (f[0] != "f" && f[0] != "d") || f[5] == "" {
			return nil, errTree
		}
		mode, err1 := strconv.ParseUint(f[1], 10, 32)
		size, err2 := strconv.ParseInt(f[2], 10, 64)
		mtime, err3 := strconv.ParseInt(f[3], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, errTree
		}
		e := tentry{name: f[5], dir: f[0] == "d"}
		if e.dir {
			e.hash, e.count = f[4], int(size)
		} else {
			h := f[4]
			if h == "-" {
				h = ""
			}
			e.f = workspace.File{Size: size, Mode: fs.FileMode(mode), ModTime: mtime, Hash: h}
		}
		out = append(out, e)
	}
	return out, nil
}

func hashTree(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// emptyTree is the tree of a folder with no file.
var emptyTree = hashTree(nil)

// treeCache keeps the trees this process read or made, by hash, up to
// maxCached entries in all; past that it starts again.
var treeCache = struct {
	sync.Mutex
	m map[string][]tentry
	n int
}{m: map[string][]tentry{}}

var maxCached = 1 << 19 // a var for tests

func cached(h string) ([]tentry, bool) {
	treeCache.Lock()
	defer treeCache.Unlock()
	es, ok := treeCache.m[h]
	return es, ok
}

func remember(h string, es []tentry) {
	treeCache.Lock()
	defer treeCache.Unlock()
	if _, ok := treeCache.m[h]; ok {
		return
	}
	if treeCache.n+len(es) > maxCached {
		treeCache.m, treeCache.n = map[string][]tentry{}, 0
	}
	treeCache.m[h] = es
	treeCache.n += len(es) + 1
}

// trees reads and writes one space's trees. A tree made is kept in pending
// until flush writes it, so a checkpoint that records nothing writes none.
type trees struct {
	dir     string
	pending map[string][]byte
}

func (s *Space) trees() *trees {
	return &trees{dir: filepath.Join(s.state(), "trees"), pending: map[string][]byte{}}
}

// get is the tree h. Its entries are shared: never change them. A tree not
// in the cache is found by reading every pack of the space once more.
func (t *trees) get(h string) ([]tentry, error) {
	if h == emptyTree {
		return nil, nil
	}
	if es, ok := cached(h); ok {
		return es, nil
	}
	if b, ok := t.pending[h]; ok {
		es, err := decodeTree(b)
		if err != nil {
			return nil, err
		}
		remember(h, es)
		return es, nil
	}
	es, ok, err := t.readPacks(h)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errTree
	}
	return es, nil
}

// readPacks reads every tree of the space's packs into the cache, and
// answers the tree want, which the cache may not keep when it is full.
func (t *trees) readPacks(want string) ([]tentry, bool, error) {
	entries, err := os.ReadDir(t.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errTree
	}
	var found []tentry
	ok := false
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pack") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(t.dir, e.Name()))
		if err != nil {
			return nil, false, errTree
		}
		err = readPack(b, func(h string, es []tentry) {
			if h == want {
				found, ok = es, true
			}
		})
		if err != nil {
			return nil, false, err
		}
	}
	return found, ok, nil
}

// A pack is its trees one after another, each as
//
//	<sha256> <length>\n<tree>
//
// and each is checked against its hash when read.
func readPack(b []byte, each func(h string, es []tentry)) error {
	for len(b) > 0 {
		nl := bytes.IndexByte(b[:min(len(b), 96)], '\n')
		if nl < 0 {
			return errTree
		}
		h, l, ok := strings.Cut(string(b[:nl]), " ")
		n, err := strconv.Atoi(l)
		if !ok || err != nil || n < 0 || len(b)-nl-1 < n {
			return errTree
		}
		body := b[nl+1 : nl+1+n]
		if hashTree(body) != h {
			return errTree
		}
		es, ok := cached(h)
		if !ok {
			var err error
			if es, err = decodeTree(body); err != nil {
				return err
			}
			remember(h, es)
		}
		each(h, es)
		b = b[nl+1+n:]
	}
	return nil
}

// put makes the tree of es, sorted by name, and answers its hash. was is
// the hash the folder had before: a tree that did not change is not made
// again.
func (t *trees) put(es []tentry, was string) string {
	b := encodeTree(es)
	h := hashTree(b)
	if h != emptyTree && h != was {
		t.pending[h] = b
	}
	return h
}

// flush writes the trees made as one pack.
func (t *trees) flush() error {
	if len(t.pending) == 0 {
		return nil
	}
	if err := os.MkdirAll(t.dir, 0o700); err != nil {
		return err
	}
	hashes := make([]string, 0, len(t.pending))
	size := 0
	for h, b := range t.pending {
		hashes = append(hashes, h)
		size += len(h) + len(b) + 12
	}
	sort.Strings(hashes)
	out := make([]byte, 0, size)
	for _, h := range hashes {
		out = append(out, h...)
		out = append(out, ' ')
		out = strconv.AppendInt(out, int64(len(t.pending[h])), 10)
		out = append(out, '\n')
		out = append(out, t.pending[h]...)
	}
	// Named by its own hash: two flushes of the same trees are one pack.
	if err := writeFileAtomic(filepath.Join(t.dir, hashTree(out)[:32]+".pack"), out); err != nil {
		return err
	}
	for _, h := range hashes {
		if es, err := decodeTree(t.pending[h]); err == nil {
			remember(h, es)
		}
	}
	t.pending = map[string][]byte{}
	return nil
}

// lookup is the file at rel in the tree root.
func (t *trees) lookup(root, rel string) (workspace.File, bool) {
	h := root
	parts := strings.Split(rel, "/")
	for i, name := range parts {
		es, err := t.get(h)
		if err != nil {
			return workspace.File{}, false
		}
		j := sort.Search(len(es), func(k int) bool { return es[k].name >= name })
		if j == len(es) || es[j].name != name {
			return workspace.File{}, false
		}
		e := es[j]
		if i == len(parts)-1 {
			return e.f, !e.dir
		}
		if !e.dir {
			return workspace.File{}, false
		}
		h = e.hash
	}
	return workspace.File{}, false
}

// flatten is every file of the tree root, as a Snapshot.
func (t *trees) flatten(root string) (*workspace.Snapshot, error) {
	snap := workspace.Empty()
	err := t.walk(root, "", func(rel string, f workspace.File) { snap.Set(rel, f) })
	return snap, err
}

// walk calls fn with each file of the tree h, its path under prefix.
func (t *trees) walk(h, prefix string, fn func(rel string, f workspace.File)) error {
	es, err := t.get(h)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.dir {
			if err := t.walk(e.hash, prefix+e.name+"/", fn); err != nil {
				return err
			}
			continue
		}
		fn(prefix+e.name, e.f)
	}
	return nil
}

// fileDiff is one path whose file differs between two trees: by hash or
// mode, or held by one of them only.
type fileDiff struct {
	rel      string
	a, b     workspace.File
	inA, inB bool
}

// diff lists the files that differ between the trees a and b, skipping
// every folder whose tree is the same in both. fix, when not
// nil, gives a file its hash learned since (learned.json) before the two
// sides are compared.
func (t *trees) diff(a, b string, fix func(rel string, f workspace.File) workspace.File) ([]fileDiff, error) {
	var out []fileDiff
	err := t.diffAt(a, b, "", fix, &out)
	return out, err
}

func (t *trees) diffAt(a, b, prefix string, fix func(string, workspace.File) workspace.File, out *[]fileDiff) error {
	if a == b {
		return nil
	}
	ea, err := t.get(a)
	if err != nil {
		return err
	}
	eb, err := t.get(b)
	if err != nil {
		return err
	}
	if fix == nil {
		fix = func(_ string, f workspace.File) workspace.File { return f }
	}
	// one side of a path: a file, or every file of a folder.
	side := func(e tentry, inA bool) error {
		add := func(rel string, f workspace.File) {
			d := fileDiff{rel: rel}
			if inA {
				d.a, d.inA = fix(rel, f), true
			} else {
				d.b, d.inB = fix(rel, f), true
			}
			*out = append(*out, d)
		}
		if !e.dir {
			add(prefix+e.name, e.f)
			return nil
		}
		return t.walk(e.hash, prefix+e.name+"/", add)
	}
	i, j := 0, 0
	for i < len(ea) || j < len(eb) {
		switch {
		case j == len(eb) || (i < len(ea) && ea[i].name < eb[j].name):
			if err := side(ea[i], true); err != nil {
				return err
			}
			i++
		case i == len(ea) || eb[j].name < ea[i].name:
			if err := side(eb[j], false); err != nil {
				return err
			}
			j++
		default:
			x, y := ea[i], eb[j]
			i, j = i+1, j+1
			switch {
			case x.dir && y.dir:
				if err := t.diffAt(x.hash, y.hash, prefix+x.name+"/", fix, out); err != nil {
					return err
				}
			case !x.dir && !y.dir:
				rel := prefix + x.name
				fa, fb := fix(rel, x.f), fix(rel, y.f)
				if fa.Hash != fb.Hash || fa.Mode != fb.Mode {
					*out = append(*out, fileDiff{rel: rel, a: fa, b: fb, inA: true, inB: true})
				}
			default:
				// A file where a folder was, or the reverse: the file on one
				// side, every file of the folder on the other.
				if err := side(x, true); err != nil {
					return err
				}
				if err := side(y, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// editor changes a tree by paths, loading only the folders on them; finish
// makes the new trees, from the deepest up.
type editor struct {
	t    *trees
	root *tnode
}

// tnode is a folder being edited: loaded from its tree, or new.
type tnode struct {
	hash    string
	count   int
	entries map[string]tentry // nil until loaded
	kids    map[string]*tnode // folders under it being edited
}

func (t *trees) edit(root string, count int) *editor {
	return &editor{t: t, root: &tnode{hash: root, count: count}}
}

func (e *editor) load(n *tnode) error {
	if n.entries != nil {
		return nil
	}
	es, err := e.t.get(n.hash)
	if err != nil {
		return err
	}
	n.entries, n.kids = make(map[string]tentry, len(es)), map[string]*tnode{}
	for _, x := range es {
		n.entries[x.name] = x
	}
	return nil
}

// folder is the folder at the path parts, made (over a file there) when
// make is true; nil when it is not there and make is false.
func (e *editor) folder(parts []string, make bool) (*tnode, error) {
	n := e.root
	for _, name := range parts {
		if err := e.load(n); err != nil {
			return nil, err
		}
		if k, ok := n.kids[name]; ok {
			n = k
			continue
		}
		x, ok := n.entries[name]
		switch {
		case ok && x.dir:
			k := &tnode{hash: x.hash, count: x.count}
			n.kids[name] = k
			n = k
		case !make:
			return nil, nil
		default:
			delete(n.entries, name)
			k := &tnode{hash: emptyTree}
			n.kids[name] = k
			n = k
		}
	}
	return n, e.load(n)
}

func split(rel string) ([]string, string) {
	parts := strings.Split(rel, "/")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// set puts the file f at rel, over whatever is there.
func (e *editor) set(rel string, f workspace.File) error {
	dir, name := split(rel)
	n, err := e.folder(dir, true)
	if err != nil {
		return err
	}
	delete(n.kids, name)
	n.entries[name] = tentry{name: name, f: f}
	return nil
}

// remove takes out what is at rel: a file, or a folder with all under it.
func (e *editor) remove(rel string) error {
	dir, name := split(rel)
	n, err := e.folder(dir, false)
	if n == nil || err != nil {
		return err
	}
	delete(n.kids, name)
	delete(n.entries, name)
	return nil
}

// removeFile takes out a file at rel, leaving a folder there as it is.
func (e *editor) removeFile(rel string) error {
	dir, name := split(rel)
	n, err := e.folder(dir, false)
	if n == nil || err != nil {
		return err
	}
	if x, ok := n.entries[name]; ok && !x.dir {
		delete(n.entries, name)
	}
	return nil
}

// finish makes the trees of every folder edited, and answers the new top
// tree and the count of its files.
func (e *editor) finish() (string, int) { return e.finishAt(e.root) }

func (e *editor) finishAt(n *tnode) (string, int) {
	if n.entries == nil {
		return n.hash, n.count
	}
	for name, k := range n.kids {
		h, c := e.finishAt(k)
		if c == 0 {
			delete(n.entries, name) // a folder left with no file
			continue
		}
		n.entries[name] = tentry{name: name, dir: true, hash: h, count: c}
	}
	es := make([]tentry, 0, len(n.entries))
	count := 0
	for _, x := range n.entries {
		es = append(es, x)
		if x.dir {
			count += x.count
		} else {
			count++
		}
	}
	if count == 0 {
		return emptyTree, 0
	}
	sort.Slice(es, func(i, j int) bool { return es[i].name < es[j].name })
	return e.t.put(es, n.hash), count
}
