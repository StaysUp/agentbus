package workspace

import (
	"strings"

	"github.com/staysup/agentbus/ids"
)

// Place is one of a workspace's folders, as its copy and its changesets hold
// it: the folder's reference, and its name, the folder under the copy's top
// that holds its files. A workspace with one folder has one place with no
// name: its copy is the folder itself, and a changeset's paths are the
// folder's own. A workspace with several has one place each, named, and a
// changeset's paths start with the name.
type Place struct {
	Name ids.FolderName `json:"name,omitempty"`
	Ref  string         `json:"folder_ref"`
}

// Prefix is where p's files start in a changeset: "" for a place with no
// name, else its name and a slash.
func (p Place) Prefix() string {
	if p.Name == "" {
		return ""
	}
	return string(p.Name) + "/"
}

// Under is the part of cs inside the place p, with p's prefix taken off each
// path: the changes to apply to p's folder.
func (cs *Changeset) Under(p Place) *Changeset {
	if cs == nil {
		return nil
	}
	pre := p.Prefix()
	out := &Changeset{Space: cs.Space, Checkpoint: cs.Checkpoint, Snapshot: cs.Snapshot, BaseTaken: cs.BaseTaken, Truncated: cs.Truncated, Files: []FileChange{}}
	for _, f := range cs.Files {
		if !strings.HasPrefix(f.Path, pre) || len(f.Path) == len(pre) {
			continue
		}
		f.Path = f.Path[len(pre):]
		out.Files = append(out.Files, f)
	}
	return out
}
