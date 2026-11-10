package isolation

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/staysup/agentbus/workspace"
)

// noneBackend is a folder used as it is: the isolation setting none, a job
// with isolate false, a review, or a worktree made before the backends kept
// a record. It makes no copy, so nothing can be applied. It exists so that
// such a job's changes come from the same checkpoints as an isolated turn's:
// Open takes the Base on the folder itself, every file hashed, and the job's
// end is one more checkpoint against it. Its record lives for one job only:
// the runner closes it when the job ends, and Close removes the record,
// never the folder. It saves no files, so a file's content in its diff is
// read from the folder while the file is still as the checkpoint saw it; a
// modified file's earlier content is not kept, so its diff is left out.
type noneBackend struct{}

func (noneBackend) Name() string { return None }

// Open records r.Folder as it is now, as the Base. The record is kept under
// r.Workdir, or in a folder of its own under the system's temporary folder
// when there is no workdir or the folder holds it, so the record is never
// among the files it records.
func (noneBackend) Open(r Request) (*Space, error) {
	if r.Folder == "" || !filepath.IsAbs(r.Folder) {
		return nil, &Refusal{"the folder must be an absolute path"}
	}
	if info, err := os.Stat(r.Folder); err != nil || !info.IsDir() {
		return nil, &Refusal{"the folder is not an existing directory"}
	}
	folder := filepath.Clean(r.Folder)
	s := &Space{ID: r.ID, Backend: None, Root: folder, Dir: folder, Workdir: r.Workdir}
	if s.Workdir == "" || holds(folder, s.Workdir) {
		tmp, err := os.MkdirTemp("", "agentbus-none-")
		if err != nil {
			return nil, err
		}
		s.Workdir, s.temp = tmp, true
	}
	if err := checkOpen(Request{ID: r.ID, Workdir: s.Workdir}); err != nil {
		s.dropTemp()
		return nil, err
	}
	// A record of the same id is an earlier attempt's: this one starts over.
	if err := removeState(s); err != nil {
		s.dropTemp()
		return nil, err
	}
	if err := s.save(); err != nil {
		noneBackend{}.Close(s)
		return nil, err
	}
	if _, err := checkpoint(s, Base, noneBackend{}); err != nil {
		noneBackend{}.Close(s)
		return nil, err
	}
	return s, nil
}

func (b noneBackend) Checkpoint(s *Space, name string) (Checkpoint, error) {
	return checkpoint(s, name, b)
}

func (noneBackend) Checkpoints(s *Space) ([]Checkpoint, error) { return checkpoints(s) }

// Diff is the changes between two checkpoints, with no from and to: there
// is no copy to apply them from.
func (noneBackend) Diff(s *Space, from, to string) (workspace.Changes, error) {
	c, err := diff(s, from, to)
	c.From, c.To = "", ""
	return c, err
}

// Close removes the record only. The folder is the person's own.
func (noneBackend) Close(s *Space) error {
	if s.Backend != None {
		return errors.New("isolation: not a folder used as it is")
	}
	err := removeState(s)
	s.dropTemp()
	return err
}

// take walks the folder. The Base hashes every file: the folder changes in
// place, so no earlier content can be read later to tell a touched file
// from a changed one. A later checkpoint hashes only what changed, and saves
// nothing.
func (noneBackend) take(s *Space, t *trees, prev *treeState, _ workspace.Hasher) (treeState, int, error) {
	if prev == nil {
		snap, err := workspace.Walk(s.Dir, nil, nil)
		if err != nil {
			return treeState{}, 0, err
		}
		return t.fromSnapshot(nil, nil, snap)
	}
	return walkAgainst(s, t, prev, workspace.Hash)
}

// learn is never needed: the Base hashed every file.
func (noneBackend) learn(*Space, string, workspace.File) (string, bool, bool) {
	return "", false, false
}

// dropTemp removes the temporary workdir Open made, if it made one.
func (s *Space) dropTemp() {
	if s.temp {
		os.RemoveAll(s.Workdir)
	}
}
