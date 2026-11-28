package runner

// When a turn has ended, and how, is decided in one place: control, when the
// turn's job ends (queue.End for a finish it takes, its lease sweep for an
// attempt it lost). It is recorded once, as the job's state and as the
// record's turn.ended (jobs.recordTurnEnded), and everything that says how a
// turn ended reads it: the API (api.view), the record's turns
// (store.Turns), the runs control closes with it (jobs.closeOpenRuns,
// jobs.CloseLost). A second finish of the same attempt is refused (the
// attempt fence), as Buildkite answers a job finished twice with 422 and
// Temporal a completed activity's token with ActivityTaskNotFound.
//
// The runner decides none of it. It keeps no clock of its own over an
// attempt: it runs the turn until control answers that the attempt is no
// longer current (a heartbeat or any call answered stale or 404), and only
// then stops it (attempt.stale). While control does not answer, from a
// restart or a link that is down, the turn goes on; control decides on its
// own clock whether the attempt was lost (its lease, renewed for every
// attempt when control starts: queue.RenewLeases), and the runner hears that
// decision when it next reaches control.
//
// So the runner never gives up what control may still hold. It keeps a
// turn's end on disk (pending.go) until control answered its finish, and its
// record until control took it. And it never lets two attempts use one copy
// at once (holdCopy): a turn control has decided lost may run on here until
// the runner hears of it, and the next turn of its workspace waits for it.
