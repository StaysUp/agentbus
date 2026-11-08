package api

// The forbid entries of each live turn, for the gateway's guard
// (gateway/guard.go). A turn's entries are the ones its permissions.toml held
// when it started, read once by startTurn with the same parser as the launch
// flags. They are set before the child starts and removed when it exits, or
// when it never starts.

import "sync"

type turnKey struct{ session, turn string }

type turnRules struct {
	mu sync.Mutex
	m  map[turnKey][]string
}

func (t *turnRules) set(session, turn string, forbid []string) {
	if len(forbid) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[turnKey][]string{}
	}
	t.m[turnKey{session, turn}] = append([]string(nil), forbid...)
}

func (t *turnRules) clear(session, turn string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, turnKey{session, turn})
}

// Forbid implements gateway.Guard: the forbid entries of a live turn, none
// for any other.
func (a *API) Forbid(session, turn string) []string {
	a.turnRules.mu.Lock()
	defer a.turnRules.mu.Unlock()
	return a.turnRules.m[turnKey{session, turn}]
}
