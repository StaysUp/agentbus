package jobs

// A runner reads only its own jobs' blobs: those reached from the jobs of
// each session it runs, or ran a moment ago, and from its own jobs
// (queue.RunnerBlobRoots), as the sweep reaches them (Live). Any other
// blob, held or not, is refused with 403, so a runner cannot read another
// session's files or learn whether they exist.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/staysup/agentbus/queue"
)

// RecentRead is how long after an attempt ends its runner may still read
// its job's blobs.
const RecentRead = 10 * time.Minute

// readScopes keeps, for each runner, the blobs its roots reached when last
// read, so a turn's many reads walk its trees once.
type readScopes struct {
	mu   sync.Mutex
	last map[string]readScope // by runner id
}

type readScope struct {
	key  string // names the roots
	live map[string]bool
}

// mayRead says runner rn may read every blob of hs. The roots are read
// each time; their trees are walked again only when the roots changed, or
// when a blob is not among what they reached, as a tree not yet held when
// last walked may be now.
func (s *Server) mayRead(ctx context.Context, rn queue.Runner, hs []string) (bool, error) {
	roots, err := s.cfg.Queue.RunnerBlobRoots(rn.ID, RecentRead)
	if err != nil {
		return false, err
	}
	key := rootsKey(roots)
	s.reads.mu.Lock()
	last := s.reads.last[rn.ID]
	s.reads.mu.Unlock()
	if last.key == key && reachedAll(last.live, hs) {
		return true, nil
	}
	live, err := Live(ctx, s.cfg.Blobs, roots)
	if err != nil {
		return false, err
	}
	s.reads.mu.Lock()
	if s.reads.last == nil {
		s.reads.last = map[string]readScope{}
	}
	s.reads.last[rn.ID] = readScope{key: key, live: live}
	s.reads.mu.Unlock()
	return reachedAll(live, hs), nil
}

func reachedAll(live map[string]bool, hs []string) bool {
	for _, h := range hs {
		if !live[h] {
			return false
		}
	}
	return true
}

// rootsKey names roots: the same roots, in any order, give the same key.
func rootsKey(r queue.BlobRoots) string {
	sum := sha256.New()
	for _, l := range [][]string{r.Trees, r.Changesets, r.Blobs} {
		l = slices.Clone(l)
		slices.Sort(l)
		sum.Write([]byte(strings.Join(slices.Compact(l), ",") + ";"))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// notYours refuses a read of a blob no job of the runner reaches.
const notYours = "this runner holds no job that reads this blob"
