package store

import (
	"regexp"
	"strings"
	"testing"
)

// planOf is SQLite's plan for q, one line per step.
func planOf(t *testing.T, s *Store, q string, args ...any) []string {
	t.Helper()
	rows, err := s.r.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// tableScan matches a step that reads the events table, or one of its
// aliases in these queries, from end to end: by its rows, or by an index it
// cannot search.
var tableScan = regexp.MustCompile(`^SCAN (events|e|e2|c|ts|p|q|r|s|n|d)( |$)`)

// The reads the 2026-11-28 audit found slow each go through an index, so
// none of them reads every event, or every event of a session, again. Each
// case is the SQL the store runs. SQLite's plan does not depend on the data
// here (the store runs no ANALYZE), so an empty store shows the plan a large
// one gets.
func TestHotReadsUseTheirIndexes(t *testing.T) {
	s := newStore(t)
	session := `e.kind = 'turn.started' AND e.turn <> '' AND e.seq <= ? AND e.session = ?`
	for _, c := range []struct {
		name string
		q    string
		args []any
		want []string // each must appear in the plan
		scan string   // the one whole-index read allowed, if any
	}{
		// 74 ms for an event of no session, under the write lock, before.
		{"AppendOnce", countByIDSQL, []any{"", "", "x"}, []string{"COVERING INDEX events_id"}, ""},
		// restarts' one run.ended per hold, under the write lock.
		{"run.ended of a hold", runEndedOfHoldSQL, []any{"s", "h"}, []string{"INDEX events_session_kind (session=? AND kind=?)"}, ""},
		{"EventByID", eventByIDSQL, []any{"", "", "x"}, []string{"INDEX events_id (session=? AND turn=? AND <expr>=?)"}, ""},
		{"WorkspaceOutcome", workspaceOutcomeSQL, []any{"ws", "x"}, []string{"INDEX events_workspace"}, ""},
		{"lastAction", lastActionSQL, []any{"ws"}, []string{"INDEX events_workspace"}, ""},
		{"Applied", appliedSQL, []any{"ws"}, []string{"INDEX events_turn_workspace", "INDEX events_turn_kind", "INDEX events_workspace", "INDEX events_session_kind"}, ""},
		// The page of a session's turns: its calls by events_turn_kind, its
		// number by events_session_kind, not row by row.
		{"Turns", turnsSQL(session), []any{9, 9, 9, "s", 101, 9},
			[]string{"INDEX events_session_kind (session=? AND kind=? AND rowid<?)", "COVERING INDEX events_turn_kind (session=? AND turn=? AND kind=? AND rowid<?)"}, ""},
		{"Turns' totals", turnTotalsSQL(2), []any{9, 9, "s", "t1", "s", "t2"}, []string{"INDEX events_session_kind (session=? AND kind=?"}, ""},
		// A fold from the start reads one index end to end, never the rows;
		// a fold of new events reads their seqs only.
		{"fold from the start", strings.Replace(activitySQL, inRange, wholeRange, 1), []any{0, 9},
			[]string{"COVERING INDEX events_activity"}, "SCAN events USING COVERING INDEX events_activity"},
		{"fold of a range", activitySQL, []any{5, 9}, []string{"INTEGER PRIMARY KEY (rowid>? AND rowid<?)"}, ""},
		{"usage of a range", usageSQL, []any{5, 9}, []string{"INDEX events_kind (kind=? AND seq>? AND seq<?)"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := planOf(t, s, c.q, c.args...)
			all := strings.Join(plan, " | ")
			for _, w := range c.want {
				if !strings.Contains(all, w) {
					t.Errorf("plan lacks %q:\n%s", w, all)
				}
			}
			for _, step := range plan {
				if tableScan.MatchString(step) && step != c.scan {
					t.Errorf("plan reads the table end to end (%q):\n%s", step, all)
				}
			}
		})
	}
}
