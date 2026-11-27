package api

import "github.com/staysup/agentbus/queue"

// laneOf is the lane of a take for folder jobs, or else the interactive one,
// for tests that take either.
func laneOf(folders bool) queue.Lane {
	if folders {
		return queue.LaneFolders
	}
	return queue.LaneInteractive
}
