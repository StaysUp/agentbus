package queue

// The budget of a request tree. A turn belongs to one request tree, recorded
// on its job when the job is made (treeOf, jobs.tree) and never changed. The
// caps on work agents ask for, [limits] in the settings, are counted by turn
// per tree from that record, never by session: a session may hold turns of
// several trees (a person's own, and a turn an agent of another tree asked for
// there), and only a tree's own turns count against it. They are checked in
// one place, create (checkBudget), which every job goes through: work a turn
// asks for, a reply delivered to it, a reply an on it registered delivers, a
// turn continuing another session. So no path can make a job and skip them.
//
// Prior art, for the shape: LiteLLM records each request's spend against the
// entities it belongs to and checks every budget in one door (common_checks,
// proxy/auth/auth_checks.py). Here the entity is the tree, the record of spend
// is the model.responded events of its turns, and the door is create.
//
// (2026-11-25, replacing three rounds of case fixes: a delivery to an empty
// session started a fresh tree; a session watching itself was held to the job
// caps but never the cost cap; and the cost was summed per session, so
// continuing another session counted that session's whole earlier cost.)

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Limits are the caps on asked work: [limits] as the asking turn's rules say,
// the rules it took at its start, read by the API, which passes them here. A
// negative value is never sent; 0 refuses all asked work of that kind, but for
// MaxCostUSD, where 0 is off.
type Limits struct {
	// MaxDepth: work may be asked for at most this many levels below the
	// person's turn.
	MaxDepth int `json:"max_depth"`
	// MaxPerTurn: jobs one turn may ask for. The ons it registers are not
	// counted: they deliver results, they are not work.
	MaxPerTurn int `json:"max_per_turn"`
	// MaxPerTree: jobs a whole request tree may ask for, over every level:
	// everything that started from one person's session.
	MaxPerTree int `json:"max_per_tree"`
	// MaxCostUSD: what the turns of one request tree may cost in all before
	// it gets no more turns. 0 is off.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// LimitError is a cap on asked work that was reached. Its text says which,
// in fixed wording.
type LimitError struct{ Msg string }

func (e *LimitError) Error() string { return e.Msg }

// CostReader sums what the turns named cost, from the record (the cost_usd of
// their model.responded events).
type CostReader func(turns []string) (float64, error)

// ReadCostWith sets how the queue reads what turns cost, for the cost cap.
// Set it before the queue is used. A queue without one refuses any job a cost
// cap binds, rather than read it as free.
func (q *Queue) ReadCostWith(r CostReader) { q.cost = r }

// treeOf is the request tree a new job of kind belongs to: for a turn, the
// root its chain names, or, for a turn of no tree yet (the person's own, or a
// reply to the person's own session from its own turn), its session, which it
// roots. Other jobs (applies, folders) belong to none.
func treeOf(kind string, links Links, session string) string {
	if kind == KindExec && links.Chain != nil {
		return links.Chain.Root // a command is in the tree of the turn that asked for it
	}
	if kind != KindTurn {
		return ""
	}
	if links.Chain != nil && links.Chain.Root != "" {
		return links.Chain.Root
	}
	return session
}

// checkBudget refuses a new job of tree that would pass a cap of budget. nil
// budget is work no cap binds. Asked work (links name the asking turn) is held
// to the depth cap and counted under the per-turn and per-tree caps; every job
// a cap binds, asked or delivered, is held to the cost cap. It runs in the
// job's own transaction, so requests at once cannot all pass the job caps.
func (q *Queue) checkBudget(tx *sql.Tx, team, tree string, links Links, budget *Limits) error {
	if budget == nil {
		return nil
	}
	if tree == "" {
		return ErrBadState // a cap binds only a turn, and every turn has a tree
	}
	if c := links.Chain; c != nil && c.By != "" {
		if c.Depth < 1 {
			return ErrBadState
		}
		if c.Depth > budget.MaxDepth {
			return &LimitError{fmt.Sprintf("work may be asked for at most %d levels below the person's turn, and this would be level %d (Depth in Settings, Limits)", budget.MaxDepth, c.Depth)}
		}
		if err := countAsked(tx, team, *c, *budget); err != nil {
			return err
		}
	}
	return q.checkCost(tx, team, tree, budget.MaxCostUSD)
}

// checkCost refuses a new job of tree once the turns of tree have cost max or
// more. 0 is off.
func (q *Queue) checkCost(tx *sql.Tx, team, tree string, max float64) error {
	if max <= 0 {
		return nil
	}
	if q.cost == nil {
		return fmt.Errorf("queue: no cost reader for the cost cap")
	}
	rows, err := tx.Query(`SELECT turn FROM jobs WHERE team = ? AND tree = ? AND turn IS NOT NULL`, team, tree)
	if err != nil {
		return fmt.Errorf("queue: list tree turns: %w", err)
	}
	var turns []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return fmt.Errorf("queue: list tree turns: %w", err)
		}
		turns = append(turns, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("queue: list tree turns: %w", err)
	}
	cost, err := q.cost(turns)
	if err != nil {
		return err
	}
	if cost >= max {
		return &LimitError{fmt.Sprintf("work started from one session may cost at most $%.2f in all, and it has cost $%.2f (Cost per request tree in Settings, Limits)", max, cost)}
	}
	return nil
}

// countAsked refuses one more asked job by c.By when the per-turn or per-tree
// cap is reached. Asked jobs are the turns and commands a turn asks for, and
// the turns its ons deliver, which deliverResult makes as asked by the on's
// maker. Until 0.6.8 registering an on counted instead (found 2026-11-25).
func countAsked(tx *sql.Tx, team string, c Chain, lim Limits) error {
	// Who asked is stored once: a job's chain names the asking turn (by). The
	// tree is each job's own record (jobs.tree). Registering an on is not
	// counted (0.6.8, framework TODO 2026-11-27); each turn it delivers is,
	// as a turn its maker asked for (deliverResult, his rule 2026-11-28).
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ?1 AND json_extract(links, '$.chain.by') = ?2`, team, c.By).Scan(&n); err != nil {
		return fmt.Errorf("queue: count asked: %w", err)
	}
	if n >= lim.MaxPerTurn {
		return &LimitError{fmt.Sprintf("one turn may ask for at most %s, and this turn has asked for %d (Turns per request in Settings, Limits)", turnsOrReviews(lim.MaxPerTurn), n)}
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ?1 AND tree = ?2 AND json_extract(links, '$.chain.by') IS NOT NULL`, team, c.Root).Scan(&n); err != nil {
		return fmt.Errorf("queue: count asked: %w", err)
	}
	if n >= lim.MaxPerTree {
		return &LimitError{fmt.Sprintf("work started from one session may ask for at most %s in all, and %d have been asked for (Turns per request tree in Settings, Limits)", turnsOrReviews(lim.MaxPerTree), n)}
	}
	return nil
}

// limitsColumn is budget as an on keeps it: NULL for none.
func limitsColumn(budget *Limits) any {
	if budget == nil {
		return nil
	}
	b, _ := json.Marshal(budget)
	return string(b)
}

// limitsOf reads an on's limits column back.
func limitsOf(col sql.NullString) (*Limits, error) {
	if !col.Valid || col.String == "" {
		return nil, nil
	}
	var l Limits
	if err := json.Unmarshal([]byte(col.String), &l); err != nil {
		return nil, fmt.Errorf("queue: read on limits: %w", err)
	}
	return &l, nil
}
