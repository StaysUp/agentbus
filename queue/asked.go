package queue

// Work an agent asks for. A running turn may start turns and reviews through
// the API with its own token. Each such job names the request tree it belongs
// to and the turn that asked for it (Links.Chain: Root, Depth, By, BySession).
// When it ends, an on row registered for it (on.go) starts a new turn in the
// asking session whose prompt is the result: a turn continuing a turn
// ("mail"). The asking turn never waits.
//
// The caps, [limits] in permissions.toml, keep a chain of agents asking
// agents from running away.

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/workspace"
)

// Chain is where a job sits in a request tree. Root is the session the
// person's own turn ran in. Depth is how many agents asked in a row: work the
// person's turn asks for has depth 1, work that work asks for depth 2. A
// delivered result has the depth of the work it carries. By and BySession are
// the turn and session that asked for the job: the provenance the per-turn cap
// (checkAsked) keys on and the record's "asked" origin (runner.go, turnview.go)
// reads, both from the asking turn's token. They are empty on a delivered
// result and on the person's own work. They moved here off the retired Asker
// struct so a turn's origin survives once delivery moves to an on row (on.go).
type Chain struct {
	Root      string `json:"root"`
	Depth     int    `json:"depth"`
	By        string `json:"by,omitempty"`
	BySession string `json:"by_session,omitempty"`
}

// ResultOf marks a turn job that carries the result of asked work: a turn
// AgentBus started, not the person.
type ResultOf struct {
	Job     string `json:"job"`
	Kind    string `json:"kind"`              // turn or review
	Session string `json:"session,omitempty"` // the session the work ran in
	Turn    string `json:"turn,omitempty"`    // the work's turn
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
	if links.Chain == nil || links.Chain.By == "" {
		return nil // not asked work: the person's own turn, or a delivered result
	}
	if links.Chain.Depth < 1 {
		return ErrBadState
	}
	if links.Chain.Depth > lim.MaxDepth {
		return &LimitError{fmt.Sprintf("work may be asked for at most %d levels below the person's turn, and this would be level %d (Depth in Settings, Limits)", lim.MaxDepth, links.Chain.Depth)}
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND json_extract(links, '$.chain.by') = ?
		AND json_extract(links, '$.chain.by_session') = ?`, team, links.Chain.By, links.Chain.BySession).Scan(&n); err != nil {
		return fmt.Errorf("queue: count asked: %w", err)
	}
	if n >= lim.MaxPerTurn {
		return &LimitError{fmt.Sprintf("one turn may ask for at most %s, and this turn has asked for %d (Turns per request in Settings, Limits)", turnsOrReviews(lim.MaxPerTurn), n)}
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE team = ? AND json_extract(links, '$.chain.by') IS NOT NULL
		AND json_extract(links, '$.chain.root') = ?`, team, links.Chain.Root).Scan(&n); err != nil {
		return fmt.Errorf("queue: count asked: %w", err)
	}
	if n >= lim.MaxPerTree {
		return &LimitError{fmt.Sprintf("work started from one session may ask for at most %s in all, and %d have been asked for (Turns per request tree in Settings, Limits)", turnsOrReviews(lim.MaxPerTree), n)}
	}
	return nil
}

// turnsOrReviews is n turns or reviews, in words: what an agent may ask
// for. A job is the queue's word, not the API's.
func turnsOrReviews(n int) string {
	if n == 1 {
		return "1 turn or review"
	}
	return fmt.Sprintf("%d turns or reviews", n)
}

// CreateTurnLinked makes a turn job, with links and the caps held to lim,
// checked in the same transaction. A turn of a session whose turn runs waits
// behind it in the queue. B15 (his call (a), 2026-11-19): if deliver is set, it
// records the delivery on in that same transaction, so starting the turn and
// arranging its reply are one atomic write under one authorization. A nil
// deliver registers no on; POST /api/v0/ons registers one separately.
func (q *Queue) CreateTurnLinked(team, key string, spec Spec, links Links, lim Limits, deliver *Delivery) (Job, bool, error) {
	return q.create(team, KindTurn, key, spec, links, lim, string(spec.Session), deliver)
}

// SessionRoot is the root of the request tree a session was started in: the
// Chain.Root of its first turn job that has one, or of the asked review that
// runs in it: a review runs in a session of its own, which control names when
// it makes the job (Links.ReviewSession).
// "" for a session the person started, or one the queue does not know.
func (q *Queue) SessionRoot(team, session string) (string, error) {
	root, _, err := q.SessionChain(team, session)
	return root, err
}

// SessionChain is the request tree a session's own turns belong to: the root and
// depth of its first rooted turn job, or ("", 0) for a session the person
// started (which has no tree). A delivered result joins the RECEIVING session's
// tree with these, so it counts against that tree's caps like any other turn
// (his rule, 2026-11-19: every turn is a turn; a delivered answer is no
// exception). B15 dropped the review kind, so only turn jobs carry a chain here.
func (q *Queue) SessionChain(team, session string) (root string, depth int, err error) {
	var r sql.NullString
	var d sql.NullInt64
	e := q.db.QueryRow(`SELECT json_extract(links, '$.chain.root'), json_extract(links, '$.chain.depth')
		FROM jobs WHERE team = ? AND kind = 'turn' AND session = ?
		AND json_extract(links, '$.chain.root') IS NOT NULL ORDER BY created, id LIMIT 1`, team, session).Scan(&r, &d)
	if errors.Is(e, sql.ErrNoRows) {
		return "", 0, nil
	}
	if e != nil {
		return "", 0, fmt.Errorf("queue: read session chain: %w", e)
	}
	return r.String, int(d.Int64), nil
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

// SessionFolder is the folder the session's first turn named, or its repo
// for a worktree (repo true): its reference and its machine, or "" when
// it named none, or a path, as a job stored before references did.
func (q *Queue) SessionFolder(team, session string) (ref, machine string, repo bool, err error) {
	ws, err := q.workspaceID(team, ids.SessionID(session))
	if err != nil {
		return "", "", false, err
	}
	sp, err := q.firstSpec(team, ws)
	if err != nil {
		return "", "", false, err
	}
	if sp.FolderRef == "" && sp.RepoRef != "" {
		return sp.RepoRef, sp.Machine, true, nil
	}
	return sp.FolderRef, sp.Machine, false, nil
}

// B15: IsReview and ReviewLinked retired with the review kind. A review is a
// normal turn (the review_turn app), so there is no "is this a review session"
// query and no review-specific create path.

// deliverResult builds and queues the turn that carries job j's result to
// toSession, on the agent the asking turn ran (resolved now from askingTurn,
// or by agent), after any turn running in toSession. It says the queued
// turn's id, or why none could be made (a plain reason, not an error) — and
// returns ErrFull, and no id, when the queue is full so the caller can try
// again later. It writes no dedupe row: the caller (FireOns, on.go) records
// the fire in on_fires so a result is carried exactly once. The turn runs on
// the agent of the asking turn, as agent reads it now: every turn names its
// agent, and control names this one. name gives a saved agent's name for the
// prompt; nil names it by id. agent nil runs it on the asking turn's agent as
// that turn was asked.
func (q *Queue) deliverResult(team, onID, toSession, askingTurn string, j Job, to func(session string) (DeliverTo, bool, error), name func(id string) string, agent AgentOf) (string, string, error) {
	c, err := q.changesOf(j)
	if err != nil {
		return "", "", err
	}
	reply, err := q.replyOf(j)
	if err != nil {
		return "", "", err
	}
	// The delivery is a turn in toSession, so it runs in toSession's workspace,
	// with the same folder, machine and mode as any of its turns -- resolved
	// from the record (to), or, for a queue-only caller or an unknown session,
	// from the session's first turn job. The workspace spec lets the runner
	// find or make the one copy the workspace shares, rather than running in the
	// folder as it is with no changeset.
	var dt DeliverTo
	if to != nil {
		ok, terr := false, error(nil)
		if dt, ok, terr = to(toSession); terr != nil {
			return "", "", terr
		} else if !ok {
			dt = DeliverTo{}
		}
	}
	if dt.Workspace == "" {
		wsID, werr := q.workspaceID(team, ids.SessionID(toSession))
		if werr != nil {
			return "", "", werr
		}
		first, ferr := q.firstSpec(team, wsID)
		if ferr != nil {
			return "", "", ferr
		}
		dt = DeliverTo{Workspace: string(first.Workspace), FolderRef: first.FolderRef, Machine: first.Machine, Isolate: first.Isolate}
		if dt.Workspace == "" {
			// The delivery session's first turn named no workspace, so the session
			// is its own workspace: take that from the resolver (workspaceID), never
			// a raw cast of the session id.
			dt.Workspace = string(wsID)
		}
	}
	spec := Spec{Session: ids.SessionID(toSession), Workspace: ids.WorkspaceID(dt.Workspace), FolderRef: dt.FolderRef, Machine: dt.Machine,
		Isolate: dt.Isolate, Prompt: ResultPrompt(j, c, reply, name)}
	// The reply runs on the asking turn's agent. An on registered by the main
	// token names no asking turn (askingTurn ""); then it runs on the agent
	// that did the work (the subject's own), so the delivery still has one.
	from := j.Spec
	if askingTurn != "" {
		asking, err := q.JobByTurn(team, askingTurn)
		switch {
		case errors.Is(err, ErrNotFound):
			return "", "the asking turn is not in the queue, so its agent is not known", nil
		case err != nil:
			return "", "", err
		}
		from = asking.Spec
	}
	if agent != nil {
		as, why := agent(from)
		if why != "" {
			return "", why, nil
		}
		from = as
	}
	spec.Harness, spec.AgentID, spec.Access, spec.KeyEnv, spec.AgentModel = from.Harness, from.AgentID, from.Access, from.KeyEnv, from.AgentModel
	res := &ResultOf{Job: j.ID, Kind: j.Kind, Session: resultSession(j), Turn: j.Turn, Agent: agentName(j.Spec)}
	links := Links{ResultOf: res}
	// The delivery is a turn in toSession, so it joins toSession's OWN request
	// tree — its root and depth, never the subject's and never none (his rule,
	// 2026-11-19: a delivered answer is a turn like any other of the receiving
	// tree). Because its Chain.Root is that tree, its cost counts toward the
	// tree's cost limit (TreeSessions reads it) and work IT asks for counts
	// against the tree; it is not itself asked work, though, so the asked-work
	// caps (checkAsked, keyed on Chain.By) skip it — By is empty on a delivery.
	// The tree choice is right in every case: the ordinary ask (toSession is the
	// asking session) carries that session's own root and depth; a main-token
	// delivery to an unrelated session the person started carries no tree, so it
	// cannot be pulled into the subject's tree; a delivery to a session already
	// in another tree keeps THAT tree (found across three review rounds).
	if root, depth, err := q.SessionChain(team, toSession); err != nil {
		return "", "", err
	} else if root != "" {
		links.Chain = &Chain{Root: root, Depth: depth}
	}
	d, _, err := q.create(team, KindTurn, ResultKey(onID, j.ID, toSession), spec, links, Limits{}, string(spec.Session), nil)
	var removed *RunnerRemovedError
	switch {
	case errors.As(err, &removed):
		return "", "the asking session's machine " + strconv.Quote(removed.Machine) + " was removed", nil
	case err == nil:
		return d.ID, "", nil
	case errors.Is(err, ErrFull):
		return "", "", ErrFull
	case errors.Is(err, ErrSessionClosed):
		return "", "the asking session is closed", nil
	case errors.Is(err, ErrSessionEnded):
		return "", "the asking session ended with its first turn", nil
	case errors.Is(err, ErrKeyConflict):
		return "", "a delivery of this result already exists with other fields", nil
	default:
		return "", "", err
	}
}

// AgentOf is the agent a delivered result's turn runs on, read now from the
// asking turn's spec (its saved agent, or its harness), or why it cannot
// run, such as "agent X was removed, pick another".
type AgentOf func(asking Spec) (Spec, string)

// quoteBlock puts text in a fenced block (moved here from review.go, B15).
func quoteBlock(s string) string { return fenced(s, "") }

// fenced puts s in a fenced block whose fence is longer than any run of
// backticks in s, so no line of s can close it and read as the prompt's own
// text. info names the block's language.
func fenced(s, info string) string {
	run, longest := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return f + info + "\n" + s + f + "\n"
}

// ResultKey is the idempotency key of the turn job that delivers job id's
// result to session for on. It is scoped to the ON (his call, 2026-11-19):
// several ons may watch one turn, each a distinct instruction that delivers its
// own turn, so a key on (source, session) alone would make two distinct ons
// share one delivery and the second silently do nothing. Scoping to the on also
// keeps delivery to different sessions distinct. It holds a "/", which no key a
// caller sends may hold, so no request can take it first. One on's own retry
// keeps the same (on, id, session) and so delivers once; crash-safety across
// ons is on_fires, keyed per on.
func ResultKey(on, id, session string) string { return "result/" + on + "/" + id + "/" + session }

// hashLinks is links as a job's request hash sees them: for asked work, its
// request tree only. A retry with the same key from a later turn of the same
// tree then gets the job the key first made; one from another tree gets
// ErrKeyConflict.
func hashLinks(l Links) Links {
	l.ApplyBy = nil // who asked, not what
	if l.Chain != nil && l.Chain.By != "" {
		l.Chain = &Chain{Root: l.Chain.Root}
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
// did the work, how it ended, where, what it changed (c), and its final reply,
// or a review's verdict and findings. It says that AgentBus wrote it, not the
// person. name gives a saved agent's name; nil names it by id.
func ResultPrompt(j Job, c *workspace.Changes, reply string, name func(id string) string) string {
	r, _ := ReadResult(j.Result)
	var b strings.Builder
	// B15: a review is a normal turn (the review_turn app), so a delivered
	// result is always "the turn you asked for"; its verdict, if any, is the
	// reviewer's reply, not a field core parses.
	fmt.Fprintf(&b, "[AgentBus] This message is from AgentBus, not from the person: the turn you asked for has ended, and this is its result.\n\n")
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
	state := TurnState(j.State)
	if j.Reason != "" {
		state += " (" + j.Reason + ")"
	}
	fmt.Fprintf(&b, "Turn: %s, %s\n", j.Turn, state)
	if s := resultSession(j); s != "" {
		fmt.Fprintf(&b, "Session: %s\n", s)
	}
	if r.Folder != "" {
		fmt.Fprintf(&b, "Folder: %s\n", short(r.Folder))
	}
	if msg := firstOf(r.Refused, r.Error); msg != "" {
		fmt.Fprintf(&b, "Error: %s\n", short(msg))
	}
	if c != nil {
		fmt.Fprintf(&b, "Changed files: %d\n", c.Count)
		for i, f := range c.Files {
			if i == MaxResultFiles {
				b.WriteString("- (more; the list stops here)\n")
				break
			}
			fmt.Fprintf(&b, "- %s (%s)\n", short(f.Path), short(f.Change))
		}
		if c.Truncated && len(c.Files) <= MaxResultFiles {
			b.WriteString("- (more; the list stops here)\n")
		}
	}
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
		out = strings.ToValidUTF8(out[:MaxResultPrompt], "") + "\n(cut here; the whole result is in its turn)\n"
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

// firstRunes is the first n characters of s.
func firstRunes(s string, n int) string {
	c := 0
	for i := range s {
		if c == n {
			return s[:i]
		}
		c++
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
