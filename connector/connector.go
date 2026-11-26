// Package connector is the folder side: the one part of AgentBus that reads
// and writes the person's folder. It holds the folders of one machine
// (package folders), checks and registers a folder a person typed, puts a
// session's changes into one when the person applies them, and lends the
// runner the base content of a file its copy no longer has.
//
// Since 0.5 it is no program of its own: the runner of the machine does its
// jobs (package runner, Config.FolderSide), so one program runs on each
// machine. The package keeps its name. An apply reads the files a turn
// wrote from the control plane's blob store, where the runner put them; in
// agentbus up the store is the control plane's own on the same disk, and
// nothing is sent over HTTP.
//
// Control packages never import it (cmd/agentbus/layers_test.go).
package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// Connector is one machine's folder side.
type Connector struct {
	// Folders are the folders it owns.
	Folders *folders.Registry
	// Store is where it keeps blobs: the control plane's own store in
	// agentbus up, its own otherwise.
	Store *blobs.Store
	// Remote is the control plane's store when it is not Store: blobs
	// go there after Store, and are read from there into Store.
	Remote interface {
		blobs.Source
		blobs.Sink
	}
	// State is the connector's own folder: what each folder's last
	// snapshot found, so the next hashes only what changed.
	State string
	// Link clones a file to a new path where the disk can, for a blob kept
	// in Store: no bytes copied. Nil copies.
	Link func(src, dst string) error
	// Prepare, when not nil, readies a folder before it is read or
	// written: a folder inside a runner's overlay copy on this machine is
	// mounted again there (agentbus up wires it).
	Prepare func(path string) error
}

// Taken is a snapshot the connector took.
type Taken struct {
	ID    string `json:"id"`              // the snapshot id: its top content tree
	Times string `json:"times,omitempty"` // the blob of its files' times (workspace.FormatTimes)
	Files int    `json:"files"`
	// Git is the content tree of the folder's .git folder, when it has
	// one: carried beside the snapshot so git works in a copy built from
	// it, and never part of its id, as a checkpoint never holds .git.
	Git string `json:"git,omitempty"`
}

// Errors, in fixed wording for the person: none names a path.
var (
	ErrUnknown   = errors.New("this machine does not hold the folder")
	ErrNotFolder = errors.New("the folder is not an existing directory")
	ErrTooMany   = errors.New("the folder has more files than a snapshot holds")
	ErrChanged   = errors.New("the folder changed while its snapshot was taken; send the turn again")
)

// locks keeps one snapshot or apply of a folder at a time.
var locks sync.Map // ref -> *sync.Mutex

func lockRef(ref string) func() {
	m, _ := locks.LoadOrStore(ref, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (c *Connector) path(ref string) (string, error) {
	p, ok := c.Folders.Path(ref)
	if !ok {
		return "", ErrUnknown
	}
	if c.Prepare != nil {
		if err := c.Prepare(p); err != nil {
			return "", ErrNotFolder
		}
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return "", ErrNotFolder
	}
	return p, nil
}

// Snapshot takes a snapshot of the folder ref: every file hashed (but one
// with the size, time and mode the folder's last snapshot saw, which keeps
// that hash), every file and content tree kept as a blob, in Store and in
// Remote. It answers the snapshot id.
func (c *Connector) Snapshot(ctx context.Context, ref string) (Taken, error) {
	defer lockRef(ref)()
	dir, err := c.path(ref)
	if err != nil {
		return Taken{}, err
	}
	prev := c.lastSnapshot(ref)
	snap, err := workspace.WalkParallel(dir, prev, hashUntil(ctx), 0)
	if err := ctx.Err(); err != nil {
		return Taken{}, err
	}
	if err != nil {
		return Taken{}, ErrNotFolder
	}
	if snap.Truncated() {
		return Taken{}, ErrTooMany
	}
	root, trees, ok := workspace.ContentTrees(snap)
	if !ok {
		return Taken{}, ErrChanged
	}
	hashes, err := c.keepFiles(ctx, dir, snap)
	if err != nil {
		return Taken{}, err
	}
	for h, b := range trees {
		if _, err := c.Store.PutBytes(b); err != nil {
			return Taken{}, err
		}
		hashes = append(hashes, h)
	}
	times, err := c.Store.PutBytes(workspace.FormatTimes(snap))
	if err != nil {
		return Taken{}, err
	}
	hashes = append(hashes, times)
	gitTree := ""
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
		h, more, err := c.keepTree(ctx, filepath.Join(dir, ".git"))
		if err != nil {
			return Taken{}, err
		}
		gitTree, hashes = h, append(hashes, more...)
	}
	if c.Remote != nil {
		if err := blobs.Copy(ctx, c.Store, c.Remote, hashes); err != nil {
			return Taken{}, err
		}
	}
	c.keepSnapshot(ref, snap)
	return Taken{ID: root, Times: times, Files: snap.Len(), Git: gitTree}, nil
}

// keepTree keeps every file under dir, and its content trees, in Store, and
// answers its top tree and every blob it names.
func (c *Connector) keepTree(ctx context.Context, dir string) (string, []string, error) {
	snap, err := workspace.WalkParallel(dir, nil, hashUntil(ctx), 0)
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err != nil || snap.Truncated() {
		return "", nil, ErrTooMany
	}
	root, trees, ok := workspace.ContentTrees(snap)
	if !ok {
		return "", nil, ErrChanged
	}
	hashes, err := c.keepFiles(ctx, dir, snap)
	if err != nil {
		return "", nil, err
	}
	for h, b := range trees {
		if _, err := c.Store.PutBytes(b); err != nil {
			return "", nil, err
		}
		hashes = append(hashes, h)
	}
	return root, hashes, nil
}

// keepWorkers is how many files keepFiles keeps at once: each is a clone
// and a read, so one at a time leaves the disk mostly idle.
const keepWorkers = 8

// keepFiles keeps every file of snap, read under dir, in Store, and answers
// their hashes in path order. A file no longer as the walk saw it is
// ErrChanged.
func (c *Connector) keepFiles(ctx context.Context, dir string, snap *workspace.Snapshot) ([]string, error) {
	paths := snap.Paths()
	hashes := make([]string, len(paths))
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		bad   atomic.Bool
	)
	next := make(chan int)
	for range keepWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if err := c.keepFile(dir, paths[i], snap); err != nil {
					once.Do(func() { first = err; bad.Store(true) })
				}
			}
		}()
	}
	for i, rel := range paths {
		f, _ := snap.File(rel)
		hashes[i] = f.Hash
		if ctx.Err() != nil || bad.Load() {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if first != nil {
		return nil, first
	}
	return hashes, nil
}

// keepFile keeps the file rel of snap, read under dir, in Store.
func (c *Connector) keepFile(dir, rel string, snap *workspace.Snapshot) error {
	f, _ := snap.File(rel)
	if c.Store.Held(f.Hash) {
		return nil
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if f.Mode&fs.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return ErrChanged
		}
		if h, err := c.Store.PutBytes(workspace.LinkContent(target)); err != nil || h != f.Hash {
			return ErrChanged
		}
		return nil
	}
	if _, err := c.Store.PutFile(p, f.Hash, c.Link); err != nil {
		if errors.Is(err, blobs.ErrBadHash) || errors.Is(err, fs.ErrNotExist) {
			return ErrChanged
		}
		return err
	}
	return nil
}

// hashUntil hashes as workspace.Walk does until ctx ends, and then hashes
// no more: the walk ends at once, and its caller sees ctx's end.
func hashUntil(ctx context.Context) workspace.Hasher {
	return func(rel, p string, info fs.FileInfo) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return workspace.Hash(rel, p, info)
	}
}

func (c *Connector) snapFile(ref string) string {
	return filepath.Join(c.State, "snapshots", ref+".json")
}

// lastSnapshot is what the folder's last snapshot found, or nil.
func (c *Connector) lastSnapshot(ref string) *workspace.Snapshot {
	if c.State == "" {
		return nil
	}
	b, err := os.ReadFile(c.snapFile(ref))
	if err != nil {
		return nil
	}
	var s workspace.Snapshot
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

func (c *Connector) keepSnapshot(ref string, s *workspace.Snapshot) {
	if c.State == "" {
		return
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	p := c.snapFile(ref)
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// Lend opens the file rel of the folder ref while it is a regular file with
// f's size, modification time and mode (isolation.Lender).
func (c *Connector) Lend(ref, rel string, f workspace.File) (io.ReadCloser, func() bool, bool) {
	dir, err := c.path(ref)
	if err != nil || !f.Mode.IsRegular() {
		return nil, nil, false
	}
	rt, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, false
	}
	name := filepath.FromSlash(rel)
	// same opens the folder again, so it can be asked after the reader is
	// closed.
	same := func() bool {
		r2, err := os.OpenRoot(dir)
		if err != nil {
			return false
		}
		defer r2.Close()
		return sameStat(r2, name, f)
	}
	if !same() {
		rt.Close()
		return nil, nil, false
	}
	file, err := rt.Open(name)
	if err != nil {
		rt.Close()
		return nil, nil, false
	}
	return &lent{File: file, rt: rt}, same, true
}

type lent struct {
	*os.File
	rt *os.Root
}

func (l *lent) Close() error {
	err := l.File.Close()
	l.rt.Close()
	return err
}

// Applied is what an apply changed in the folder.
type Applied struct {
	// Files were written or removed, sorted.
	Files []string `json:"files"`
}

// Conflict refuses an apply: these files of the folder are neither as the
// changes start from nor as they end. Nothing was written.
type Conflict struct {
	Files []ConflictFile `json:"files"`
}

// ConflictFile is one file an apply would not overwrite, and why.
type ConflictFile struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

func (c *Conflict) Error() string {
	paths := make([]string, len(c.Files))
	for i, f := range c.Files {
		paths[i] = f.Path
	}
	return fmt.Sprintf("apply refused: %d file(s) in your folder changed since the checkpoint: %s",
		len(c.Files), strings.Join(paths, ", "))
}

// Why a file conflicts, in the refusal.
const (
	WhyChanged = "changed in your folder since the checkpoint"
	WhyAdded   = "added in your folder since the checkpoint"
	WhyRemoved = "removed from your folder since the checkpoint"
	WhyNotFile = "is a folder or cannot be read in your folder"
	// WhyNoFolder: the file's own folder, or one above it, is no folder.
	WhyNoFolder = "belongs in a folder that is a file or a link in your folder"
	// WhyMarkers: a merge left both sides in the file between conflict
	// markers, and they are still there.
	WhyMarkers = "still holds conflict markers from the merge; ask the agent to finish combining it"
	// WhyBeside: a session in several folders made a file beside them, at
	// the top of its copy, which none of the folders holds.
	WhyBeside = "is beside the folders, in none of them; ask the agent to move it into a folder, or discard it"
)

// More apply refusals, in fixed wording.
var (
	ErrTruncated = errors.New("the copy had more files than a checkpoint holds, so its changes cannot be applied")
	ErrChangeset = errors.New("the turn's changes could not be read from the control plane's store")
	ErrPartial   = errors.New("the changes could not all be written to your folder; files lists those that were")
)

// MaxChangeset bounds a changeset blob.
const MaxChangeset = 256 << 20

// applyMu keeps one apply at a time in this process, whatever folder it
// writes: two folders, one inside the other, must not both pass the
// conflict check and then write over each other. A preview does not take it.
var applyMu sync.Mutex

// Apply makes the folder ref as the changeset to holds it, from as the
// changeset from holds it ("" is the copy's base): every file the two
// differ on. It is all or nothing for conflicts: each file must be as at
// from, or already as at to; any other refuses the whole apply with a
// *Conflict, and nothing is written. What the folder holds at a path is what
// a checkpoint would list there (lookup). Then every new file is written out
// of sight (staged) before any is put in place, so a file that cannot be read
// from the store writes nothing either. After staging, each file to write or
// remove is looked at again before the first is put in place: one the person
// changed meanwhile refuses the apply as the check would have. Only a
// failure while files are put in place leaves some written; ErrPartial says
// so, and Applied lists them.
func (c *Connector) Apply(ctx context.Context, ref, from, to string) (Applied, error) {
	return c.ApplyPlaces(ctx, []workspace.Place{{Ref: ref}}, from, to, false)
}

// ApplyPlaces is Apply for a workspace's folders: each place's part of the
// changesets (its files, under its name) into its folder. Conflicts are checked
// in every folder before any is written, and a conflict in one refuses the
// whole apply, so a turn's changes across folders land together or not at all.
// It is still not one write across folders on disk: a failure while files are
// put in place in one folder, after another was written, is ErrPartial, with
// the files written. Paths in what it answers are the changesets' own, each
// starting with its folder's name.
func (c *Connector) ApplyPlaces(ctx context.Context, places []workspace.Place, from, to string, checkMarkers bool) (Applied, error) {
	applyMu.Lock()
	defer applyMu.Unlock()
	csFrom, csTo, err := c.changesets(ctx, from, to)
	if err != nil {
		return Applied{}, err
	}
	if len(places) > 1 {
		// Every folder's conflicts, before any folder is written. One folder needs
		// no first pass: its own apply checks every file before it writes one.
		var all Conflict
		out, err := beside(csFrom, csTo, places)
		if err != nil {
			return Applied{}, err
		}
		all.Files = append(all.Files, out...)
		for _, p := range places {
			_, _, err := c.apply(ctx, p.Ref, csFrom.Under(p), csTo.Under(p), checkOnly, checkMarkers)
			var cf *Conflict
			switch {
			case errors.As(err, &cf):
				for _, f := range cf.Files {
					all.Files = append(all.Files, ConflictFile{p.Prefix() + f.Path, f.Why})
				}
			case err != nil:
				return Applied{}, err
			}
		}
		if len(all.Files) > 0 {
			sort.Slice(all.Files, func(i, j int) bool { return all.Files[i].Path < all.Files[j].Path })
			return Applied{}, &all
		}
	}
	out := Applied{Files: []string{}}
	for _, p := range places {
		done, _, err := c.apply(ctx, p.Ref, csFrom.Under(p), csTo.Under(p), writeFiles, checkMarkers)
		for _, f := range done.Files {
			out.Files = append(out.Files, p.Prefix()+f)
		}
		if err != nil {
			sort.Strings(out.Files)
			if len(out.Files) > 0 && !errors.Is(err, ErrPartial) {
				// An earlier folder was written: say so, as a partial apply.
				return out, ErrPartial
			}
			var cf *Conflict
			if errors.As(err, &cf) {
				for i := range cf.Files {
					cf.Files[i].Path = p.Prefix() + cf.Files[i].Path
				}
			}
			return out, err
		}
	}
	sort.Strings(out.Files)
	return out, nil
}

// Preview is Apply's dry run, by the same code up to the point where Apply
// would write: each file Apply would write or remove, and each file that
// would refuse it, with its change and its diff against the folder now.
// Conflicts are in the preview, not an error. It writes nothing in the
// folder; it may fetch the small files it shows into this machine's store.
func (c *Connector) Preview(ctx context.Context, ref, from, to string) (queue.PreviewResult, error) {
	return c.PreviewPlaces(ctx, []workspace.Place{{Ref: ref}}, from, to, false)
}

// PreviewPlaces is ApplyPlaces' dry run: each folder's preview, its paths
// under the folder's name, as one.
func (c *Connector) PreviewPlaces(ctx context.Context, places []workspace.Place, from, to string, checkMarkers bool) (queue.PreviewResult, error) {
	csFrom, csTo, err := c.changesets(ctx, from, to)
	if err != nil {
		return queue.PreviewResult{}, err
	}
	out := queue.PreviewResult{Files: []queue.PreviewFile{}}
	if len(places) > 1 {
		steps, err := workspace.Between(csFrom, csTo)
		if err != nil {
			return queue.PreviewResult{}, ErrChangeset
		}
		for _, st := range steps {
			if inPlace(st.Path, places) {
				continue
			}
			change := "modified"
			switch {
			case st.From == nil:
				change = "added"
			case st.To == nil:
				change = "deleted"
			}
			out.Files = append(out.Files, queue.PreviewFile{Path: st.Path, Change: change})
			out.Conflicts = append(out.Conflicts, queue.ApplyConflict{Path: st.Path, Why: WhyBeside})
			out.Count++
		}
	}
	for _, p := range places {
		_, one, err := c.apply(ctx, p.Ref, csFrom.Under(p), csTo.Under(p), dryRun, checkMarkers)
		if err != nil {
			return queue.PreviewResult{}, err
		}
		for _, f := range one.Files {
			f.Path = p.Prefix() + f.Path
			out.Files = append(out.Files, f)
		}
		for _, f := range one.Conflicts {
			f.Path = p.Prefix() + f.Path
			out.Conflicts = append(out.Conflicts, f)
		}
		out.Count += one.Count
		out.Truncated = out.Truncated || one.Truncated
		out.DiffsCut = out.DiffsCut || one.DiffsCut
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	sort.Slice(out.Conflicts, func(i, j int) bool { return out.Conflicts[i].Path < out.Conflicts[j].Path })
	return out, nil
}

// beside are the files the changesets change outside every place: files a
// turn put at the top of a copy of several folders, beside them, which no
// folder holds. Each refuses the apply, as a conflict does, so nothing a turn
// made is dropped without a word.
func beside(csFrom, csTo *workspace.Changeset, places []workspace.Place) ([]ConflictFile, error) {
	steps, err := workspace.Between(csFrom, csTo)
	if err != nil {
		return nil, ErrChangeset
	}
	var out []ConflictFile
	for _, st := range steps {
		if !inPlace(st.Path, places) {
			out = append(out, ConflictFile{st.Path, WhyBeside})
		}
	}
	return out, nil
}

// inPlace says whether a changeset's path is inside one of places.
func inPlace(path string, places []workspace.Place) bool {
	for _, p := range places {
		if pre := p.Prefix(); strings.HasPrefix(path, pre) && len(path) > len(pre) {
			return true
		}
	}
	return false
}

// changesets reads the changesets an apply goes from ("" is the copy's base)
// and to.
func (c *Connector) changesets(ctx context.Context, from, to string) (csFrom, csTo *workspace.Changeset, err error) {
	if from != "" {
		if csFrom, err = c.changeset(ctx, from); err != nil {
			return nil, nil, err
		}
	}
	if csTo, err = c.changeset(ctx, to); err != nil {
		return nil, nil, err
	}
	return csFrom, csTo, nil
}

// How far apply goes: writeFiles puts the files in place; dryRun stops after the
// conflict check and answers the preview; checkOnly stops there too and
// answers only the conflicts, as the *Conflict a write would refuse with.
type applyMode int

const (
	writeFiles applyMode = iota
	dryRun
	checkOnly
)

// apply is one folder's part of an apply, or its preview: the changesets from
// and to, already read. A dry run or a check stops after the conflict check,
// before anything is staged or written. A write runs under applyMu, which the
// caller holds; a dry run writes nothing, so it keeps no other apply out: one
// into a folder inside this one may run meanwhile, and the preview shows the
// folder as it read it. With checkMarkers (a merge left conflict markers in the
// copy), a file to write that still holds them (workspace.HasMergeMarkers) is a
// conflict (WhyMarkers), never written: the agent has not combined it.
func (c *Connector) apply(ctx context.Context, ref string, csFrom, csTo *workspace.Changeset, mode applyMode, checkMarkers bool) (Applied, queue.PreviewResult, error) {
	var none queue.PreviewResult
	dry := mode != writeFiles
	defer lockRef(ref)()
	dir, err := c.path(ref)
	if err != nil {
		return Applied{}, none, err
	}
	if csTo.Truncated || (csFrom != nil && csFrom.Truncated) {
		return Applied{}, none, ErrTruncated
	}
	steps, err := workspace.Between(csFrom, csTo)
	if err != nil {
		return Applied{}, none, ErrChangeset
	}
	// Every write goes through the folder as a root, so no symbolic link in
	// it can take a write outside it.
	rt, err := os.OpenRoot(dir)
	if err != nil {
		return Applied{}, none, ErrNotFolder
	}
	defer rt.Close()
	type op struct {
		rel        string
		to         *workspace.File
		chmod      bool
		staged     string
		removeOnly bool
		respelt    string // the path removed that rel spells in another way
		// What the conflict check found, for the look again before any
		// write: the step, whether a file stood there, its stat and, when
		// the check read it, its hash.
		step  workspace.Step
		there bool
		seen  fs.FileInfo
		hash  string
	}
	var ops []*op
	var conflict Conflict
	look := newLookup(rt, steps)
	// shown, for a dry run: each file an op or a conflict names, and the
	// folder's file at that path as the check found it.
	var shown []shownFile
	for _, st := range steps {
		nops, nconf := len(ops), len(conflict.Files)
		name := filepath.FromSlash(st.Path)
		fa, fb := st.From, st.To
		there, respelt, why := look.at(st)
		var seen fs.FileInfo
		if there {
			seen, _ = rt.Lstat(name)
		}
		// A file the base recorded without a hash is as the base saw it,
		// by size, modification time and mode: one still so in the folder
		// is as at from, without reading it.
		if there && fa != nil && fa.Hash == "" && sameStat(rt, name, *fa) {
			ops = append(ops, &op{rel: st.Path, to: fb, removeOnly: fb == nil, step: st, there: there, seen: seen})
			shown = append(shown, shownFile{rel: st.Path, to: fb, exists: true, mode: fa.Mode})
			continue
		}
		var cur string
		var mode fs.FileMode
		var exists bool
		if there {
			var err error
			if cur, mode, exists, err = hashIn(rt, name); err != nil {
				why = WhyNotFile
			}
		}
		// The same content is the same file only as the same kind: a
		// regular file holding "link:x" is not the link to x. And a file is a
		// state only with that state's mode too — one chmod'ed since the base
		// is not what the base recorded, as a changed file is not. A link's
		// mode bits are the machine's, so its kind is all that compares.
		contentIs := func(f *workspace.File) bool {
			return f != nil && exists && cur == f.Hash && mode&fs.ModeSymlink == f.Mode&fs.ModeSymlink
		}
		is := func(f *workspace.File) bool {
			return contentIs(f) && (!mode.IsRegular() || mode == f.Mode)
		}
		switch {
		case why != "":
			conflict.Files = append(conflict.Files, ConflictFile{st.Path, why})
		case contentIs(fb):
			switch {
			case !mode.IsRegular() || mode == fb.Mode:
				// The target is already there, mode and all.
			case is(fa):
				ops = append(ops, &op{rel: st.Path, to: fb, chmod: true})
			default:
				conflict.Files = append(conflict.Files, ConflictFile{st.Path, WhyChanged})
			}
		case fb == nil && !exists:
		case is(fa), fa == nil && !exists:
			ops = append(ops, &op{rel: st.Path, to: fb, removeOnly: fb == nil, respelt: respelt})
		case fa == nil:
			conflict.Files = append(conflict.Files, ConflictFile{st.Path, WhyAdded})
		case !exists:
			conflict.Files = append(conflict.Files, ConflictFile{st.Path, WhyRemoved})
		default:
			conflict.Files = append(conflict.Files, ConflictFile{st.Path, WhyChanged})
		}
		if len(ops) > nops {
			o := ops[len(ops)-1]
			o.step, o.there, o.seen, o.hash = st, there, seen, cur
		}
		if len(ops) > nops || len(conflict.Files) > nconf {
			shown = append(shown, shownFile{rel: st.Path, to: fb, exists: exists || why == WhyNotFile, mode: mode, unread: why == WhyNotFile})
		}
	}
	// While a merge's markers may be in the copy, every file to write is read
	// for them, whatever its path: a rename or a later update cannot carry one
	// past this check.
	kept := ops[:0]
	for _, o := range ops {
		if checkMarkers && o.to != nil && !o.chmod && o.to.Mode.IsRegular() {
			marked, err := c.hasMergeMarkers(ctx, o.to.Hash)
			if err != nil {
				return Applied{}, none, err
			}
			if marked {
				conflict.Files = append(conflict.Files, ConflictFile{o.rel, WhyMarkers})
				continue
			}
		}
		kept = append(kept, o)
	}
	ops = kept
	sort.Slice(conflict.Files, func(i, j int) bool { return conflict.Files[i].Path < conflict.Files[j].Path })
	if mode == checkOnly {
		if len(conflict.Files) > 0 {
			return Applied{}, none, &conflict
		}
		return Applied{}, none, nil
	}
	if dry {
		p, err := c.preview(ctx, rt, shown, conflict.Files)
		return Applied{}, p, err
	}
	if len(conflict.Files) > 0 {
		return Applied{}, none, &conflict
	}
	// Stage: every file to write, out of sight under one folder at the top.
	stage := ".agentbus-apply-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	staged := false
	defer func() {
		if staged {
			removeAllIn(rt, stage)
		}
	}()
	src := blobs.Local{Store: c.Store}
	if c.Remote != nil {
		src.From = c.Remote
		// Every file to write, fetched at once: few requests, not one each.
		var hs []string
		for _, o := range ops {
			if o.to != nil && !o.chmod {
				hs = append(hs, o.to.Hash)
			}
		}
		if err := src.Fetch(ctx, hs); err != nil {
			if err := ctx.Err(); err != nil {
				return Applied{}, none, err
			}
			return Applied{}, none, ErrChangeset
		}
	}
	for i, o := range ops {
		if err := ctx.Err(); err != nil {
			return Applied{}, none, err
		}
		if o.to == nil || o.chmod {
			continue
		}
		if !staged {
			if err := rt.Mkdir(stage, 0o700); err != nil {
				return Applied{}, none, errors.New("connector: the folder cannot be written")
			}
			staged = true
		}
		o.staged = stage + string(filepath.Separator) + strconv.Itoa(i)
		if err := c.stageFile(ctx, rt, src, o.staged, *o.to); err != nil {
			return Applied{}, none, err
		}
	}
	// Put in place: every removal first, deepest first, so a file can take
	// the name of a folder the changes emptied; then every write, in path
	// order, so a folder is made before what goes in it. A folder emptied
	// stays, as before.
	if err := ctx.Err(); err != nil {
		return Applied{}, none, err // stopped before any file is put in place
	}
	// The person may write in the folder while the files are fetched and
	// staged. Each file to write or remove is looked at again, before the
	// first is: one no longer as the check found it, nor as the apply leaves
	// it, refuses the apply as the check would have, and nothing is written.
	again := newLookup(rt, steps)
	for _, o := range ops {
		if why := changedSince(rt, again, o.step, o.there, o.seen, o.hash); why != "" {
			conflict.Files = append(conflict.Files, ConflictFile{o.rel, why})
		}
	}
	if len(conflict.Files) > 0 {
		sort.Slice(conflict.Files, func(i, j int) bool { return conflict.Files[i].Path < conflict.Files[j].Path })
		return Applied{}, none, &conflict
	}
	out := Applied{Files: []string{}}
	fail := func() (Applied, queue.PreviewResult, error) {
		sort.Strings(out.Files)
		return out, none, ErrPartial
	}
	for i := len(ops) - 1; i >= 0; i-- {
		o := ops[i]
		if o.to != nil {
			continue
		}
		name := filepath.FromSlash(o.rel)
		if err := rt.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail()
		}
		out.Files = append(out.Files, o.rel)
	}
	// A folder whose name changes only in case is emptied by now, and is made
	// in its new spelling. One that still holds a file keeps its name.
	renamed := map[string]bool{}
	for _, o := range ops {
		if d := respeltFolder(o.respelt, o.rel); d != "" && !renamed[d] {
			renamed[d] = true
			_ = removeEmptied(rt, filepath.FromSlash(d))
		}
	}
	for _, o := range ops {
		if o.to == nil {
			continue
		}
		name := filepath.FromSlash(o.rel)
		var err error
		if o.chmod {
			err = rt.Chmod(name, o.to.Mode.Perm())
		} else {
			if d := filepath.Dir(name); d != "." {
				err = rt.MkdirAll(d, 0o755)
			}
			if err == nil {
				err = placeStaged(rt, o.staged, name)
			}
		}
		if err != nil {
			return fail()
		}
		out.Files = append(out.Files, o.rel)
	}
	sort.Strings(out.Files)
	return out, none, nil
}

// shownFile is one file of a dry run: the state the apply would leave it in
// (to; nil: removed), and the folder's file there now.
type shownFile struct {
	rel    string
	to     *workspace.File
	exists bool        // the folder holds something at rel
	mode   fs.FileMode // what it holds, when it was read
	unread bool        // it could not be read: a folder, or unreadable
}

// preview is a dry run's answer: each shown file's change against the
// folder now, with a unified diff when both sides are small text, within
// the 1 MiB of diffs a turn's turn.changed holds.
func (c *Connector) preview(ctx context.Context, rt *os.Root, shown []shownFile, conflicts []ConflictFile) (queue.PreviewResult, error) {
	p := queue.PreviewResult{Files: make([]queue.PreviewFile, 0, len(shown)), Count: len(shown)}
	for _, f := range conflicts {
		p.Conflicts = append(p.Conflicts, queue.ApplyConflict{Path: f.Path, Why: f.Why, Markers: f.Why == WhyMarkers})
	}
	small := func(f *workspace.File) bool {
		return f != nil && f.Mode&fs.ModeSymlink == 0 && f.Size <= workspace.MaxFileDiff
	}
	src := blobs.Local{Store: c.Store}
	if c.Remote != nil {
		src.From = c.Remote
		// Every file to show, fetched at once, as the apply fetches them.
		var hs []string
		for _, f := range shown {
			if small(f.to) {
				hs = append(hs, f.to.Hash)
			}
		}
		if err := src.Fetch(ctx, hs); err != nil {
			if err := ctx.Err(); err != nil {
				return queue.PreviewResult{}, err
			}
			return queue.PreviewResult{}, ErrChangeset
		}
	}
	budget := workspace.MaxFileDiffs
	for _, f := range shown {
		if err := ctx.Err(); err != nil {
			return queue.PreviewResult{}, err
		}
		pf := queue.PreviewFile{Path: f.rel, Change: "modified"}
		switch {
		case !f.exists:
			pf.Change = "added"
		case f.to == nil:
			pf.Change = "deleted"
		}
		// Each side: the folder now, and what the apply would leave.
		var old, cur []byte
		ok := !f.unread
		if ok && f.exists {
			old, ok = readRegularIn(rt, filepath.FromSlash(f.rel), workspace.MaxFileDiff)
		}
		if ok && f.to != nil {
			if !small(f.to) {
				ok = false
			} else if b, err := blobs.ReadAll(ctx, src, f.to.Hash, workspace.MaxFileDiff); err == nil {
				cur = b
			} else {
				ok = false
			}
		}
		if ok && (!isText(old) || !isText(cur)) {
			ok = false
		}
		switch {
		case !ok, budget == 0:
			pf.DiffLeftOut, p.DiffsCut = true, p.DiffsCut || ok
		case bytes.Equal(old, cur):
			// Only its mode changes.
		default:
			d, add, rem := workspace.UnifiedCount(f.rel, old, cur, pf.Change)
			if len(d) > budget {
				pf.DiffLeftOut, p.DiffsCut = true, true
				break
			}
			budget -= len(d)
			pf.Diff, pf.Added, pf.Removed = &d, add, rem
		}
		p.Files = append(p.Files, pf)
	}
	return p, nil
}

// readRegularIn reads the regular file name of rt, at most max bytes.
func readRegularIn(rt *os.Root, name string, max int) ([]byte, bool) {
	fi, err := rt.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > int64(max) {
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

// isText: valid UTF-8 with no NUL byte.
func isText(b []byte) bool {
	return bytes.IndexByte(b, 0) < 0 && utf8.Valid(b)
}

// changeset reads the changeset blob h.
func (c *Connector) changeset(ctx context.Context, h string) (*workspace.Changeset, error) {
	var src blobs.Source = c.Store
	if c.Remote != nil {
		src = blobs.Local{Store: c.Store, From: c.Remote}
	}
	b, err := blobs.ReadAll(ctx, src, h, MaxChangeset)
	if err != nil {
		return nil, ErrChangeset
	}
	var cs workspace.Changeset
	if json.Unmarshal(b, &cs) != nil {
		return nil, ErrChangeset
	}
	return &cs, nil
}

// hasMergeMarkers reads blob h from the store as a stream and reports
// whether it holds an update's merge markers. A blob that cannot be read
// refuses the apply (ErrChangeset), so a marked file is never written for want
// of a read.
func (c *Connector) hasMergeMarkers(ctx context.Context, h string) (bool, error) {
	src := blobs.Local{Store: c.Store}
	if c.Remote != nil {
		src.From = c.Remote
	}
	rc, err := src.Open(ctx, h)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, ErrChangeset
	}
	defer rc.Close()
	marked, err := workspace.HasMergeMarkers(rc)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, ErrChangeset
	}
	return marked, nil
}

// stageFile writes the file f, from its blob, as tmp of rt.
func (c *Connector) stageFile(ctx context.Context, rt *os.Root, src blobs.Local, tmp string, f workspace.File) error {
	if f.Mode&fs.ModeSymlink != 0 {
		b, err := blobs.ReadAll(ctx, src, f.Hash, 64<<10)
		if err != nil {
			return ErrChangeset
		}
		target, ok := workspace.LinkTarget(b)
		if !ok {
			return ErrChangeset
		}
		return rt.Symlink(target, tmp)
	}
	if err := src.Fetch(ctx, []string{f.Hash}); err != nil {
		return ErrChangeset
	}
	p := c.Store.Path(f.Hash)
	// A clone of the blob where the disk has them: no bytes copied.
	if !cloneInto(rt, tmp, p) {
		if err := copyInto(rt, tmp, p, f.Mode.Perm()); err != nil {
			return errors.New("connector: the folder cannot be written")
		}
	}
	if err := rt.Chmod(tmp, f.Mode.Perm()); err != nil { // the umask narrowed it
		return errors.New("connector: the folder cannot be written")
	}
	got, _, ok, err := hashIn(rt, tmp)
	if err != nil || !ok || got != f.Hash {
		return ErrChangeset
	}
	return nil
}

// placeStaged puts the staged file at name, by rename, so a reader never
// sees half a file. An empty folder in the way goes first: the changes
// emptied it.
func placeStaged(rt *os.Root, staged, name string) error {
	if fi, err := rt.Lstat(name); err == nil && fi.IsDir() {
		if err := rt.Remove(name); err != nil {
			return err
		}
	}
	return rt.Rename(staged, name)
}

// removeAllIn removes the folder name of rt and all it holds.
func removeAllIn(rt *os.Root, name string) {
	_ = rt.RemoveAll(name)
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

// changedSince says why the file of st, about to be written or removed, is no
// longer as the conflict check found it (there; seen, and hash when it read
// it) nor as st leaves it, or "" when it is one of the two. look is a new
// lookup, before anything is written. A file whose stat changed is read
// again, so one touched and not changed is still as it was.
func changedSince(rt *os.Root, look *lookup, st workspace.Step, there bool, seen fs.FileInfo, hash string) string {
	now, _, why := look.at(st)
	if why != "" {
		return why
	}
	gone := WhyRemoved
	if !there || st.To == nil {
		gone = ""
	}
	if !now {
		return gone
	}
	name := filepath.FromSlash(st.Path)
	if there && seen != nil {
		if fi, err := rt.Lstat(name); err == nil && fi.Mode() == seen.Mode() && fi.Size() == seen.Size() && fi.ModTime().Equal(seen.ModTime()) {
			return ""
		}
	}
	cur, mode, exists, err := hashIn(rt, name)
	switch {
	case err != nil:
		return WhyNotFile
	case !exists:
		return gone
	case there && seen != nil && hash != "" && cur == hash && mode == seen.Mode():
		return ""
	case st.To != nil && cur == st.To.Hash && mode&fs.ModeSymlink == st.To.Mode&fs.ModeSymlink && (!mode.IsRegular() || mode == st.To.Mode):
		return ""
	case !there:
		return WhyAdded
	}
	return WhyChanged
}

// sameStat says whether the file name of rt has f's size, modification time
// and mode.
func sameStat(rt *os.Root, name string, f workspace.File) bool {
	fi, err := rt.Lstat(name)
	return err == nil && fi.Mode() == f.Mode && fi.Size() == f.Size && fi.ModTime().UnixNano() == f.ModTime
}

// hashIn hashes the file name of rt as workspace.Walk does. A missing file
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
		sum := sha256.Sum256(workspace.LinkContent(target))
		return hex.EncodeToString(sum[:]), fi.Mode(), true, nil
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

// Jobs is the connector as the machine's runner calls it (runner.FolderSide):
// the answers in the queue's words.
type Jobs struct{ *Connector }

// FolderJob is Folders.Add, for a folder job: a folder a person typed,
// checked and registered. A leading ~ is this machine's home (folders.Expand),
// and the folder is registered by its expanded path. Its errors are one
// sentence each, naming no path. A job stopped while the folder was checked
// registers nothing.
func (j Jobs) FolderJob(ctx context.Context, path string) (string, error) {
	home, _ := os.UserHomeDir()
	path, err := folders.Expand(path, home)
	if err != nil {
		return "", err
	}
	if err := folders.Check(path); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", errors.New("the folder job was stopped")
	}
	return j.Folders.Register(path)
}

// PreviewJob is PreviewPlaces, for a preview job: Apply's dry run.
func (j Jobs) PreviewJob(ctx context.Context, places []workspace.Place, from, to string, checkMarkers bool) (queue.PreviewResult, error) {
	return j.PreviewPlaces(ctx, places, from, to, checkMarkers)
}

// ApplyJob is Apply, for an apply job: the files written, or the conflicts
// that refused it; partial when not all files could be put in place.
func (j Jobs) ApplyJob(ctx context.Context, places []workspace.Place, from, to string, checkMarkers bool) (files []string, conflicts []queue.ApplyConflict, partial bool, err error) {
	done, err := j.ApplyPlaces(ctx, places, from, to, checkMarkers)
	var cf *Conflict
	switch {
	case errors.As(err, &cf):
		out := make([]queue.ApplyConflict, len(cf.Files))
		for i, x := range cf.Files {
			out[i] = queue.ApplyConflict{Path: x.Path, Why: x.Why, Markers: x.Why == WhyMarkers}
		}
		return nil, out, false, nil
	case errors.Is(err, ErrPartial):
		return done.Files, nil, true, nil
	}
	return done.Files, nil, false, err
}
