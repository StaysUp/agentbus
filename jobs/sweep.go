package jobs

// Cleaning the control plane's blob store. A blob stays while a snapshot or
// a changeset the queue still names reaches it (queue.BlobRoots): every job
// but those of a closed session. A session closed, its blobs go at the next
// sweep, once they are older than the grace; a blob some other session
// still names stays, as the same content is one blob.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// SweepGrace is how long a blob nothing names stays: a snapshot being sent,
// or a turn's changes before its finish names them, are named within it.
// blobs.TouchAfter is less, so a blob said to be held stays this long less
// TouchAfter at the least.
const SweepGrace = time.Hour

// SweepEvery is how often the store is swept, besides after a session
// closes.
const SweepEvery = time.Hour

// Live is every blob the roots reach in src: each tree's trees and files,
// each changeset's files and its base's snapshot. A root or a tree no
// longer held reaches nothing; one held but not readable as its kind is an
// error, and nothing is swept.
func Live(ctx context.Context, src blobs.Source, roots queue.BlobRoots) (map[string]bool, error) {
	live := map[string]bool{}
	var trees []string
	for _, h := range roots.Blobs {
		live[h] = true
	}
	read := map[string]bool{}
	for _, h := range roots.Changesets {
		if read[h] {
			continue
		}
		read[h], live[h] = true, true
		b, err := blobs.ReadAll(ctx, src, h, MaxChangeset)
		if errors.Is(err, blobs.ErrNotFound) {
			continue
		}
		var cs workspace.Changeset
		if err != nil || json.Unmarshal(b, &cs) != nil {
			return nil, errors.New("jobs: a changeset could not be read")
		}
		for _, f := range cs.Files {
			for _, v := range []*workspace.File{f.Base, f.Now} {
				if v != nil && v.Hash != "" {
					live[v.Hash] = true
				}
			}
		}
		if cs.Snapshot != "" {
			trees = append(trees, cs.Snapshot)
		}
	}
	trees = append(trees, roots.Trees...)
	// Trees read, apart from blobs live: a file's bytes can be a tree's, and
	// that tree must still be read.
	read = map[string]bool{}
	for len(trees) > 0 {
		h := trees[len(trees)-1]
		trees = trees[:len(trees)-1]
		if read[h] {
			continue
		}
		read[h], live[h] = true, true
		if h == workspace.EmptyTree {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := blobs.ReadAll(ctx, src, h, 64<<20)
		if errors.Is(err, blobs.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, errors.New("jobs: a content tree could not be read")
		}
		es, err := workspace.DecodeTree(b)
		if err != nil {
			return nil, errors.New("jobs: a content tree could not be read")
		}
		for _, e := range es {
			if e.Dir {
				trees = append(trees, e.Hash)
			} else {
				live[e.Hash] = true
			}
		}
	}
	return live, nil
}

// MaxChangeset bounds a changeset blob read for a sweep.
const MaxChangeset = 256 << 20

// Sweeper cleans the control plane's blob store: at start, every Every, and
// soon after Kick (a session closed).
type Sweeper struct {
	Queue *queue.Queue
	Store *blobs.Store
	Grace time.Duration // zero: SweepGrace
	Every time.Duration // zero: SweepEvery
	Logf  func(string, ...any)

	kick chan struct{}
}

// NewSweeper is a Sweeper of q's roots over st.
func NewSweeper(q *queue.Queue, st *blobs.Store, logf func(string, ...any)) *Sweeper {
	return &Sweeper{Queue: q, Store: st, Logf: logf, kick: make(chan struct{}, 1)}
}

// Kick asks for a sweep soon.
func (s *Sweeper) Kick() {
	if s == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Once sweeps the store now.
func (s *Sweeper) Once(ctx context.Context) (kept, removed int, err error) {
	roots, err := s.Queue.BlobRoots()
	if err != nil {
		return 0, 0, err
	}
	live, err := Live(ctx, s.Store, roots)
	if err != nil {
		return 0, 0, err
	}
	grace := s.Grace
	if grace == 0 {
		grace = SweepGrace
	}
	return s.Store.Sweep(ctx, func(h string) bool { return live[h] }, grace)
}

// Run sweeps until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	every := s.Every
	if every == 0 {
		every = SweepEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		start := time.Now()
		kept, removed, err := s.Once(ctx)
		if s.Logf != nil {
			if err != nil {
				s.Logf("blobs.sweep.error")
			} else if removed > 0 {
				s.Logf("blobs.sweep kept=%d removed=%d took=%s", kept, removed, time.Since(start).Round(time.Millisecond))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}
