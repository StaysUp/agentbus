package workspace

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

// Entry is one file or folder List found: Rel is slash-separated from the
// root, Path the full path, Info its Lstat.
type Entry struct {
	Rel, Path string
	Info      fs.FileInfo
}

// List finds every file and folder under root, in no set order, reading
// several folders at once: a stat of each entry is most of a walk's cost,
// and a disk answers several at once faster than one after another. A
// symbolic link is listed, never followed. The root's .git is left out
// when skipGit. A folder it cannot read is left out with what it holds;
// root itself is an error. Past limit files (not folders) it stops, and
// truncated is true.
func List(root string, skipGit bool, limit int) (out []Entry, truncated bool, err error) {
	f, err := os.Open(root)
	if err != nil {
		return nil, false, err
	}
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		f.Close()
		if err == nil {
			err = &fs.PathError{Op: "list", Path: root, Err: fs.ErrInvalid}
		}
		return nil, false, err
	}
	f.Close()
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		files atomic.Int64
		stop  atomic.Bool
		sem   = make(chan struct{}, max(4, runtime.GOMAXPROCS(0)))
	)
	var visit func(dir, rel string)
	visit = func(dir, rel string) {
		defer wg.Done()
		if stop.Load() {
			return
		}
		sem <- struct{}{}
		var mine []Entry
		var subs []Entry
		if d, err := os.Open(dir); err == nil {
			names, _ := d.Readdirnames(-1)
			d.Close()
			for _, name := range names {
				r := name
				if rel != "" {
					r = rel + "/" + name
				}
				if skipGit && rel == "" && name == ".git" {
					continue
				}
				p := filepath.Join(dir, name)
				info, err := os.Lstat(p)
				if err != nil {
					continue
				}
				e := Entry{Rel: r, Path: p, Info: info}
				mine = append(mine, e)
				if info.IsDir() {
					subs = append(subs, e)
				} else if files.Add(1) > int64(limit) {
					stop.Store(true)
					break
				}
			}
		}
		<-sem
		mu.Lock()
		out = append(out, mine...)
		mu.Unlock()
		for _, e := range subs {
			wg.Add(1)
			go visit(e.Path, e.Rel)
		}
	}
	wg.Add(1)
	visit(root, "")
	wg.Wait()
	return out, stop.Load(), nil
}
