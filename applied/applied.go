// Package applied is a workspace's applied state: which of its turns are
// appliable, how far a successful apply has gone, and what the person's folder
// holds. The record is its one owner (his call, 2026-11-24): the store reads a
// workspace's events in order and folds them here (Fold), and both the API and
// the queue read the State that comes out. The queue keeps no copy of it.
//
// It uses nothing of the module, so the queue, whose types the folder side
// uses, can hold a State without reaching the record.
package applied

import (
	"sort"
	"time"
)

// State is a workspace's applied state.
type State struct {
	// Turns are the workspace's appliable turns in the order they ended: every
	// turn that recorded a changeset, other than one a discard dropped or an
	// update folded into its merge.
	Turns []AppliableTurn
	// Mark is the index in Turns of the furthest turn a successful apply went
	// up to; -1 when none of them was applied.
	Mark int
	// AppliedTurn is the turn the latest successful apply went up to, and
	// AppliedTo the changeset it wrote: what the folder holds. Applied is false
	// when no apply succeeded.
	AppliedTurn, AppliedTo string
	Applied                bool
	// Update is the latest clean update when no successful apply or discard
	// came after it, so it still sets the folder's baseline. Nil otherwise.
	Update *UpdateInForce
	// Markers: a merge left conflict markers in the copy after the latest
	// successful apply or discard, so an apply checks the files it writes.
	Markers bool
	// DiscardedAt is when the latest apply-or-discard was a discard; nil when
	// it was an apply (of any result), or there was neither.
	DiscardedAt *time.Time

	changed         map[string]bool   // every turn that recorded a changeset, dropped or folded too
	turnCheckpoint  map[string]string // changeset -> the newest turn's end checkpoint
	mergeCheckpoint map[string]string // changeset -> the newest update's checkpoint of it
}

// AppliableTurn is one appliable turn: its changeset and its end checkpoint.
type AppliableTurn struct {
	Turn       string `json:"turn" doc:"The turn."`
	Changeset  string `json:"changeset" doc:"The copy at the turn's end, from its base: what an apply up to the turn writes, unless the turn heads an update in force, whose merge it writes then."`
	Checkpoint string `json:"checkpoint,omitempty" doc:"The copy's checkpoint at the turn's end."`
}

// UpdateInForce is a clean update: until the next apply the folder holds From, and an
// apply up to HeadTurn writes To. TheirsCheckpoint and MergedCheckpoint are the
// copy's checkpoints of From and To.
type UpdateInForce struct {
	From             string `json:"from" doc:"The folder as the update read it: the next apply's baseline."`
	To               string `json:"to" doc:"The merge: what an apply up to head_turn writes."`
	HeadTurn         string `json:"head_turn,omitempty" doc:"The latest unapplied turn when the update ran."`
	TheirsCheckpoint string `json:"theirs_checkpoint,omitempty" doc:"The copy's checkpoint of from."`
	MergedCheckpoint string `json:"merged_checkpoint,omitempty" doc:"The copy's checkpoint of to."`
}

// Head is the latest appliable turn, "" when there is none.
func (s State) Head() string {
	if len(s.Turns) == 0 {
		return ""
	}
	return s.Turns[len(s.Turns)-1].Turn
}

// Index is turn's place in Turns, -1 when it is not appliable.
func (s State) Index(turn string) int {
	for i, t := range s.Turns {
		if t.Turn == turn {
			return i
		}
	}
	return -1
}

// Changed reports whether turn recorded a changeset, appliable now or not (a
// discard may have dropped it, an update folded it).
func (s State) Changed(turn string) bool { return s.changed[turn] }

// CheckpointOf is the copy checkpoint whose content is changeset cs: the
// newest turn that ended with it, else the newest update that read (from) or
// merged (to) it. "" for an empty cs (the copy's base) or one never recorded.
func (s State) CheckpointOf(cs string) string {
	if cs == "" {
		return ""
	}
	if cp, ok := s.turnCheckpoint[cs]; ok {
		return cp
	}
	return s.mergeCheckpoint[cs]
}

// Fold builds a State from a workspace's outcomes, given in the record's
// order. The zero Fold is ready to use.
type Fold struct {
	s       State
	turns   []AppliableTurn
	applied []string // the turns successful applies went up to
	gone    map[string]bool
}

func (f *Fold) init() {
	if f.gone == nil {
		f.s.Mark = -1
		f.gone = map[string]bool{}
		f.s.changed = map[string]bool{}
		f.s.turnCheckpoint = map[string]string{}
		f.s.mergeCheckpoint = map[string]string{}
	}
}

// Changed is a turn that ended with changeset (its copy from base, "" when it
// could not be kept) at checkpoint. A turn without a changeset is not
// appliable. A turn given again is the same turn.
func (f *Fold) Changed(turn, changeset, checkpoint string) {
	f.init()
	if changeset == "" || f.s.changed[turn] {
		return
	}
	f.turns = append(f.turns, AppliableTurn{Turn: turn, Changeset: changeset, Checkpoint: checkpoint})
	f.s.turnCheckpoint[changeset] = checkpoint
	f.s.changed[turn] = true
}

// Applied is an apply up to turn that ended: ok when it wrote the folder, up
// to changeset to. Any apply ends a discard's standing; only a successful one
// moves the folder.
func (f *Fold) Applied(turn, to string, ok bool) {
	f.init()
	f.s.DiscardedAt = nil
	if !ok {
		return
	}
	f.s.AppliedTurn, f.s.AppliedTo, f.s.Applied = turn, to, true
	f.applied = append(f.applied, turn)
	f.s.Update, f.s.Markers = nil, false
}

// Discarded is a discard at when that dropped the turns dropped.
func (f *Fold) Discarded(when time.Time, dropped []string) {
	f.init()
	f.s.DiscardedAt = &when
	for _, t := range dropped {
		f.gone[t] = true
	}
	f.s.Update, f.s.Markers = nil, false
}

// Updated is a clean update u that folded the turns folded into its merge and
// left conflict markers when markers. A conflicting update changed nothing
// and is not given.
func (f *Fold) Updated(u UpdateInForce, folded []string, markers bool) {
	f.init()
	for _, t := range folded {
		f.gone[t] = true
	}
	f.s.Update = &u
	f.s.mergeCheckpoint[u.To] = u.MergedCheckpoint
	if u.From != u.To {
		f.s.mergeCheckpoint[u.From] = u.TheirsCheckpoint
	}
	if markers {
		f.s.Markers = true
	}
}

// State is the state the outcomes given so far add up to.
func (f *Fold) State() State {
	f.init()
	s := f.s
	s.Turns = nil
	for _, t := range f.turns {
		if !f.gone[t.Turn] {
			s.Turns = append(s.Turns, t)
		}
	}
	for _, t := range f.applied {
		s.Mark = max(s.Mark, s.Index(t))
	}
	return s
}

// AppliedSnapshot is a State as the record keeps it (workspace.snapshot): what a
// workspace's outcomes added up to when control took it. A fold that meets
// one starts again from it (Restore), so it stands for every outcome before
// it. Control writes one, once, for a workspace whose record was written
// before the record held its applied state (0.6.6), from the queue's job rows
// of that time, replayed through this same fold.
type AppliedSnapshot struct {
	Turns       []AppliableTurn `json:"turns" doc:"The appliable turns, in the order they ended."`
	Mark        int             `json:"mark" doc:"The index in turns of the furthest turn a successful apply went up to; -1 when none was."`
	AppliedTurn string          `json:"applied_turn,omitempty" doc:"The turn the latest successful apply went up to."`
	AppliedTo   string          `json:"applied_to,omitempty" doc:"The changeset the latest successful apply wrote: what the folder holds."`
	Applied     bool            `json:"applied" doc:"An apply succeeded."`
	Update      *UpdateInForce  `json:"update,omitempty" doc:"The clean update setting the folder's baseline, when no successful apply or discard came after it."`
	Markers     bool            `json:"markers,omitempty" doc:"A merge left conflict markers in the copy since the latest successful apply or discard."`
	DiscardedAt *time.Time      `json:"discarded_at,omitempty" doc:"When the latest apply-or-discard was a discard."`
	Changed     []string        `json:"changed" doc:"Every turn that recorded a changeset, dropped or folded ones too."`
	// TurnCheckpoints and MergeCheckpoints map a changeset to the copy
	// checkpoint holding it, from turns and from updates.
	TurnCheckpoints  map[string]string `json:"turn_checkpoints" doc:"Each turn's changeset and the checkpoint the turn ended with."`
	MergeCheckpoints map[string]string `json:"merge_checkpoints" doc:"Each update's from and to changesets and the copy's checkpoints of them."`
}

// Snapshot is s as the record keeps it.
func (s State) Snapshot() AppliedSnapshot {
	sn := AppliedSnapshot{Turns: s.Turns, Mark: s.Mark, AppliedTurn: s.AppliedTurn, AppliedTo: s.AppliedTo, Applied: s.Applied,
		Update: s.Update, Markers: s.Markers, DiscardedAt: s.DiscardedAt, Changed: []string{},
		TurnCheckpoints: s.turnCheckpoint, MergeCheckpoints: s.mergeCheckpoint}
	if sn.Turns == nil {
		sn.Turns = []AppliableTurn{}
	}
	for t := range s.changed {
		sn.Changed = append(sn.Changed, t)
	}
	sort.Strings(sn.Changed)
	return sn
}

// Restore starts the fold again from sn: every outcome given before stands
// replaced by it, and later ones fold on top.
func (f *Fold) Restore(sn AppliedSnapshot) {
	*f = Fold{}
	f.init()
	f.turns = append(f.turns, sn.Turns...)
	if sn.Mark >= 0 && sn.Mark < len(sn.Turns) {
		f.applied = []string{sn.Turns[sn.Mark].Turn}
	}
	f.s.AppliedTurn, f.s.AppliedTo, f.s.Applied = sn.AppliedTurn, sn.AppliedTo, sn.Applied
	f.s.Update, f.s.Markers, f.s.DiscardedAt = sn.Update, sn.Markers, sn.DiscardedAt
	for _, t := range sn.Changed {
		f.s.changed[t] = true
	}
	for _, t := range sn.Turns {
		f.s.changed[t.Turn] = true
	}
	for k, v := range sn.TurnCheckpoints {
		f.s.turnCheckpoint[k] = v
	}
	for k, v := range sn.MergeCheckpoints {
		f.s.mergeCheckpoint[k] = v
	}
}

// Empty is the state of a workspace with no outcomes.
func Empty() State {
	var f Fold
	return f.State()
}
