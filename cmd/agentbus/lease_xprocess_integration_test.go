//go:build integration

package main

// Control alone decides when a turn ended (runner/lease.go), across
// processes: a runner whose link to control drops for less than control's
// 30 s lease keeps its turn, which finishes when the link is back. (Review
// t_5d83bfd85c47b87c P1: the runner kept a clock of its own and stopped a turn
// control still held.) A turn control does decide lost waits for the runner:
// runner.TestNextTurnWaitsForALostAttemptsAgent.

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// holeProxy forwards HTTP to control until it is made a black hole: then it
// takes every request but never answers, as a network gap does (the caller
// times out; nothing is refused). It names control's own address to it.
type holeProxy struct {
	srv  *httptest.Server
	quit chan struct{} // closed at cleanup: every held request ends
	mu   sync.Mutex
	hole bool
}

func newHoleProxy(t *testing.T, base string) *holeProxy {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	p := &holeProxy{quit: make(chan struct{})}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(u) }, FlushInterval: -1}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.holed() {
			select { // never answered
			case <-r.Context().Done():
			case <-p.quit:
			}
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { close(p.quit); p.srv.CloseClientConnections(); p.srv.Close() })
	return p
}

func (p *holeProxy) url() string { return p.srv.URL }

func (p *holeProxy) holed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hole
}

// setHole makes the proxy a black hole (true), or forwards again (false).
func (p *holeProxy) setHole(on bool) {
	p.mu.Lock()
	p.hole = on
	p.mu.Unlock()
}

func TestRunnerKeepsTurnThroughAShortGap(t *testing.T) {
	t.Parallel() // its own up, runner and data folder
	folder := t.TempDir()
	x := startServer(t, "claude")
	p := newHoleProxy(t, x.base)
	mark := filepath.Join(t.TempDir(), "stopped")
	ran := filepath.Join(t.TempDir(), "ran")
	release := filepath.Join(t.TempDir(), "go")
	x.setAgentEnv("AGENTBUS_FAKE_WAIT="+release, "AGENTBUS_FAKE_WAIT_S=300",
		"AGENTBUS_FAKE_TERM_MARK="+mark, "AGENTBUS_FAKE_MARK="+ran, "AGENTBUS_FAKE_STDOUT=done")
	// The runner reaches control only through the proxy.
	x.addRunnerVia(p.url(), "far", folder)
	fs := x.folders()
	code, st := x.startTurnX(map[string]any{"agent": "claude", "folder_ref": fs[0].Ref, "machine": fs[0].Machine,
		"prompt": "hello", "isolate": true})
	if code/100 != 2 || st.Turn == "" {
		t.Fatalf("start: %d %+v", code, st)
	}
	waitForN(t, "the turn's agent runs", 30*time.Second, func() bool { _, err := os.Stat(ran); return err == nil })
	// 25 s with no answer: past what the runner's own clock allowed (20 s),
	// short of control's lease (30 s).
	p.setHole(true)
	time.Sleep(25 * time.Second)
	p.setHole(false)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if state, why := waitEndedState(t, x, st.Turn, 90*time.Second); state != "finished" {
		t.Fatalf("the turn ended %s (%s), want finished: control still held it", state, why)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the runner stopped a turn control still held")
	}
}
