package queue

import (
	"encoding/json"

	"github.com/staysup/agentbus/workspace"
)

// Result is what a finished job's result holds: how its turn ended, and what
// it changed in its workspace. The runner writes it; the queue and the API
// read it back. It is the one declaration of that format.
type Result struct {
	// Session is the session the turn ran in. For a turn job it is the job's
	// own session; a review runs in a session of its own.
	Session    string   `json:"session,omitempty"`
	State      string   `json:"state,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Stopped    bool     `json:"stopped,omitempty"`
	Unsettled  bool     `json:"unsettled,omitempty"`
	Error      string   `json:"error,omitempty"`
	Reply      string   `json:"reply,omitempty"`
	ModelCalls int      `json:"model_calls,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	// Refused: why the turn did not start, and the status that says what
	// kind (run.Refusal).
	Refused string `json:"refused,omitempty"`
	Status  int    `json:"status,omitempty"`
	// ReplyText, for a review job and asked work: the whole text of the turn's
	// last reply, at most runner.MaxReplyText bytes. The queue reads the
	// verdict from it. Reply is only its first 500 characters.
	ReplyText string `json:"reply_text,omitempty"`
	// Folder and Changes: the job's workspace and what changed in it, set
	// when the runner watched a folder.
	Folder  string             `json:"folder,omitempty"`
	Changes *workspace.Changes `json:"changes,omitempty"`
}

// ReadResult reads a job's result. An empty one is the zero Result.
func ReadResult(raw json.RawMessage) (Result, error) {
	var r Result
	if len(raw) == 0 {
		return r, nil
	}
	err := json.Unmarshal(raw, &r)
	return r, err
}
