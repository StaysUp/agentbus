package api

// GET /api/v0/runners: the runners and connectors control knows, for a
// person. The runner protocol, under /api/v0/runner/, is the machines' side;
// this is the read of it a screen shows. It holds no token: a runner's token
// is stored only as its hash, and that stays in the queue.

import (
	"net/http"
	"time"

	"github.com/staysup/agentbus/queue"
)

// RunnerInfo is one runner or connector in GET /api/v0/runners.
type RunnerInfo struct {
	ID       string    `json:"id" doc:"The runner's id, as GET /api/v0/harnesses names it."`
	Name     string    `json:"name" doc:"The name it enrolled with, such as its machine's."`
	Role     string    `json:"role" doc:"runner, which runs turns, or connector, which owns folders and takes only their snapshot and apply jobs."`
	Online   bool      `json:"online" doc:"It asked control for work in the last minute."`
	LastSeen time.Time `json:"last_seen" doc:"When it last asked control for work."`
	Enrolled time.Time `json:"enrolled" doc:"When it enrolled."`
	Capacity int       `json:"capacity" doc:"How many jobs it runs at once."`
	Labels   []string  `json:"labels" doc:"The labels it enrolled with. Empty when none."`
	OS       string    `json:"os,omitempty" doc:"Its operating system, as Go names it, such as linux or darwin. Absent before its first report."`
	Arch     string    `json:"arch,omitempty" doc:"Its processor, as Go names it, such as amd64 or arm64. Absent before its first report."`
	// Harnesses: the runner's last probe of each harness.
	Harnesses []RunnerProbe `json:"harnesses" doc:"Its last probe of each harness, in the order it reported them. Empty for a connector, and before its first probe ends."`
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
	Runners []RunnerInfo `json:"runners" doc:"Newest seen first. A runner that stopped stays listed, offline."`
}

func (a *API) listRunners(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	list := []RunnerInfo{}
	if a.cfg.Queue != nil {
		all, err := a.cfg.Queue.Runners(queue.DefaultTeam)
		if err != nil {
			a.internal(w)
			return
		}
		now := a.cfg.Now()
		for _, rn := range all {
			list = append(list, runnerInfo(rn, now))
		}
	}
	writeJSON(w, http.StatusOK, RunnerList{list})
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
	v.OS, v.Arch = c.OS, c.Arch
	for _, rep := range c.Harnesses {
		v.Harnesses = append(v.Harnesses, RunnerProbe{Name: rep.Name, Found: rep.Found, Version: rep.Version, SignedIn: rep.SignedIn})
	}
	return v
}
