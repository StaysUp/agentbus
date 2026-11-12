package main

// The folder side, in the runner of each machine. A runner holds its
// machine's folders, makes each session's copy from its folder by clone or
// overlay, and applies changes back into the folder (package connector,
// which is no program of its own since 0.5). In agentbus up it shares the
// control plane's blob store on the same disk, so nothing is sent over
// HTTP; a runner of a server keeps its own store, with the server's behind
// it.

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/connector"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/jobs"
)

// blobClient is the control plane's blob store over HTTP, with a runner's
// token: no overall timeout, since a blob is as big as a file.
func blobClient(server, token string) blobs.Client {
	return blobs.Client{URL: server, Token: token, HTTP: &http.Client{Transport: http.DefaultTransport}}
}

// cacheDir is a runner's own blob store, in its data
// folder: apart from the control plane's (blobs), whose sweep keeps what
// open sessions need, so a cache's sweep never reaches control's blobs even
// where the two share a data folder.
const cacheDir = "blob-cache"

// sweepCache cleans a runner's own blob store, at start and
// every jobs.SweepEvery, until ctx ends. That store is a cache of the control
// plane's: a blob it lacks is fetched again from there (blobs.Local), and the
// control plane keeps every blob an open session needs (jobs.Sweeper). So
// every blob older than the grace may go; one fetched or sent within it, or
// said to be held, stays.
func sweepCache(ctx context.Context, st *blobs.Store, logf func(string, ...any)) {
	t := time.NewTicker(jobs.SweepEvery)
	defer t.Stop()
	for {
		if _, n, err := st.Sweep(ctx, func(string) bool { return false }, jobs.SweepGrace); err == nil && n > 0 {
			logf("blobs.cache_sweep removed=%d", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// folderSide is this machine's folder side, for its runner to do its folder
// jobs and lend from (runner.Config.FolderSide, Lender). store, in agentbus
// up, is the control plane's own: nothing is sent over HTTP. Otherwise it
// is the runner's own store, and the control plane's is set as its Remote
// once the runner has its token.
func folderSide(home string, fr *folders.Registry, store *blobs.Store) *connector.Connector {
	c := &connector.Connector{Folders: fr, Store: store, State: filepath.Join(home, "connector"), Link: isolation.Clone}
	// A folder inside a session's overlay copy on this machine (a session
	// started in another's copy) is mounted again before it is read or
	// written: after a restart the copy's mount is gone.
	c.Prepare = func(path string) error {
		for _, dir := range []string{copiesDir, oldCopiesDir} {
			if s, err := isolation.Load(filepath.Join(home, dir), path); err == nil {
				return isolation.Remount(s)
			}
		}
		return nil // not in a copy
	}
	return c
}
