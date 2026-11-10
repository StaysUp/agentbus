package workspace

// Content trees: a folder's files as a Merkle tree of their contents only,
// so equal files give an equal root on any machine, whatever their times.
// A tree is one folder, its entries sorted by name, each
//
//	f <mode> <size> <sha256> <name>\x00   a file: its content's hash
//	d <count> <tree> <name>\x00           a folder: its tree's hash, and the files under it
//
// and a tree is named by the hex SHA-256 of those bytes. A snapshot id is
// the name of the top folder's tree. A folder with no file under it is not
// in a tree. A symbolic link is a file whose content is "link:" and its
// target (Hash).
//
// The connector makes these trees from a folder; the runner makes the same
// trees from its checkpoints (package isolation), and checks that a copy it
// built from a snapshot has the snapshot's id. Both send them through the
// control plane's blob store, so a tree, like a file, is sent once.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// TreeEntry is one entry of a content tree.
type TreeEntry struct {
	Name string
	Dir  bool
	// Hash: a file's content hash, or a folder's tree.
	Hash string
	// Count: for a folder, the files under it.
	Count int
	// Mode and Size: a file's.
	Mode fs.FileMode
	Size int64
}

// EncodeTree is the content tree of es, which must be sorted by name.
func EncodeTree(es []TreeEntry) []byte {
	var b strings.Builder
	for _, e := range es {
		if e.Dir {
			fmt.Fprintf(&b, "d %d %s %s\x00", e.Count, e.Hash, e.Name)
			continue
		}
		fmt.Fprintf(&b, "f %d %d %s %s\x00", uint32(treeMode(e.Mode)), e.Size, e.Hash, e.Name)
	}
	return []byte(b.String())
}

// treeMode is the mode a tree records: a symbolic link's permissions are
// whatever its machine gives links (0777 on Linux, 0755 on macOS), so a
// link is always recorded as 0777.
func treeMode(m fs.FileMode) fs.FileMode {
	if m&fs.ModeSymlink != 0 {
		return fs.ModeSymlink | 0o777
	}
	return m
}

// ErrTree: bytes that are not a content tree.
var ErrTree = errors.New("workspace: not a content tree")

// DecodeTree reads a content tree.
func DecodeTree(b []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	prev := ""
	for _, rec := range strings.Split(string(b), "\x00") {
		if rec == "" {
			continue
		}
		var e TreeEntry
		switch {
		case strings.HasPrefix(rec, "f "):
			f := strings.SplitN(rec, " ", 5)
			if len(f) != 5 {
				return nil, ErrTree
			}
			mode, err1 := strconv.ParseUint(f[1], 10, 32)
			size, err2 := strconv.ParseInt(f[2], 10, 64)
			if err1 != nil || err2 != nil || size < 0 || !hexHash(f[3]) {
				return nil, ErrTree
			}
			e = TreeEntry{Name: f[4], Hash: f[3], Mode: fs.FileMode(mode), Size: size}
		case strings.HasPrefix(rec, "d "):
			f := strings.SplitN(rec, " ", 4)
			if len(f) != 4 {
				return nil, ErrTree
			}
			n, err := strconv.Atoi(f[1])
			if err != nil || n < 1 || !hexHash(f[2]) {
				return nil, ErrTree
			}
			e = TreeEntry{Name: f[3], Dir: true, Hash: f[2], Count: n}
		default:
			return nil, ErrTree
		}
		if !NameOK(e.Name) || e.Name <= prev {
			return nil, ErrTree
		}
		prev = e.Name
		out = append(out, e)
	}
	return out, nil
}

// NameOK says name can be one entry of a tree: not empty, not . or .., no
// slash and no NUL.
func NameOK(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func hexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// TreeHash names a tree: the hex SHA-256 of its bytes.
func TreeHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// EmptyTree is the tree of a folder with no file.
var EmptyTree = TreeHash(nil)

// ContentTrees makes the content trees of snap, whose files must all be
// hashed: the root's name and every tree by its name. ok is false when a
// file has no hash, or a path cannot be in a tree.
func ContentTrees(snap *Snapshot) (root string, trees map[string][]byte, ok bool) {
	type node struct {
		files []TreeEntry
		dirs  map[string]*node
	}
	top := &node{dirs: map[string]*node{}}
	for rel, e := range snap.files {
		if e.hash == "" {
			return "", nil, false
		}
		parts := strings.Split(rel, "/")
		n := top
		for _, p := range parts[:len(parts)-1] {
			if !NameOK(p) {
				return "", nil, false
			}
			k, ok := n.dirs[p]
			if !ok {
				k = &node{dirs: map[string]*node{}}
				n.dirs[p] = k
			}
			n = k
		}
		name := parts[len(parts)-1]
		if !NameOK(name) {
			return "", nil, false
		}
		n.files = append(n.files, TreeEntry{Name: name, Hash: e.hash, Mode: e.mode, Size: e.size})
	}
	trees = map[string][]byte{}
	var make func(n *node) (string, int, bool)
	make = func(n *node) (string, int, bool) {
		es := append([]TreeEntry(nil), n.files...)
		count := len(n.files)
		for name, k := range n.dirs {
			h, c, ok := make(k)
			if !ok {
				return "", 0, false
			}
			if c == 0 {
				continue
			}
			es = append(es, TreeEntry{Name: name, Dir: true, Hash: h, Count: c})
			count += c
		}
		sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
		for i := 1; i < len(es); i++ {
			if es[i].Name == es[i-1].Name {
				return "", 0, false // a file and a folder of one name
			}
		}
		if count == 0 {
			return EmptyTree, 0, true
		}
		b := EncodeTree(es)
		h := TreeHash(b)
		trees[h] = b
		return h, count, true
	}
	root, _, ok = make(top)
	return root, trees, ok
}

// LinkContent is what a symbolic link's Hash is the hash of: "link:" and
// its target. A blob of a link holds these bytes.
func LinkContent(target string) []byte { return []byte("link:" + target) }

// LinkTarget is the target a link's blob names, and false when b is not
// one.
func LinkTarget(b []byte) (string, bool) {
	s := string(b)
	if !strings.HasPrefix(s, "link:") || len(s) == len("link:") {
		return "", false
	}
	return s[len("link:"):], true
}
