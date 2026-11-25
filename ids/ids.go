// Package ids holds the two identifier types that keep a session and the
// workspace its turns run in from standing in for one another.
//
// A SessionID names a conversation: a label that groups turns. A WorkspaceID
// names where turns run: the copy, its checkpoints, its folder, Apply and
// Discard. They are different strings, and the compiler keeps them apart, so no
// function about a copy can be handed a session by accident. The one bridge is
// a resolver that reads the record -- store.WorkspaceOf, queue.workspaceID, or
// the API's workspaceOf -- which takes a SessionID and returns the WorkspaceID
// of the workspace the session runs in. A session migrated from before
// workspaces is its own workspace, and that identity is the only place a raw
// conversion between the two is written.
package ids

// WorkspaceID names a workspace: the copy its turns share, its checkpoints and
// its folder.
type WorkspaceID string

// SessionID names a session: a conversation that groups turns.
type SessionID string

// FolderName names one folder of a workspace: its place under the copy's top,
// and the group its files are listed under in a review. It is set once, when
// the workspace is made, and never computed again. It is not a path and not a
// folder's reference, and the compiler keeps it apart from both.
type FolderName string
