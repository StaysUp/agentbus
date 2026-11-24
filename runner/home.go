package runner

// What "Use my config" offers: which of the usual config files
// (settings.UsualHomeFiles) this machine's home holds, for a home job
// (queue.KindHome). The runner, not control, sees the home. It answers names,
// kinds and sizes only: no file is opened, and a vendor sign-in file is never
// counted or listed (settings.SignInFile).

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/staysup/agentbus/homefiles"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// homeWork answers a home job of this machine.
func (r *Runner) homeWork(at *attempt) queue.Finish {
	refuse := func(msg string) queue.Finish {
		return queue.Finish{Status: queue.Failed, Reason: queue.ReasonSetup,
			Result: mustJSON(queue.Result{Refused: msg, Status: http.StatusConflict})}
	}
	if r.cfg.Folders == nil || at.job.Spec.Machine != r.cfg.Folders.Machine() {
		return refuse("this runner is not on the machine the job names")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return refuse("this runner's home folder is not known")
	}
	return queue.Finish{Status: queue.Finished, Result: mustJSON(queue.Result{Home: ListHome(home, settings.UsualHomeFiles)})}
}

// ListHome is which of paths (each ~/x, a folder's ending in /) home
// holds: a file of the kind its path says, or a folder, each with its size.
// A path through a link is followed, as home_files copies it. Nothing is
// opened: Lstat and a walk of names only. A folder's walk stops past what
// home_files copies (Over).
func ListHome(home string, paths []string) []queue.HomeFile {
	out := []queue.HomeFile{}
	realHome := home
	if h, err := filepath.EvalSymlinks(home); err == nil {
		realHome = h
	}
	for _, p := range paths {
		rel := filepath.Clean(strings.TrimPrefix(p, "~/"))
		if settings.SignInFile(p) != "" {
			continue
		}
		src, err := filepath.EvalSymlinks(filepath.Join(home, rel))
		if err != nil {
			continue
		}
		if r, err := filepath.Rel(realHome, src); err == nil && settings.SignInFile(r) != "" {
			continue
		}
		info, err := os.Stat(src)
		if err != nil {
			continue
		}
		wantDir := strings.HasSuffix(p, "/")
		switch {
		case info.Mode().IsRegular() && !wantDir:
			f := queue.HomeFile{Path: p, Kind: "file", Size: info.Size()}
			if p == homefiles.ClaudeJSON {
				f.Part = "mcpServers"
			}
			out = append(out, f)
		case info.IsDir() && wantDir:
			f := queue.HomeFile{Path: p, Kind: "folder"}
			_ = filepath.WalkDir(src, func(q string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return nil
				}
				if r, err := filepath.Rel(realHome, q); err == nil && settings.SignInFile(r) != "" {
					return nil
				}
				i, err := d.Info()
				if err != nil {
					return nil
				}
				f.Files++
				f.Size += i.Size()
				if f.Files > homefiles.MaxGrantFiles || f.Size > homefiles.MaxGrant {
					f.Over = true
					return fs.SkipAll
				}
				return nil
			})
			out = append(out, f)
		}
	}
	return out
}
