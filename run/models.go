package run

// What a harness's own models cache held after a turn: the names of the
// models its sign-in may use, which the unmodified harness wrote in the
// session's home (harness.Info.ModelsCache, such as codex's
// models_cache.json). The runner reports them to control with its harnesses
// (Report.Models), so the agent form can offer them. Only the names are
// read; nothing else of the file or the home leaves it.

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/staysup/agentbus/harness"
)

// SeenModels are the model names a harness's cache held, and when they were
// read.
type SeenModels struct {
	Names []string
	At    time.Time
}

type seenModels struct {
	mu sync.Mutex
	m  map[string]SeenModels
}

// maxModelsCache bounds the cache file read.
const maxModelsCache = 4 << 20

// maxSeen bounds how many names are kept of one cache.
const maxSeen = 500

// readModels reads agent's models cache from session's home, if the harness
// keeps one and the session has a home of its own. A cache that is missing,
// too big or not readable changes nothing.
func (e *Engine) readModels(session, agent string) {
	h, ok := harness.Get(agent)
	if !ok || e.cfg.Homes == "" || !SessionShape.MatchString(session) {
		return
	}
	i := h.Info()
	if i.ModelsCache == "" || i.ReadModelsCache == nil {
		return
	}
	root, err := os.OpenRoot(filepath.Join(e.cfg.Homes, session))
	if err != nil {
		return
	}
	defer root.Close()
	f, err := harness.OpenFile(root, filepath.Join(i.ConfigDir, i.ModelsCache))
	if err != nil {
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, maxModelsCache+1))
	f.Close()
	if err != nil || len(b) > maxModelsCache {
		return
	}
	names := i.ReadModelsCache(b)
	if len(names) == 0 {
		return
	}
	if len(names) > maxSeen {
		names = names[:maxSeen]
	}
	e.models.mu.Lock()
	defer e.models.mu.Unlock()
	if e.models.m == nil {
		e.models.m = map[string]SeenModels{}
	}
	e.models.m[agent] = SeenModels{Names: slices.Clone(names), At: e.cfg.Now().UTC()}
}

// Models is what each harness's own models cache held after its latest turn
// here, by harness.
func (e *Engine) Models() map[string]SeenModels {
	e.models.mu.Lock()
	defer e.models.mu.Unlock()
	out := make(map[string]SeenModels, len(e.models.m))
	for k, v := range e.models.m {
		out[k] = SeenModels{Names: slices.Clone(v.Names), At: v.At}
	}
	return out
}
