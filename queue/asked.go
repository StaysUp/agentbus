package queue

// Work an agent asks for. A running turn may start turns and reviews through
// the API with its own token. Each such job names the request tree it belongs
// to and the turn that asked for it (Links.Chain: Root, Depth, By, BySession).
// When it ends, an on row registered for it (on.go) starts a new turn in the
// asking session whose prompt is the result: a turn continuing a turn
// ("mail"). The asking turn never waits.
//
// The caps, [limits] in permissions.toml, keep a chain of agents asking
// agents from running away. They are checked in one place, where any job is
// made (budget.go, checkBudget). An on a turn token registers through POST
// /api/v0/ons counts under the per-turn and per-tree caps too (countAsked).

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/workspace"
)

// Chain is where a job sits in a request tree. Root is the session the
// person's own turn ran in. Depth is how many agents asked in a row: work the
// person's turn asks for has depth 1, work that work asks for depth 2. A
// delivered result has the depth of the work it carries. By and BySession are
// the turn and session that asked for the job: the provenance the per-turn cap
// (countAsked) keys on and the record's "asked" origin (runner.go, turnview.go)
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

// turnsOrReviews is n turns or reviews, in words: what an agent may ask
// for. A job is the queue's word, not the API's.
func turnsOrReviews(n int) string {
	if n == 1 {
		return "1 turn or review"
	}
	return fmt.Sprintf("%d turns or reviews", n)
}

// CreateTurnLinked makes a turn job, with links and the caps held to lim,
// checked in the same transaction. lim binds asked work only (links name the
// asking turn, Chain.By): the person's own turn is held to no cap. A turn of a session whose turn runs waits
// behind it in the queue. B15 (his call (a), 2026-11-19): if deliver is set, it
// records the delivery on in that same transaction, so starting the turn and
// arranging its reply are one atomic write under one authorization. A nil
// deliver registers no on; POST /api/v0/ons registers one separately.
func (q *Queue) CreateTurnLinked(team, key string, spec Spec, links Links, lim Limits, deliver *Delivery) (Job, bool, error) {
	var budget *Limits
	if links.Chain != nil && links.Chain.By != "" {
		budget = &lim
	}
	return q.create(team, KindTurn, key, spec, links, budget, string(spec.Session), deliver)
}

// CreateCommand makes an exec job: a command an agent's turn handed to
// AgentBus (POST /api/v0/commands), in the asking turn's workspace (spec),
// asked by the turn links names, held to lim as work that turn asked for, with
// deliver's on (command.ended) recorded in the same write. Its session column
// stays empty: it belongs to the workspace's lane, not to a session's turns.
func (q *Queue) CreateCommand(team, key string, spec Spec, links Links, lim Limits, deliver *Delivery) (Job, bool, error) {
	if spec.Command == "" || spec.Workspace == "" || links.Chain == nil || links.Chain.By == "" {
		return Job{}, false, ErrBadState
	}
	return q.create(team, KindExec, key, spec, links, &lim, "", deliver)
}

// SessionRoot is the root of the request tree a session was started in: the
// Chain.Root of its first turn job. "" for a session the person started, or
// one the queue does not know.
func (q *Queue) SessionRoot(team, session string) (string, error) {
	root, _, err := q.SessionChain(team, session)
	return root, err
}

// SessionChain is the request tree a session's own turns belong to: the root
// and depth of its FIRST turn job, or ("", 0) when that job has no chain (a
// session the person started, which has no tree) or the session has no job. A
// delivered result the main token asked for joins the RECEIVING session's tree
// with these; one a turn asked for joins the asking turn's tree instead
// (deliverResult).
//
// Only the first job decides. A later turn in the session may carry another
// tree's chain, such as a delivery another tree's turn sent into it; that tree
// continued the session, it did not start it. Reading the first job with a
// chain took that tree for a session the person started, so an owner's
// delivery into it joined a tree it was never in (O2.3 review P2). Ties of
// created, at the clock's resolution, go to the job stored first (rowid), as
// firstSpec reads a workspace's first turn.
func (q *Queue) SessionChain(team, session string) (root string, depth int, err error) {
	var r sql.NullString
	var d sql.NullInt64
	e := q.db.QueryRow(`SELECT json_extract(links, '$.chain.root'), json_extract(links, '$.chain.depth')
		FROM jobs WHERE team = ? AND kind = 'turn' AND session = ?
		ORDER BY created, rowid LIMIT 1`, team, session).Scan(&r, &d)
	if errors.Is(e, sql.ErrNoRows) {
		return "", 0, nil
	}
	if e != nil {
		return "", 0, fmt.Errorf("queue: read session chain: %w", e)
	}
	return r.String, int(d.Int64), nil
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
// that turn was asked. budget is the caps the on recorded from the turn that
// made it (nil for the main token's): the delivered turn is made under them,
// so a tree over its cost cap gets no more turns this way either.
func (q *Queue) deliverResult(team, onID, toSession, askingTurn string, j Job, to func(session string) (DeliverTo, bool, error), name func(id string) string, agent AgentOf, budget *Limits) (string, string, error) {
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
		dt = DeliverTo{Workspace: string(first.Workspace), FolderRef: first.FolderRef, Folders: first.Folders, Machine: first.Machine, Isolate: first.Isolate}
		if dt.Workspace == "" {
			// The delivery session's first turn named no workspace, so the session
			// is its own workspace: take that from the resolver (workspaceID), never
			// a raw cast of the session id.
			dt.Workspace = string(wsID)
		}
	}
	spec := Spec{Session: ids.SessionID(toSession), Workspace: ids.WorkspaceID(dt.Workspace), FolderRef: dt.FolderRef, Folders: dt.Folders, Machine: dt.Machine,
		Isolate: dt.Isolate, Prompt: ResultPrompt(j, c, reply, name)}
	// The reply runs on the asking turn's agent. An on registered by the main
	// token names no asking turn (askingTurn ""); then it runs on the agent
	// that did the work (the subject's own), so the delivery still has one.
	from := j.Spec
	// cause is the request tree that asked for this delivery: the asking turn's,
	// read from its job as its token reads it (jobs/tokens.go): its chain's root
	// and depth, or, for the person's own turn, its session at depth 0.
	var cause *Chain
	if askingTurn != "" {
		asking, err := q.JobByTurn(team, askingTurn)
		switch {
		case errors.Is(err, ErrNotFound):
			return "", "the asking turn is not in the queue, so its agent is not known", nil
		case err != nil:
			return "", "", err
		}
		from = asking.Spec
		cause = &Chain{Root: asking.Session}
		if c := asking.Links.Chain; c != nil {
			cause = &Chain{Root: c.Root, Depth: c.Depth}
		}
	} else if c := j.Links.Chain; j.Kind == KindExec && c != nil && c.By != "" {
		// A command runs no agent of its own, so an on the main token made over
		// it delivers on the agent of the turn that asked for the command
		// (review P2, 2026-11-27). Only the agent: its tree stays the receiving
		// session's, as for any main-token on.
		asking, err := q.JobByTurn(team, c.By)
		switch {
		case errors.Is(err, ErrNotFound):
			return "", "the turn that asked for the command is not in the queue, so its agent is not known", nil
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
	// Work a request tree causes stays in that tree, whatever session it lands
	// in (his rule, 2026-11-25). A delivery a turn asked for (its on, or the
	// delivery recorded with its ask) takes the ASKING turn's root and depth,
	// never the receiving session's: an empty session has no tree, so taking
	// its tree gave the delivery a new root at depth zero with fresh caps, and
	// a turn could repeat that to pass the depth, job and cost limits (review
	// P1, 2026-11-25). Because its Chain.Root is the asker's tree, it is
	// recorded in that tree (treeOf), its cost counts toward that tree's cost
	// cap, and work IT asks for counts against the tree. It is not itself asked
	// work (By is empty), so the job caps do not count it; the on that asked for
	// it is counted instead (countAsked), and the ask_agent on is covered by its
	// job. The cost cap is checked for it (checkBudget). Being in a tree widens
	// no reach: reach is by workspace (inAuthority), never by tree.
	// An on the main token registered has no asking turn and no tree that
	// caused it, so the delivery joins the receiving session's own tree, or
	// none for a session the person started: a main-token delivery never pulls
	// an unrelated session into the subject's tree.
	// A reply to the person's own session, from its own turn, is that session's
	// root-level turn, and so carries no chain, as the person's turns do.
	if cause != nil {
		if cause.Root != toSession || cause.Depth != 0 {
			links.Chain = cause
		}
	} else if root, depth, err := q.SessionChain(team, toSession); err != nil {
		return "", "", err
	} else if root != "" {
		links.Chain = &Chain{Root: root, Depth: depth}
	}
	d, _, err := q.create(team, KindTurn, ResultKey(onID, j.ID, toSession), spec, links, budget, string(spec.Session), nil)
	var removed *RunnerRemovedError
	var limit *LimitError
	switch {
	case errors.As(err, &limit):
		return "", limit.Msg, nil
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
	if j.Kind == KindExec {
		return CommandPrompt(j)
	}
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

// CommandPrompt is the prompt of the turn that delivers exec job j's result:
// the command, how it ended, its exit code and the end of its output. It says
// that AgentBus wrote it, not the person.
func CommandPrompt(j Job) string {
	r, _ := ReadResult(j.Result)
	var b strings.Builder
	b.WriteString("[AgentBus] This message is from AgentBus, not from the person: the command you asked AgentBus to run has ended, and this is its result.\n\n")
	fmt.Fprintf(&b, "Command: %s\n", short(j.Spec.Command))
	fmt.Fprintf(&b, "Id: %s (GET /api/v0/commands/%s)\n", j.Turn, j.Turn)
	state := TurnState(j.State)
	switch {
	case r.TimedOut:
		state += " (it ran past its time limit and was stopped)"
	case j.Reason != "":
		state += " (" + j.Reason + ")"
	}
	fmt.Fprintf(&b, "State: %s\n", state)
	if r.ExitCode != nil {
		fmt.Fprintf(&b, "Exit code: %d\n", *r.ExitCode)
	}
	if r.DurationMS > 0 {
		fmt.Fprintf(&b, "Ran for: %s\n", (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second))
	}
	if msg := firstOf(r.Refused, r.Error); msg != "" {
		fmt.Fprintf(&b, "Error: %s\n", short(msg))
	}
	if r.Output != "" {
		if r.OutputBytes > int64(len(r.Output)) {
			fmt.Fprintf(&b, "\nThe end of its output (the last %d of %d bytes):\n\n", len(r.Output), r.OutputBytes)
		} else {
			b.WriteString("\nIts output:\n\n")
		}
		b.WriteString(quoteBlock(strings.ToValidUTF8(r.Output, "")))
	} else if r.ExitCode != nil {
		b.WriteString("\nIt wrote no output.\n")
	}
	return strings.ReplaceAll(b.String(), "\x00", "")
}
