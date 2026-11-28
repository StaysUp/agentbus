package queue

import (
	"strings"
	"testing"
)

// Every Get reads its workspace's head (workspaceOf): each of its three
// parts searches jobs_workspace, never every job's JSON (2026-11-28: 4 ms a
// read at 1,000 jobs, and 870 reads at a start, 2.8 s of it).
func TestWorkspaceHeadUsesItsIndex(t *testing.T) {
	q, _ := open(t)
	rows, err := q.db.Query("EXPLAIN QUERY PLAN "+workspaceHeadSQL, DefaultTeam, "ws", DefaultTeam, "ws", DefaultTeam, "ws")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(plan, " | ")
	if n := strings.Count(all, "USING INDEX jobs_workspace (team=? AND <expr>=? AND kind=?)"); n != 3 {
		t.Errorf("%d of the three parts search jobs_workspace:\n%s", n, all)
	}
}
