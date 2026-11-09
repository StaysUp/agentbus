// Package workspace is the format of a folder's files and of what changed
// in it: a Snapshot of every file by size, modification time, mode and hash,
// and the Changes between two of them that a job's result carries. Package
// isolation keeps a session's checkpoints in this format and computes every
// job's changes from them; package queue and the API read the Changes back.
// Nothing here runs git or any other program.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits of one Snapshot and of the changes kept in a result.
const (
	MaxSnapshotFiles = 200000
	MaxChanges       = 1000
)

// entry is one file of a Snapshot.
type entry struct {
	size    int64
	mode    fs.FileMode
	modTime int64
	hash    string
}

// Racy: a file whose modification time is this close to when a Snapshot
// was taken may change again without its time changing, since a file clock
// ticks coarsely (every few milliseconds on Linux). The next Walk hashes it
// again rather than trust its size and time.
const Racy = 2 * time.Second

// Snapshot is a workspace's files, by slash-separated path from its root.
type Snapshot struct {
	files     map[string]entry
	taken     int64 // when the walk began, in Unix nanoseconds; 0 if not known
	truncated bool  // MaxSnapshotFiles was reached
}

// Hasher answers the hash of a regular file or a symbolic link that Walk
// found, at path, rel from the root. An empty hash records the file as not
// hashed. An error leaves the file out, as an unreadable file is left out.
type Hasher func(rel, path string, info fs.FileInfo) (string, error)

// Hash is the Hasher Walk uses by default: a file's content, a link's target.
func Hash(_, path string, info fs.FileInfo) (string, error) {
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return hashBytes([]byte("link:" + target)), nil
	}
	return hashFile(path)
}

// Walk records every file under root. A file whose size, modification time
// and mode match prev's keeps prev's hash, even an empty one, unless its time
// was within Racy of when prev was taken; every other file is passed to hash.
// A nil hash is Hash. The root's .git, a folder in a repo or a file in a
// worktree, is skipped. A symbolic link is recorded by its target, never
// followed.
func Walk(root string, prev *Snapshot, hash Hasher) (*Snapshot, error) {
	if hash == nil {
		hash = Hash
	}
	s := &Snapshot{files: map[string]entry{}, taken: time.Now().UnixNano()}
	list, truncated, err := List(root, true, MaxSnapshotFiles)
	if err != nil {
		return nil, err
	}
	// The files past the limit are a matter of which folder was read first:
	// a truncated snapshot is never applied.
	s.truncated = truncated
	for _, it := range list {
		info := it.Info
		if info.IsDir() || (info.Mode()&fs.ModeSymlink == 0 && !info.Mode().IsRegular()) {
			continue // a folder; a socket, a device: not a file of the work
		}
		if len(s.files) >= MaxSnapshotFiles {
			s.truncated = true
			break
		}
		e := entry{size: info.Size(), mode: info.Mode(), modTime: info.ModTime().UnixNano()}
		if old, ok := prevEntry(prev, it.Rel); ok && old.size == e.size && old.modTime == e.modTime && old.mode == e.mode && !prev.racy(old.modTime) {
			e.hash = old.hash
		} else {
			h, err := hash(it.Rel, it.Path, info)
			if err != nil {
				continue // unreadable: left out
			}
			e.hash = h
		}
		s.files[it.Rel] = e
	}
	return s, nil
}

// File is one file of a Snapshot. Hash is the SHA-256 of its content, or of
// "link:" and its target for a symbolic link. An empty Hash is a file not
// hashed: package isolation records a copy's first state by size,
// modification time and mode only.
type File struct {
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	ModTime int64       `json:"mtime"`
	Hash    string      `json:"sha256"`
}

// File is the file at rel, a slash-separated path from the root.
func (s *Snapshot) File(rel string) (File, bool) {
	e, ok := s.files[rel]
	return File{Size: e.size, Mode: e.mode, ModTime: e.modTime, Hash: e.hash}, ok
}

// Paths are the snapshot's files, sorted.
func (s *Snapshot) Paths() []string {
	out := make([]string, 0, len(s.files))
	for p := range s.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Set records f at rel, such as the hash of a copy taken after the walk.
func (s *Snapshot) Set(rel string, f File) {
	s.files[rel] = entry{size: f.Size, mode: f.Mode, modTime: f.ModTime, hash: f.Hash}
}

// Delete leaves rel out of the snapshot.
func (s *Snapshot) Delete(rel string) { delete(s.files, rel) }

// DeleteUnder leaves out every file inside the folder dir.
func (s *Snapshot) DeleteUnder(dir string) {
	prefix := dir + "/"
	for p := range s.files {
		if strings.HasPrefix(p, prefix) {
			delete(s.files, p)
		}
	}
}

// Empty is a snapshot with no files, to fill with Set, taken now.
func Empty() *Snapshot { return &Snapshot{files: map[string]entry{}, taken: time.Now().UnixNano()} }

// Racy says whether a file of s modified at mtime (Unix nanoseconds) may have
// changed since without its time changing: its time is within Racy of when
// s was taken, or after. A snapshot whose time is not known trusts every
// time, as one written before it was recorded.
func (s *Snapshot) Racy(mtime int64) bool { return s.racy(mtime) }

func (s *Snapshot) racy(mtime int64) bool {
	return s.taken != 0 && mtime >= s.taken-int64(Racy)
}

// SetTaken sets when s was taken, as Walk does when it begins.
func (s *Snapshot) SetTaken(t time.Time) { s.taken = t.UnixNano() }

// Taken is when s was taken, in Unix nanoseconds; 0 if not known.
func (s *Snapshot) Taken() int64 { return s.taken }

// SetTruncated marks s as missing files past MaxSnapshotFiles.
func (s *Snapshot) SetTruncated() { s.truncated = true }

// Len is the number of files.
func (s *Snapshot) Len() int { return len(s.files) }

// Truncated: the walk stopped at MaxSnapshotFiles, so files are missing.
func (s *Snapshot) Truncated() bool { return s.truncated }

// Changed are the paths whose file differs between s and o, by hash or
// mode, or that only one of them has, sorted.
func (s *Snapshot) Changed(o *Snapshot) []string {
	var out []string
	for p, e := range s.files {
		if f, ok := o.files[p]; !ok || f.hash != e.hash || f.mode != e.mode {
			out = append(out, p)
		}
	}
	for p := range o.files {
		if _, ok := s.files[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// snapshotJSON is a Snapshot as it is kept on disk.
type snapshotJSON struct {
	Files     map[string]File `json:"files"`
	Taken     int64           `json:"taken,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

func (s *Snapshot) MarshalJSON() ([]byte, error) {
	j := snapshotJSON{Files: make(map[string]File, len(s.files)), Taken: s.taken, Truncated: s.truncated}
	for p := range s.files {
		j.Files[p], _ = s.File(p)
	}
	return json.Marshal(j)
}

func (s *Snapshot) UnmarshalJSON(b []byte) error {
	var j snapshotJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*s = Snapshot{files: make(map[string]entry, len(j.Files)), taken: j.Taken, truncated: j.Truncated}
	for p, f := range j.Files {
		s.Set(p, f)
	}
	return nil
}

// HashOf hashes the file at p as Walk does, without following a symbolic
// link. A missing file is fs.ErrNotExist; a folder or a device is an error.
func HashOf(p string) (string, fs.FileMode, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return "", 0, err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return "", 0, err
		}
		return hashBytes([]byte("link:" + target)), info.Mode(), nil
	case info.Mode().IsRegular():
		h, err := hashFile(p)
		return h, info.Mode(), err
	}
	return "", info.Mode(), errors.New("not a file")
}

func prevEntry(prev *Snapshot, rel string) (entry, bool) {
	if prev == nil {
		return entry{}, false
	}
	e, ok := prev.files[rel]
	return e, ok
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Change is one file that differs between the start and the end of a job.
type Change struct {
	Path   string `json:"path"`
	Change string `json:"change"` // added, modified or deleted
	// Size and SHA256 are the file at the end; for a deleted file, at the
	// start.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Content: the whole file at the end, when it is small UTF-8 text.
	Content *string `json:"content,omitempty"`
	// Diff: this file's unified diff from the turn's start to its end, when
	// both sides are small text and its earlier content is kept.
	// DiffLeftOut: the content changed and no diff is here (binary, a link,
	// too big, past the budget, or not kept).
	Diff        *string `json:"diff,omitempty"`
	DiffLeftOut bool    `json:"diff_left_out,omitempty"`
}

// Changes is what a job changed in its workspace: the difference between
// two checkpoints of it (package isolation).
type Changes struct {
	// Kind is always snapshot: the files were compared by their hashes.
	Kind  string   `json:"kind"`
	Files []Change `json:"files"`
	// Count is every change; Files holds at most MaxChanges of them.
	Count     int  `json:"count"`
	Truncated bool `json:"truncated,omitempty"`
	// Partial: taken while the turn may still run, so it may miss changes
	// made after it.
	Partial bool `json:"partial,omitempty"`
	// Error: the end of the job's workspace could not be read.
	Error string `json:"error,omitempty"`
	// ContentLeftOut: some small text files have no content here, to keep
	// the result small.
	ContentLeftOut bool `json:"content_left_out,omitempty"`
	// From and To, in an isolated session: the checkpoints the changes are
	// between, which an apply names. The same name twice: the turn changed
	// nothing. Empty for a folder used as it is, which cannot be applied.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// KindSnapshot is every Changes' Kind.
const KindSnapshot = "snapshot"

// Limits of the contents a result carries.
const (
	MaxContent = 8 << 10 // a small text file's content
	// MaxContents bounds the content of all files together, so a job that
	// writes many small files still has a result that can be sent.
	MaxContents = 256 << 10
)

// Diff lists the changes from before to after, sorted by path, with each
// small text file's content at the end read by read, such as from a
// checkpoint's own copy of it. read answers at most max bytes, or false; a
// nil read gives no contents.
func Diff(before, after *Snapshot, read func(rel string, max int) ([]byte, bool)) Changes {
	c := Changes{Kind: KindSnapshot, Files: []Change{}, Truncated: before.truncated || after.truncated}
	for p, e := range after.files {
		old, ok := before.files[p]
		switch {
		case !ok:
			c.Files = append(c.Files, Change{Path: p, Change: "added", Size: e.size, SHA256: e.hash})
		case old.hash != e.hash || old.mode != e.mode:
			c.Files = append(c.Files, Change{Path: p, Change: "modified", Size: e.size, SHA256: e.hash})
		}
	}
	for p, e := range before.files {
		if _, ok := after.files[p]; !ok {
			c.Files = append(c.Files, Change{Path: p, Change: "deleted", Size: e.size, SHA256: e.hash})
		}
	}
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
	c.Count = len(c.Files)
	if len(c.Files) > MaxChanges {
		c.Files, c.Truncated = c.Files[:MaxChanges], true
	}
	budget := MaxContents
	for i := range c.Files {
		f := &c.Files[i]
		if f.Change == "deleted" || f.Size > MaxContent {
			continue
		}
		if f.Size > int64(budget) || read == nil {
			c.ContentLeftOut = true
			continue
		}
		if b, ok := read(f.Path, budget); ok && isText(b) {
			t := string(b)
			f.Content = &t
			budget -= len(b)
		}
	}
	return c
}

// Fit shrinks c until its JSON is at most max bytes: first the files'
// content goes, then each file's own diff, then files from the end of the
// list. What was left out is marked: ContentLeftOut, DiffLeftOut, Truncated.
// Count stays the whole count.
func (c *Changes) Fit(max int) {
	size := func() int { b, _ := json.Marshal(c); return len(b) }
	if size() <= max {
		return
	}
	for i := range c.Files {
		if c.Files[i].Content != nil {
			c.Files[i].Content, c.ContentLeftOut = nil, true
		}
	}
	if size() <= max {
		return
	}
	for i := range c.Files {
		if c.Files[i].Diff != nil {
			c.Files[i].Diff, c.Files[i].DiffLeftOut = nil, true
		}
	}
	for len(c.Files) > 0 && size() > max {
		c.Files, c.Truncated = c.Files[:len(c.Files)/2], true
	}
}

// isText: valid UTF-8 with no NUL byte.
func isText(b []byte) bool {
	return bytes.IndexByte(b, 0) < 0 && utf8.Valid(b)
}
