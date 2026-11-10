package isolation

// A copy built from a snapshot, and what goes out of a space: its
// changesets and the saved files they name. Nothing here reads the
// person's folder: the snapshot's files come from a blob store, and the
// local fast path's base content comes through a Lender, which is the
// folder side's.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// BlobSource reads blobs by their hash (package blobs).
type BlobSource interface {
	Open(ctx context.Context, h string) (io.ReadCloser, error)
}

// blobPather is a BlobSource that keeps each blob as a file of its own: a
// copy built from it clones that file instead of reading it.
type blobPather interface {
	Has(h string) bool
	Path(h string) string
}

// Snapshot is a folder as its connector recorded it, to build a copy from.
type Snapshot struct {
	// ID is the snapshot id: the name of its top content tree.
	ID string
	// Blobs holds its trees and its files.
	Blobs BlobSource
	// Times, when not empty, names a blob of each file's modification time,
	// one "<unix nanoseconds> <path>" a line, which the copy takes; any
	// other file gets the time the copy was built at, less a little more
	// than workspace.Racy, so a checkpoint trusts it.
	Times string
	// Git, when not empty, is the content tree of the folder's .git: the
	// copy gets it as its .git, so git works there, and no checkpoint
	// holds it.
	Git string
}

// Lender reads the person's folder a copy was made from, for the base
// content of a file a turn changed: the local fast path only, where the
// folder side (package connector) runs in this process. The base of a copy
// made by clone records most files by size, time and mode only, and keeps
// none of their content.
type Lender interface {
	// Lend opens the file rel of the folder ref while it is a regular file
	// with f's size, modification time and mode. same says, once it was
	// read, whether it still has them; the bytes read count only then.
	Lend(ref, rel string, f workspace.File) (r io.ReadCloser, same func() bool, ok bool)
}

// ErrSnapshot: the snapshot could not be read, or the copy built from it
// is not it.
var ErrSnapshot = errors.New("isolation: the copy could not be built from the snapshot")

// MaxTimes bounds a snapshot's times blob.
const MaxTimes = 64 << 20

// materialise writes the files of snap into dir, which must not exist,
// and answers them as a Snapshot: each with its hash from the snapshot and
// its size, time and mode as written. A file is a clone of its blob where
// the blob is a file of its own and the disk clones.
func materialise(ctx context.Context, snap *Snapshot, dir string, mode fs.FileMode) (*workspace.Snapshot, error) {
	if snap == nil || snap.Blobs == nil || !hexHash(snap.ID) {
		return nil, ErrSnapshot
	}
	times := map[string]int64{}
	if snap.Times != "" {
		b, err := readBlob(ctx, snap.Blobs, snap.Times, MaxTimes)
		if err != nil {
			return nil, ErrSnapshot
		}
		times = workspace.ParseTimes(b)
	}
	if err := os.Mkdir(dir, mode.Perm()|0o700); err != nil {
		return nil, err
	}
	out := workspace.Empty()
	fallback := time.Now().Add(-workspace.Racy - time.Second)
	pather, _ := snap.Blobs.(blobPather)
	// A store that fetches from another is asked for every file at once,
	// so only the ones it lacks are sent, and each is then cloned.
	if f, ok := snap.Blobs.(blobFetcher); ok {
		var hs []string
		for _, root := range []string{snap.ID, snap.Git} {
			if root == "" {
				continue
			}
			if err := eachFile(ctx, snap.Blobs, root, func(e workspace.TreeEntry) { hs = append(hs, e.Hash) }); err != nil {
				removeAll(dir)
				return nil, ErrSnapshot
			}
		}
		if err := f.Fetch(ctx, hs); err != nil {
			removeAll(dir)
			return nil, ErrSnapshot
		}
	}
	var write func(tree, rel string) error
	write = func(tree, rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tree == workspace.EmptyTree {
			return nil
		}
		b, err := readBlob(ctx, snap.Blobs, tree, 64<<20)
		if err != nil {
			return ErrSnapshot
		}
		es, err := workspace.DecodeTree(b)
		if err != nil {
			return ErrSnapshot
		}
		for _, e := range es {
			if err := ctx.Err(); err != nil {
				return err
			}
			r := e.Name
			if rel != "" {
				r = rel + "/" + e.Name
			}
			p := filepath.Join(dir, filepath.FromSlash(r))
			if e.Dir {
				if err := os.Mkdir(p, 0o755); err != nil {
					return err
				}
				if err := write(e.Hash, r); err != nil {
					return err
				}
				continue
			}
			if err := writeBlobFile(ctx, snap.Blobs, pather, e, p); err != nil {
				return err
			}
			mt := fallback
			if t, ok := times[r]; ok {
				mt = time.Unix(0, t)
			}
			if e.Mode&fs.ModeSymlink == 0 {
				_ = os.Chtimes(p, mt, mt)
			}
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			out.Set(r, workspace.File{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime().UnixNano(), Hash: e.Hash})
		}
		return nil
	}
	if err := write(snap.ID, ""); err != nil {
		removeAll(dir)
		return nil, err
	}
	if snap.Git != "" {
		if !hexHash(snap.Git) {
			removeAll(dir)
			return nil, ErrSnapshot
		}
		git := workspace.Empty()
		files := out
		out = git // what write records under .git is not the copy's
		err := os.Mkdir(filepath.Join(dir, ".git"), 0o755)
		if err == nil {
			err = write(snap.Git, ".git")
		}
		out = files
		// A tree holds no empty folder, and git needs some that are often
		// empty (a new repo's objects, refs once packed).
		for _, d := range []string{"objects/info", "objects/pack", "refs/heads", "refs/tags"} {
			if err == nil {
				err = os.MkdirAll(filepath.Join(dir, ".git", filepath.FromSlash(d)), 0o755)
			}
		}
		if err != nil {
			removeAll(dir)
			return nil, err
		}
	}
	return out, nil
}

// blobFetcher is a store that fetches the blobs it lacks from another, all
// at once (blobs.Local).
type blobFetcher interface {
	Fetch(ctx context.Context, hs []string) error
}

// eachFile calls fn with every file of the content tree root, reading its
// trees from src.
func eachFile(ctx context.Context, src BlobSource, root string, fn func(workspace.TreeEntry)) error {
	if root == workspace.EmptyTree {
		return nil
	}
	b, err := readBlob(ctx, src, root, 64<<20)
	if err != nil {
		return err
	}
	es, err := workspace.DecodeTree(b)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Dir {
			if err := eachFile(ctx, src, e.Hash, fn); err != nil {
				return err
			}
			continue
		}
		fn(e)
	}
	return nil
}

// writeBlobFile writes the file e of a tree at p from its blob.
func writeBlobFile(ctx context.Context, src BlobSource, pather blobPather, e workspace.TreeEntry, p string) error {
	if e.Mode&fs.ModeSymlink != 0 {
		b, err := readBlob(ctx, src, e.Hash, 64<<10)
		if err != nil {
			return ErrSnapshot
		}
		target, ok := workspace.LinkTarget(b)
		if !ok {
			return ErrSnapshot
		}
		return os.Symlink(target, p)
	}
	if !e.Mode.IsRegular() {
		return ErrSnapshot
	}
	if pather != nil && pather.Has(e.Hash) {
		if err := cloneFile(pather.Path(e.Hash), p, e.Mode.Perm()); err == nil {
			if err := os.Chmod(p, e.Mode.Perm()); err != nil {
				return err
			}
			return checkSize(p, e)
		}
		os.Remove(p)
	}
	rc, err := src.Open(ctx, e.Hash)
	if err != nil {
		return ErrSnapshot
	}
	defer rc.Close()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, e.Mode.Perm())
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != e.Hash {
		return ErrSnapshot
	}
	if err := os.Chmod(p, e.Mode.Perm()); err != nil {
		return err
	}
	return checkSize(p, e)
}

func checkSize(p string, e workspace.TreeEntry) error {
	fi, err := os.Lstat(p)
	if err != nil || fi.Size() != e.Size {
		return ErrSnapshot
	}
	return nil
}

// readBlob reads the blob h, at most max bytes, checked against h.
func readBlob(ctx context.Context, src BlobSource, h string, max int64) ([]byte, error) {
	rc, err := src.Open(ctx, h)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, ErrSnapshot
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != h {
		return nil, ErrSnapshot
	}
	return b, nil
}

func hexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ChangesetOf is what the checkpoint name of s holds that differs from its
// Base: every file a later checkpoint changed, as the Base held it and as
// name holds it.
func ChangesetOf(s *Space, name string) (workspace.Changeset, error) {
	unlock := lock(s)
	defer unlock()
	list, err := headers(s)
	if err != nil {
		return workspace.Changeset{}, err
	}
	if len(list) == 0 || list[0].Checkpoint.Name != Base {
		return workspace.Changeset{}, ErrNoCheckpoint
	}
	a, b, ha, hb, err := pair(s, list, Base, name)
	if err != nil {
		return workspace.Changeset{}, err
	}
	cs := workspace.Changeset{Space: s.ID, Checkpoint: hb.Checkpoint.Name, Snapshot: ha.Checkpoint.Root, Files: []workspace.FileChange{},
		Truncated: a.Truncated() || b.Truncated()}
	for _, rel := range b.Changed(a) {
		fc := workspace.FileChange{Path: rel}
		if f, ok := a.File(rel); ok {
			fc.Base = &f
		}
		if f, ok := b.File(rel); ok {
			fc.Now = &f
		}
		cs.Files = append(cs.Files, fc)
	}
	return cs, nil
}

// Blob reads the file a checkpoint of s saved with hash h: its saved copy,
// or for a link the bytes its hash is of. ErrNoCheckpoint when s keeps no
// such file.
func Blob(s *Space, h string) (io.ReadCloser, error) {
	if !hexHash(h) {
		return nil, ErrNoCheckpoint
	}
	p := filepath.Join(s.objectDir(), h)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, ErrNoCheckpoint
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return nil, ErrNoCheckpoint
		}
		return io.NopCloser(bytes.NewReader(workspace.LinkContent(target))), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, ErrNoCheckpoint
	}
	return f, nil
}

// BlobPath is where s keeps the saved regular file h, for a clone of it,
// and false when it keeps none.
func BlobPath(s *Space, h string) (string, bool) {
	if !hexHash(h) {
		return "", false
	}
	p := filepath.Join(s.objectDir(), h)
	fi, err := os.Lstat(p)
	return p, err == nil && fi.Mode().IsRegular()
}

// lend reads the base content of rel, f, from the person's folder through
// s's Lender: hashed, and saved when keep; ok false when the folder no
// longer has it as f or there is no Lender.
func lend(s *Space, rel string, f workspace.File, keep bool) (hash string, saved, ok bool) {
	if s.Lender == nil || s.Ref == "" || !f.Mode.IsRegular() {
		return "", false, false
	}
	r, same, ok := s.Lender.Lend(s.Ref, rel, f)
	if !ok {
		return "", false, false
	}
	defer r.Close()
	if !keep {
		h := sha256.New()
		if _, err := io.Copy(h, r); err != nil || !same() {
			return "", false, false
		}
		return hex.EncodeToString(h.Sum(nil)), false, true
	}
	if err := os.MkdirAll(s.objectDir(), 0o700); err != nil {
		return "", false, false
	}
	h, err := saveReader(r, s.objectDir(), f.Mode.Perm())
	if err != nil || !same() {
		return "", false, false
	}
	return h, true, true
}

// fromBlobs reads the base content of a file with hash h from s's Blobs,
// saved as a saved file of s so a later read needs no store.
func fromBlobs(s *Space, h string) bool {
	if s.Blobs == nil {
		return false
	}
	if _, err := os.Lstat(filepath.Join(s.objectDir(), h)); err == nil {
		return true
	}
	rc, err := s.Blobs.Open(context.Background(), h)
	if err != nil {
		return false
	}
	defer rc.Close()
	if err := os.MkdirAll(s.objectDir(), 0o700); err != nil {
		return false
	}
	got, err := saveReader(rc, s.objectDir(), 0o600)
	return err == nil && got == h
}

// saveReader saves what r reads under objects by its hash, and answers it.
func saveReader(r io.Reader, objects string, perm fs.FileMode) (string, error) {
	tmp, err := os.CreateTemp(objects, ".tmp-")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), perm)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	dst := filepath.Join(objects, sum)
	if _, err := os.Lstat(dst); err == nil {
		os.Remove(tmp.Name())
		return sum, nil
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return sum, nil
}

// BaseBlob reads the content a file had in s's Base, f, as rel: its saved
// copy, the snapshot's blob, or on the local fast path the person's folder
// through the Lender, kept as a saved file once read. ErrNoCheckpoint when
// none still has it. An apply that puts a file back as the Base had it
// needs that content in the store.
func BaseBlob(s *Space, rel string, f workspace.File) (io.ReadCloser, error) {
	if f.Hash == "" {
		return nil, ErrNoCheckpoint
	}
	if rc, err := Blob(s, f.Hash); err == nil {
		return rc, nil
	}
	if fromBlobs(s, f.Hash) {
		return Blob(s, f.Hash)
	}
	if h, saved, ok := lend(s, rel, f, true); ok && saved && h == f.Hash {
		return Blob(s, f.Hash)
	}
	return nil, ErrNoCheckpoint
}
