package api

// GET /api/v0/version: which build of AgentBus this service is.

import "net/http"

// Version is the answer to GET /api/v0/version. The values are the ones
// agentbus version prints.
type Version struct {
	Version string `json:"version" doc:"The release, such as 0.1.0. dev for a build from a clone."`
	Commit  string `json:"commit,omitempty" doc:"The git commit the binary was built from. Absent when the build does not know it."`
	// Modified is always sent: false is an answer too.
	Modified bool `json:"modified" doc:"The commit's tree had changes when it was built. Always false for a release, and when commit is absent."`
}

func (a *API) version(w http.ResponseWriter, r *http.Request) {
	if !noBody(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, a.cfg.Build)
}
