package queue

// Work an agent asks for. A running turn may start turns and reviews through
// the API with its own token. Each such job names the turn that asked for it
// (Links.Asker) and the request tree it belongs to (Links.Chain). When it
// ends, Deliver starts a new turn in the asking session whose prompt is the
// result: a turn continuing a turn ("mail"). The asking turn never waits.
//
// The caps, [limits] in permissions.toml, keep a chain of agents asking
// agents from running away.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Asker is the turn whose agent asked for a job, with its turn token.
type Asker struct {
	Session string `json:"session"`
	Turn    string `json:"turn"`
}

// Chain is where a job sits in a request tree. Root is the session the
// person's own turn ran in. Depth is how many agents asked in a row: work the
// person's turn asks for has depth 1, work that work asks for depth 2. A
// delivered result has the depth of the work it carries.
type Chain struct {
	Root  string `json:"root"`
	Depth int    `json:"depth"`
}

// ResultOf marks a turn job that carries the result of asked work: a turn
// AgentBus started, not the person.
type ResultOf struct {
	Job     string `json:"job"`
	Kind    string `json:"kind"`              // turn or review
	Session string `json:"session,omitempty"` // the session the work ran in
	Turn    string `json:"turn,omitempty"`    // the work's turn: its job's last attempt
	Agent   string `json:"agent,omitempty"`   // the saved agent, or harness, that did it
}

// Limits are the caps on asked work: [limits] in permissions.toml, read for
// each request by the API, which passes them here. A negative value is never
// sent; 0 refuses all asked work of that kind.
type Limits struct {
	// MaxDepth: work may be asked for at most this many levels below the
	// person's turn.
	MaxDepth int
	// MaxPerTurn: jobs one turn may ask for.
	MaxPerTurn int
	// MaxPerTree: jobs a whole request tree may ask for, over every level:
	// everything that started from one person's session.
	MaxPerTree int
}

// LimitError is a cap on asked work that was reached. Its text says which,
// in fixed wording.
type LimitError struct{ Msg string }

func (e *LimitError) Error() string { return e.Msg }

// checkAsked refuses a new asked job that would pass a cap. It runs in the
// job's own transaction, so requests at once cannot all pass.
func checkAsked(tx *sql.Tx, team string, links Links, lim Limits) error {
	if links.Asker == nil {
		return nil
	}
	if links.Chain == nil || links.Chain.Depth < 1 {
		return ErrBadState
	}
	if links.Chain.Depth > lim.MaxDepth {
		return &LimitError{fmt.Sprintf("work may be asked for at most %d levels below the person's turn, and this would be level %d (max_depth in [limits] of permissions.toml)", lim.MaxDepth, links.Chain.Depth)}
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND json_extract(links, '$.asker.session') = ?
		AND json_extract(links, '$.asker.turn') = ?`, team, links.Asker.Session, links.Asker.Turn).Scan(&n); err != nil {
		return fmt.Errorf("queue: count asked: %w", err)
	}
	if n >= lim.MaxPerTurn {
		return &LimitError{fmt.Sprintf("one turn may ask for at most %d jobs, and this turn has asked for %d (max_jobs_per_turn in [limits] of permissions.toml)", lim.MaxPerTurn, n)}
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND json_extract(links, '$.asker.session') IS NOT NULL
		AND json_extract(links, '$.chain.root') = ?`, team, links.Chain.Root).Scan(&n); err != nil {
		return fmt.Errorf("queue: count asked: %w", err)
	}
	if n >= lim.MaxPerTree {
		return &LimitError{fmt.Sprintf("work started from one session may ask for at most %d jobs in all, and %d have been asked for (max_jobs_per_tree in [limits] of permissions.toml)", lim.MaxPerTree, n)}
	}
	return nil
}

// CreateTurnLinked makes a turn job, with links: for a turn an agent asked
// for, its Asker and Chain, held to lim. The caps are checked in the same
// transaction. A turn of a session whose turn runs waits behind it in the
// queue.
func (q *Queue) CreateTurnLinked(team, key string, spec Spec, links Links, lim Limits) (Job, bool, error) {
	return q.create(team, KindTurn, key, spec, links, lim, spec.Session)
}

// SessionRoot is the root of the request tree a session was started in: the
// Chain.Root of its first turn job that has one, or of the asked review that
// runs in it: a review runs in a session of its own, which its runner names
// in the attempt's turn.created event as soon as the review's turn starts.
// "" for a session the person started, or one the queue does not know.
func (q *Queue) SessionRoot(team, session string) (string, error) {
	var root sql.NullString
	err := q.db.QueryRow(`SELECT json_extract(links, '$.chain.root') FROM jobs j WHERE team = ?
		AND ((kind = 'turn' AND session = ?) OR (kind = 'review' AND EXISTS (SELECT 1 FROM attempts a
			JOIN attempt_events e ON e.attempt = a.id WHERE a.job = j.id AND e.kind = 'turn.created'
			AND json_extract(e.data, '$.session') = ?)))
		AND json_extract(links, '$.chain.root') IS NOT NULL ORDER BY created, id LIMIT 1`, team, session, session).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("queue: read session root: %w", err)
	}
	return root.String, nil
}

// TreeSessions are the sessions work in root's request tree ran in: each
// asked turn's session, and each asked review's own session once its turn
// has started. root itself is not among them unless work ran there.
func (q *Queue) TreeSessions(team, root string) ([]string, error) {
	rows, err := q.db.Query(`SELECT DISTINCT session FROM jobs WHERE team = ? AND kind = 'turn' AND session IS NOT NULL
		AND json_extract(links, '$.chain.root') = ?
		UNION SELECT DISTINCT json_extract(e.data, '$.session') FROM jobs j JOIN attempts a ON a.job = j.id
		JOIN attempt_events e ON e.attempt = a.id WHERE j.team = ? AND j.kind = 'review' AND e.kind = 'turn.created'
		AND json_extract(j.links, '$.chain.root') = ? AND json_extract(e.data, '$.session') IS NOT NULL`, team, root, team, root)
	if err != nil {
		return nil, fmt.Errorf("queue: list tree sessions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("queue: list tree sessions: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SessionFolders are the folders a session was started in: asked, the
// folder its first turn job named, and ws, its workspace's folder (a
// worktree inside asked, or asked itself). Either may be "".
func (q *Queue) SessionFolders(team, session string) (asked, ws string, err error) {
	var spec string
	err = q.db.QueryRow(`SELECT spec FROM jobs WHERE team = ? AND session = ? AND kind = 'turn' ORDER BY created, id LIMIT 1`,
		team, session).Scan(&spec)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("queue: read session: %w", err)
	}
	var sp Spec
	_ = json.Unmarshal([]byte(spec), &sp)
	err = q.db.QueryRow(`SELECT folder FROM sessions WHERE id = ? AND team = ?`, session, team).Scan(&ws)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("queue: read session: %w", err)
	}
	return sp.Folder, ws, nil
}

// ReviewLinked is ReviewAs with links: for a review an agent asked for, its
// Asker and Chain. The caps are checked in the same transaction.
func (q *Queue) ReviewLinked(team, id string, as Spec, key string, includeReply bool, links Links, lim Limits) (Job, bool, error) {
	return q.review(team, id, as, key, includeReply, links, lim)
}

// Deliver starts the next turn of each asking session whose asked work has
// ended, with the result as its prompt, and says how many it started. Each
// job's result is delivered once: the delivery's idempotency key is the job's
// id, and the deliveries table records it, or why it could not be. The turn
// runs on the asking session's own agent, after the turn running there now.
// name gives a saved agent's name for the prompt; nil names it by id.
func (q *Queue) Deliver(team string, name func(id string) string) (int, error) {
	rows, err := q.db.Query(`SELECT id FROM jobs j WHERE team = ? AND ended IS NOT NULL
		AND json_extract(links, '$.asker.session') IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.job = j.id) ORDER BY ended, id LIMIT 20`, team)
	if err != nil {
		return 0, fmt.Errorf("queue: list results: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("queue: list results: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		j, err := q.Get(team, id)
		if err != nil {
			return n, err
		}
		spec := Spec{Session: j.Links.Asker.Session, Prompt: ResultPrompt(j, name)}
		res := &ResultOf{Job: j.ID, Kind: j.Kind, Session: resultSession(j), Turn: j.Attempt, Agent: agentName(j.Spec)}
		links := Links{ResultOf: res}
		if j.Links.Chain != nil {
			links.Chain = &Chain{Root: j.Links.Chain.Root, Depth: j.Links.Chain.Depth}
		}
		d, _, err := q.create(team, KindTurn, ResultKey(j.ID), spec, links, Limits{}, spec.Session)
		why := ""
		switch {
		case err == nil:
			n++
		case errors.Is(err, ErrFull):
			return n, nil // the queue is full: try again later
		case errors.Is(err, ErrSessionClosed):
			why = "the asking session is closed"
		case errors.Is(err, ErrKeyConflict):
			why = "a delivery of this result already exists with other fields"
		default:
			return n, err
		}
		if _, err := q.db.Exec(`INSERT INTO deliveries(job, delivery, error, created) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`,
			j.ID, d.ID, why, nanos(q.now())); err != nil {
			return n, fmt.Errorf("queue: write delivery: %w", err)
		}
	}
	return n, nil
}

// ResultKey is the idempotency key of the turn job that delivers job id's
// result. It holds a "/", which no key a caller sends may hold, so no request
// can take it first.
func ResultKey(id string) string { return "result/" + id }

// hashLinks is links as a job's request hash sees them: for asked work, its
// request tree only. A retry with the same key from a later turn of the same
// tree then gets the job the key first made; one from another tree gets
// ErrKeyConflict.
func hashLinks(l Links) Links {
	if l.Asker != nil {
		l.Asker = nil
		if l.Chain != nil {
			l.Chain = &Chain{Root: l.Chain.Root}
		}
	}
	return l
}

// resultSession is the session asked work ran in: a turn's own, or for a
// review the session of its own turn, from its result.
func resultSession(j Job) string {
	r, _ := ReadResult(j.Result)
	if r.Session != "" {
		return r.Session
	}
	if j.Kind == KindTurn {
		return j.Session
	}
	return ""
}

// agentName is the saved agent a spec names, else its harness.
func agentName(sp Spec) string {
	if sp.AgentID != "" {
		return sp.AgentID
	}
	return sp.Harness
}

// MaxResultReply bounds the reply a delivered result quotes, in bytes.
const MaxResultReply = 8 << 10

// MaxResultFiles bounds the changed files and findings a delivered result
// lists.
const MaxResultFiles = 50

// MaxResultLine bounds one listed line of a delivered result (a finding's
// summary, a path, an error), in bytes.
const MaxResultLine = 500

// MaxResultPrompt bounds a delivered result's whole prompt, well under a
// turn's 256 KiB.
const MaxResultPrompt = 32 << 10

// ResultPrompt is the prompt of the turn that delivers job j's result: who
// did the work, how it ended, where, what it changed, and its final reply,
// or a review's verdict and findings. It says that AgentBus wrote it, not the
// person. name gives a saved agent's name; nil names it by id.
func ResultPrompt(j Job, name func(id string) string) string {
	r, _ := ReadResult(j.Result)
	var b strings.Builder
	what := "the turn you asked for"
	if j.Kind == KindReview {
		what = "the review you asked for"
	}
	fmt.Fprintf(&b, "[AgentBus] This message is from AgentBus, not from the person: %s has ended, and this is its result.\n\n", what)
	id := agentName(j.Spec)
	var about []string
	agent := id
	if name != nil && id != "" {
		if n := name(id); n != "" && n != id {
			agent = n
			about = append(about, "id "+id)
		}
	}
	if agent == "" {
		agent = "the session's own agent"
	}
	if j.Spec.Model != "" {
		about = append(about, "model "+j.Spec.Model)
	}
	fmt.Fprintf(&b, "Agent: %s", short(agent))
	if len(about) > 0 {
		fmt.Fprintf(&b, " (%s)", short(strings.Join(about, ", ")))
	}
	b.WriteString("\n")
	state := j.State
	if j.Reason != "" {
		state += " (" + j.Reason + ")"
	}
	fmt.Fprintf(&b, "Job: %s, %s\n", j.ID, state)
	if s := resultSession(j); s != "" {
		fmt.Fprintf(&b, "Session: %s", s)
		if j.Attempt != "" {
			fmt.Fprintf(&b, ", turn %s", j.Attempt)
		}
		b.WriteString("\n")
	}
	if r.Folder != "" {
		fmt.Fprintf(&b, "Folder: %s\n", short(r.Folder))
	}
	if msg := firstOf(r.Refused, r.Error); msg != "" {
		fmt.Fprintf(&b, "Error: %s\n", short(msg))
	}
	if j.Kind == KindReview {
		v := "none"
		if j.Verdict != nil {
			v = *j.Verdict
		}
		fmt.Fprintf(&b, "Verdict: %s\n", v)
		if len(j.Findings) > 0 {
			b.WriteString("Findings:\n")
			for i, f := range j.Findings {
				if i == MaxResultFiles {
					fmt.Fprintf(&b, "- (%d more)\n", len(j.Findings)-i)
					break
				}
				loc := f.File
				if f.Line > 0 {
					loc = fmt.Sprintf("%s:%d", f.File, f.Line)
				}
				fmt.Fprintf(&b, "- %s %s %s\n", short(f.Severity), short(loc), short(f.Summary))
			}
		}
	} else if r.Changes != nil {
		fmt.Fprintf(&b, "Changed files: %d\n", r.Changes.Count)
		for i, f := range r.Changes.Files {
			if i == MaxResultFiles {
				b.WriteString("- (more; the list stops here)\n")
				break
			}
			fmt.Fprintf(&b, "- %s (%s)\n", short(f.Path), short(f.Change))
		}
		if r.Changes.Truncated && len(r.Changes.Files) <= MaxResultFiles {
			b.WriteString("- (more; the list stops here)\n")
		}
	}
	reply := firstOf(r.ReplyText, r.Reply)
	if reply != "" {
		b.WriteString("\nIts final reply:\n\n")
		if len(reply) > MaxResultReply {
			reply = strings.ToValidUTF8(reply[:MaxResultReply], "") + "\n(cut here; the whole reply is in its session)"
		}
		b.WriteString(quoteBlock(reply))
	}
	// Every part is bounded, and a turn's prompt holds no NUL byte.
	out := strings.ReplaceAll(b.String(), "\x00", "")
	if len(out) > MaxResultPrompt {
		out = strings.ToValidUTF8(out[:MaxResultPrompt], "") + "\n(cut here; the whole result is in its job)\n"
	}
	return out
}

// short is s on one line, at most MaxResultLine bytes.
func short(s string) string {
	s = oneLine(s)
	if len(s) > MaxResultLine {
		s = strings.ToValidUTF8(s[:MaxResultLine], "") + "…"
	}
	return s
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
