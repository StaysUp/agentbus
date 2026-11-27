package queue

import (
	"database/sql"
	"errors"
)

// Lanes: which place a job takes on its runner. A runner takes each lane on
// its own, and control counts each lane apart (Take):
//
//   - folders: a folder job of the runner's machine (folderKinds), in one of
//     its FolderCapacity places, so an apply never waits behind turns;
//   - delegated: a turn an agent started, or a reply delivered into a session
//     an agent started (delegatedJob). At most the settings' limit run on one
//     runner at once ([limits] max_delegated_at_once, read at every take, so a
//     change holds with no restart), and never more than the runner's own
//     capacity; the rest wait, oldest first;
//   - interactive: everything else that is not a folder job: the turns the
//     person sends, replies delivered into the person's own sessions, and the
//     lane jobs (discard, update, close, put-away). Not counted: they start at
//     once, whatever delegated work runs (his call, 2026-11-26).
//
// As CI systems keep work apart: Buildkite routes jobs to queues that agent
// pools serve, and caps a job group with concurrency groups; GitHub Actions
// routes by runner labels and serialises with concurrency groups; Temporal
// polls a task queue per kind of work, each worker bounding its own slots.
// Here the runner is one per machine, so the lanes are its places and control,
// which holds the record, counts them.
//
// A workspace's jobs still run one at a time on its one copy (laneTake): the
// person's message waits for a turn that runs there and for its own session's
// earlier turns, but goes ahead of delegated turns that have not started
// (laneOrder), since those hold no copy yet.

import "fmt"

// Lane is which of a runner's places a take is for.
type Lane string

const (
	LaneFolders     Lane = "folders"
	LaneDelegated   Lane = "delegated"
	LaneInteractive Lane = "interactive"
)

// Known says l is one of the lanes.
func (l Lane) Known() bool {
	return l == LaneFolders || l == LaneDelegated || l == LaneInteractive
}

// delegatedJob is the one rule of a delegated job, in SQL, for a job of alias
// a. It is decided by who asked, never by where the work belongs:
//
//   - a turn an agent asked for, with its turn token, is delegated wherever it
//     lands, its own root session too: its links name the asking turn
//     (Chain.By, set from the token on every such turn, api/tokens.go);
//   - a reply an on delivers (ResultOf) is delegated unless it lands in a
//     session whose own turns are the person's: one that roots its own tree,
//     as every session the person started does (treeOf), and no session an
//     agent started does. The receiving session decides, not the reply's tree,
//     which is that of the session it came from;
//   - every other turn is the person's: the main token's, the cockpit's.
//
// (Codex reviews: evt_ffeb7b5ccdb2ee05, read from the tree alone a turn an
// agent asked for in its own root session counted as the person's; round 3, a
// reply from one of the person's sessions into another counted as delegated.)
// Every reader uses it: Take, the lane order, TurnsAhead, the waiting counts,
// GET /api/v0/runners and Queue.JobDelegated. There is no second copy.
func delegatedJob(a string) string {
	return `(` + a + `.kind = 'turn' AND (COALESCE(json_extract(` + a + `.links, '$.chain.by'), '') != ''
		OR (json_extract(` + a + `.links, '$.result_of') IS NOT NULL AND NOT EXISTS (SELECT 1 FROM jobs rs
			WHERE rs.team = ` + a + `.team AND rs.session = ` + a + `.session AND rs.kind = 'turn' AND rs.tree = rs.session))))`
}

// JobDelegated says the job is delegated work, as delegatedJob decides it.
func (q *Queue) JobDelegated(team, id string) (bool, error) {
	var d bool
	err := q.db.QueryRow(`SELECT `+delegatedJob("j")+` FROM jobs j WHERE j.team = ? AND j.id = ?`, team, id).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return d, err
}

// DelegatedSetting reads how many delegated turns one runner runs at once, as
// the settings say now.
type DelegatedSetting func() int

// ReadDelegatedWith sets how the queue reads the delegated limit, at every
// take. Set it before the queue is used. Without it the limit is
// DefaultDelegatedAtOnce.
func (q *Queue) ReadDelegatedWith(r DelegatedSetting) { q.delegatedAtOnce = r }

// DefaultDelegatedAtOnce is the limit of a queue given no settings reader:
// settings.DefaultMaxDelegatedAtOnce, which the queue does not import (the
// package rule keeps connector, which uses the queue's types, off settings).
// TestDefaultDelegatedIsTheSettings holds the two equal.
const DefaultDelegatedAtOnce = 8

// DelegatedAtOnce is the delegated limit as the settings say now.
func (q *Queue) DelegatedAtOnce() int {
	if q.delegatedAtOnce == nil {
		return DefaultDelegatedAtOnce
	}
	return q.delegatedAtOnce()
}

// laneOf is Take's filter of the jobs of lane l, and how many places the
// runner has in it: its capacity of folder jobs, of delegated turns (the
// settings' limit, and never more than the runner's capacity), or -1 for no
// count.
func (q *Queue) laneOf(l Lane, capacity int) (kinds string, places int) {
	switch l {
	case LaneFolders:
		return `j.kind IN ` + folderKinds, FolderCapacity
	case LaneDelegated:
		return `j.kind NOT IN ` + folderKinds + ` AND ` + delegatedJob("j"), min(capacity, q.DelegatedAtOnce())
	default:
		return `j.kind NOT IN ` + folderKinds + ` AND NOT ` + delegatedJob("j"), -1
	}
}

// DelegatedCount is how the delegated lane stands now, for a screen: the
// limit, the delegated turns each runner has taken or runs, and those waiting
// for a place.
type DelegatedCount struct {
	AtOnce int
	Busy   map[string]int // by runner id
	// Waiting: delegated turns queued and due whose workspace's lane is free,
	// so a place is all they wait for. A turn waiting for an earlier job of
	// its own workspace is not counted.
	Waiting int
	// WaitingFor: the same turns, by the runner that will take each one: the
	// runner holding its workspace's copy, or else the runner of the machine
	// it names, the one seen last. A turn bound to neither (a first turn that
	// names no machine) may go to any runner, so it counts in Waiting only.
	WaitingFor map[string]int
}

// Delegated reads the delegated lane of team.
func (q *Queue) Delegated(team string) (DelegatedCount, error) {
	c := DelegatedCount{AtOnce: q.DelegatedAtOnce(), Busy: map[string]int{}, WaitingFor: map[string]int{}}
	rows, err := q.db.Query(`SELECT a.runner, COUNT(*) FROM attempts a JOIN jobs j ON j.id = a.job
		WHERE j.team = ? AND a.state IN ('taken','running') AND `+delegatedJob("j")+` GROUP BY a.runner`, team)
	if err != nil {
		return c, fmt.Errorf("queue: count attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return c, fmt.Errorf("queue: count attempts: %w", err)
		}
		c.Busy[id] = n
	}
	if err := rows.Err(); err != nil {
		return c, fmt.Errorf("queue: count attempts: %w", err)
	}
	now := nanos(q.now())
	machine := `COALESCE(json_extract(%s, '$.machine'), json_extract(%s, '$.connector'), '')`
	wait, err := q.db.Query(`SELECT COALESCE(
			(SELECT s.runner FROM copies s WHERE s.id = `+jobCopyKey("j")+` AND s.team = j.team),
			(SELECT r.id FROM runners r WHERE r.team = j.team AND r.removed IS NULL AND `+fmt.Sprintf(machine, "j.spec", "j.spec")+` != ''
				AND `+fmt.Sprintf(machine, "r.caps", "r.caps")+` = `+fmt.Sprintf(machine, "j.spec", "j.spec")+` ORDER BY r.seen DESC LIMIT 1), '')
		FROM jobs j WHERE j.team = ? AND j.state = 'queued' AND `+delegatedJob("j")+` AND `+due("j", now)+`
		AND NOT EXISTS (SELECT 1 FROM jobs o WHERE o.team = j.team AND o.id != j.id AND (`+laneTake(now)+`))`, team)
	if err != nil {
		return c, fmt.Errorf("queue: count jobs: %w", err)
	}
	defer wait.Close()
	for wait.Next() {
		var id string
		if err := wait.Scan(&id); err != nil {
			return c, fmt.Errorf("queue: count jobs: %w", err)
		}
		c.Waiting++
		if id != "" {
			c.WaitingFor[id]++
		}
	}
	if err := wait.Err(); err != nil {
		return c, fmt.Errorf("queue: count jobs: %w", err)
	}
	return c, nil
}
