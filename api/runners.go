package api

// GET /api/v0/runners: the runners control knows, one per machine, for a
// person, and the machines waiting for the person to approve them (agentbus
// login). The runner protocol, under /api/v0/runner/, is the machines' side;
// this is the read of it a screen shows, and the person's decisions: approve
// or refuse a machine, remove one. It holds no token and no sign-in code: a
// runner's token is stored only as its hash, and that stays in the queue.

import (
	"errors"
	"net/http"
	"time"

	"github.com/staysup/agentbus/queue"
)

// RunnerInfo is one runner in GET /api/v0/runners.
type RunnerInfo struct {
	ID   string `json:"id" doc:"The runner's id, as GET /api/v0/harnesses names it."`
	Name string `json:"name" doc:"The name it enrolled with, such as its machine's."`
	Role string `json:"role" doc:"runner: the one program of its machine, which holds its folders and runs its sessions. connector: a connector of a build before 0.5, which takes no work from this build."`
	// Connector is the runner's machine, which a new session names.
	Connector string    `json:"connector,omitempty" doc:"The id of the runner's machine (con_ and 16 hex digits): a new session on this machine names it as connector, with a folder typed for that machine. Absent before its first report."`
	Online    bool      `json:"online" doc:"It asked control for work in the last minute."`
	LastSeen  time.Time `json:"last_seen" doc:"When it last asked control for work."`
	Enrolled  time.Time `json:"enrolled" doc:"When it enrolled."`
	Capacity  int       `json:"capacity" doc:"How many jobs it runs at once."`
	Labels    []string  `json:"labels" doc:"The labels it enrolled with. Empty when none."`
	OS        string    `json:"os,omitempty" doc:"Its operating system, as Go names it, such as linux or darwin. Absent before its first report."`
	Arch      string    `json:"arch,omitempty" doc:"Its processor, as Go names it, such as amd64 or arm64. Absent before its first report."`
	// Harnesses: the runner's last probe of each harness.
	Harnesses []RunnerProbe `json:"harnesses" doc:"Its last probe of each harness, in the order it reported them. Empty for a connector, and before its first probe ends."`
	Sandbox   *SandboxInfo  `json:"sandbox,omitempty" doc:"Whether it can build a session's wall (the sandbox setting). Absent from a runner of a build before it reported it."`
}

// SandboxInfo is whether a runner can build a session's wall.
type SandboxInfo struct {
	Can bool   `json:"can" doc:"It can: sandbox-exec on macOS, or bwrap on Linux with user namespaces allowed, started a program in a wall when the runner started."`
	Why string `json:"why,omitempty" doc:"Why it cannot, in a plain sentence, such as AppArmor refusing user namespaces on Ubuntu 24.04. Absent when it can."`
}

// PendingMachine is a machine waiting for the person's approval.
type PendingMachine struct {
	ID       string       `json:"id" doc:"The sign-in's id (lgn_ and 16 hex digits): approve or refuse it by this."`
	Name     string       `json:"name" doc:"The name the machine gave, such as its host name. Anyone who reaches control can ask; the code the machine shows is what proves it is yours."`
	OS       string       `json:"os,omitempty" doc:"Its operating system, as it reported it."`
	Arch     string       `json:"arch,omitempty" doc:"Its processor, as it reported it."`
	Labels   []string     `json:"labels" doc:"The labels it asked for. Empty when none."`
	Capacity int          `json:"capacity" doc:"How many jobs it will run at once."`
	Sandbox  *SandboxInfo `json:"sandbox,omitempty" doc:"Whether it can build a session's wall, as it reported it."`
	Asked    time.Time    `json:"asked" doc:"When it asked."`
	Expires  time.Time    `json:"expires" doc:"When it stops waiting: then agentbus login must be run again."`
}

// RunnerProbe is one harness as one runner last probed it.
type RunnerProbe struct {
	Name     string `json:"name" doc:"The harness, such as claude."`
	Found    bool   `json:"found" doc:"The binary is on that runner's PATH."`
	Version  string `json:"version,omitempty" doc:"From <bin> --version there. Absent when not found or not shaped like a version."`
	SignedIn *bool  `json:"signed_in,omitempty" doc:"From the harness's own offline status command there. Absent when it gave no clear answer."`
}

// RunnerList is the answer to GET /api/v0/runners.
type RunnerList struct {
	Runners []RunnerInfo     `json:"runners" doc:"Newest seen first, one per machine. A runner that stopped stays listed, offline, until the person removes it. A connector of a build before 0.5 is listed only while no runner reports its machine."`
	Pending []PendingMachine `json:"pending" doc:"Machines that ran agentbus login and wait for the person, oldest first. None of them takes work. Empty when none."`
}

func (a *API) listRunners(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	list := []RunnerInfo{}
	pending := []PendingMachine{}
	if a.cfg.Queue != nil {
		logins, err := a.cfg.Queue.PendingLogins(queue.DefaultTeam)
		if err != nil {
			a.internal(w)
			return
		}
		for _, l := range logins {
			pending = append(pending, pendingMachine(l))
		}
		all, err := a.cfg.Queue.Runners(queue.DefaultTeam)
		if err != nil {
			a.internal(w)
			return
		}
		now := a.cfg.Now()
		// The machines a runner reports. A connector of a build before 0.5
		// on one of them is the same machine, now its runner's: it stays in
		// the queue, and is not listed twice.
		held := map[string]bool{}
		for _, rn := range all {
			if rn.Caps != nil && rn.Caps.Role == "" && rn.Caps.Connector != "" {
				held[rn.Caps.Connector] = true
			}
		}
		for _, rn := range all {
			if rn.Caps != nil && rn.Caps.Role == queue.RoleConnector && held[rn.Caps.Connector] {
				continue
			}
			list = append(list, runnerInfo(rn, now))
		}
	}
	writeJSON(w, http.StatusOK, RunnerList{list, pending})
}

// runnerInfo is rn as a person reads it, online when seen within
// queue.RunnerFresh of now.
func runnerInfo(rn queue.Runner, now time.Time) RunnerInfo {
	v := RunnerInfo{ID: rn.ID, Name: rn.Name, Role: "runner", Online: now.Sub(rn.Seen) <= queue.RunnerFresh,
		LastSeen: rn.Seen.UTC(), Enrolled: rn.Created.UTC(), Capacity: rn.Capacity, Labels: rn.Labels, Harnesses: []RunnerProbe{}}
	if v.Labels == nil {
		v.Labels = []string{}
	}
	c := rn.Caps
	if c == nil {
		return v
	}
	if c.Role == queue.RoleConnector {
		v.Role = queue.RoleConnector
	}
	v.OS, v.Arch, v.Connector, v.Sandbox = c.OS, c.Arch, c.Connector, sandboxInfo(c)
	for _, rep := range c.Harnesses {
		v.Harnesses = append(v.Harnesses, RunnerProbe{Name: rep.Name, Found: rep.Found, Version: rep.Version, SignedIn: rep.SignedIn})
	}
	return v
}

func sandboxInfo(c *queue.Capabilities) *SandboxInfo {
	if c == nil || c.Sandbox == nil {
		return nil
	}
	return &SandboxInfo{Can: c.Sandbox.Can, Why: c.Sandbox.Why}
}

func pendingMachine(l queue.Login) PendingMachine {
	p := PendingMachine{ID: l.ID, Name: l.Name, Labels: l.Labels, Capacity: l.Capacity, Asked: l.Created.UTC(), Expires: l.Expires.UTC(),
		Sandbox: sandboxInfo(l.Caps)}
	if p.Labels == nil {
		p.Labels = []string{}
	}
	if l.Caps != nil {
		p.OS, p.Arch = l.Caps.OS, l.Caps.Arch
	}
	return p
}

// Approval is the body of POST /api/v0/runners/pending/{id}/approve.
type Approval struct {
	Code string `json:"code" doc:"The code agentbus login shows on the machine, such as BCDF-GHJK; the dash and case do not matter. The list never holds it: typing it proves the machine is the one in front of you."`
}

// POST /api/v0/runners/pending/{id}/approve
func (a *API) approveMachine(w http.ResponseWriter, r *http.Request, id string) {
	var body Approval
	if !decodeObject(r, &body) || body.Code == "" || len(body.Code) > 32 {
		writeError(w, http.StatusBadRequest, "the body must be one JSON object with code, the code the machine shows")
		return
	}
	if a.cfg.Jobs == nil {
		writeError(w, http.StatusNotFound, "this service keeps no job queue")
		return
	}
	rn, err := a.cfg.Jobs.ApproveLogin(id, body.Code)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, "no machine waits by that id")
	case errors.Is(err, queue.ErrBadState):
		writeError(w, http.StatusConflict, "that machine was approved or refused already, or waited too long: run agentbus login on it again")
	case errors.Is(err, queue.ErrWrongCode):
		writeError(w, http.StatusBadRequest, "that is not the code the machine shows")
	case err != nil:
		a.internal(w)
	default:
		writeJSON(w, http.StatusOK, runnerInfo(rn, a.cfg.Now()))
	}
}

// POST /api/v0/runners/pending/{id}/refuse
func (a *API) refuseMachine(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	if a.cfg.Jobs == nil {
		writeError(w, http.StatusNotFound, "this service keeps no job queue")
		return
	}
	err := a.cfg.Jobs.RefuseLogin(id)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, "no machine waits by that id")
	case errors.Is(err, queue.ErrBadState):
		writeError(w, http.StatusConflict, "that machine was approved or refused already, or waited too long")
	case err != nil:
		a.internal(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// POST /api/v0/runners/{id}/remove
func (a *API) removeRunner(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	if a.cfg.Jobs == nil {
		writeError(w, http.StatusNotFound, "this service keeps no job queue")
		return
	}
	err := a.cfg.Jobs.RemoveRunner(id)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		writeError(w, http.StatusNotFound, "no runner by that id")
	case errors.Is(err, queue.ErrRunnerOnline):
		writeError(w, http.StatusConflict, "that runner is online: stop it on its machine first, then remove it a minute later")
	case err != nil:
		a.internal(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
