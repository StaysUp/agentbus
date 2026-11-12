package jobs

// A command job in control's record (the person's "!", queue.KindCommand):
// its runner's start stores command.started, and its end stores command.ran
// (the runner's own event, from the finish) or command.failed. The API
// stores command.queued when it makes the job, and answers at once. Once
// command.ran is in the record, the job keeps only its id (outcome.go).

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// commandStarted stores command.started for a command job's attempt that
// started. Its id is the attempt's, so a start sent again stores it once.
func (s *Server) commandStarted(rn queue.Runner, attempt string, number int) error {
	if s.cfg.Record == nil {
		return nil
	}
	j, err := s.cfg.Queue.Current(rn.ID, attempt, number)
	if err != nil || j.Kind != queue.KindCommand {
		return nil
	}
	_, err = s.cfg.Record.AppendOnce(event.Event{ID: "evt_cs_" + attempt, Kind: event.CommandStarted, Time: time.Now().UTC(),
		Session: j.Session, Command: &event.Command{ID: j.Spec.CommandID, Line: j.Spec.Command}})
	return err
}

// commandRan is a command job's command.ran (queue.Result.Ran), checked
// before the finish is: control keeps it once the queue accepts the finish.
// It must be a command.ran of no turn and, for the current attempt, of the
// job's session and command. nil with true: nothing to keep. false once it
// has answered the runner.
func (s *Server) commandRan(w http.ResponseWriter, rn queue.Runner, attempt string, b FinishCall) (*event.Event, bool) {
	res, _ := queue.ReadResult(b.Result)
	if s.cfg.Record == nil || len(res.Ran) == 0 {
		return nil, true
	}
	var e event.Event
	bad := json.Unmarshal(res.Ran, &e) != nil || e.ID == "" || e.Kind != event.CommandRan || e.Command == nil ||
		e.Session == "" || e.Turn != ""
	// A finish of an attempt that is not current is End's to answer.
	if j, err := s.cfg.Queue.Current(rn.ID, attempt, b.Number); err == nil && j.Kind == queue.KindCommand &&
		(e.Session != j.Session || e.Command == nil || e.Command.ID != j.Spec.CommandID) {
		bad = true
	}
	if bad {
		fail(w, http.StatusBadRequest, "a command job's result needs its command.ran event, of the job's session and command and no turn")
		return nil, false
	}
	e.Seq, e.Folder = 0, ""
	return &e, true
}

// Recorded is the part of control's record a command's end is written to.
type Recorded interface {
	AppendOnce(e event.Event) (bool, error)
}

// CommandEnded stores what control records when command job j ended: ran,
// its command.ran when it ran, else command.failed with why. Each is stored
// once, by its id.
func CommandEnded(rec Recorded, j queue.Job, ran *event.Event) error {
	if ran != nil && ran.Session == j.Session && ran.Command != nil && ran.Command.ID == j.Spec.CommandID {
		_, err := rec.AppendOnce(*ran)
		return err
	}
	res, _ := queue.ReadResult(j.Result)
	why := "the command did not run"
	switch {
	case res.Refused != "":
		why = res.Refused
	case j.Reason == queue.ReasonRunnerLost:
		why = "the session's runner stopped answering while the command ran"
	case j.State == queue.Cancelled:
		why = "the command was stopped before it ran"
	}
	t := time.Now().UTC()
	if j.Ended != nil {
		t = *j.Ended
	}
	_, err := rec.AppendOnce(event.Event{ID: "evt_cf_" + j.ID, Kind: event.CommandFailed, Time: t, Session: j.Session,
		Command: &event.Command{ID: j.Spec.CommandID, Line: j.Spec.Command}, Error: why})
	return err
}

// CloseCommands stores the end of each command the record has queued with
// no end, whose job has ended with no finish to say so: its lease ran out,
// or its finish could not be stored. It is CloseLost for commands.
func CloseCommands(q *queue.Queue, st *store.Store) (int, error) {
	open, err := st.OpenCommands()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range open {
		j, err := q.JobOfCommand(queue.DefaultTeam, id)
		if err != nil || j.Ended == nil {
			continue
		}
		res, _ := queue.ReadResult(j.Result)
		if err := CommandEnded(st, j, ranOf(res)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ranOf is the command.ran a command job's result res carries, or nil.
// CommandEnded checks it is the job's.
func ranOf(res queue.Result) *event.Event {
	if len(res.Ran) == 0 {
		return nil
	}
	var e event.Event
	if json.Unmarshal(res.Ran, &e) != nil || e.Kind != event.CommandRan || e.Turn != "" {
		return nil
	}
	e.Seq, e.Folder = 0, ""
	return &e
}
