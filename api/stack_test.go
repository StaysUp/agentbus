package api

import (
	"context"
	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/connector"
	"net"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/runner"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// stack is what a turn needs behind the API, as agentbus up has it: the job
// queue, the jobs API, the engine and a runner that takes jobs over HTTP.
type stack struct {
	wire        run.Wire
	maxRuns     int
	permissions string
	grace       time.Duration
	turnsToken  bool
	turnWait    time.Duration
	// before runs on the store before the engine opens it.
	before func(st *store.Store)
}

// stackEnv is an API on loopback with a whole stack behind it.
func stackEnv(t *testing.T, s stack) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	if s.before != nil {
		s.before(st)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	base := "http://127.0.0.1:" + port
	if s.maxRuns == 0 {
		s.maxRuns = run.DefaultMaxRuns
	}
	// permissions is a permissions.toml path: its folder is the settings'.
	var cfgStore settings.Store
	if s.permissions != "" {
		cfgStore = settings.NewFiles(filepath.Dir(s.permissions))
	}
	eng, err := run.New(run.Config{Store: st, BaseURL: base, Wire: s.wire, MaxRuns: s.maxRuns, Settings: cfgStore,
		RunGrace: s.grace})
	if err != nil {
		t.Fatal(err)
	}
	q, err := queue.Open(filepath.Join(dir, "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fr, err := folders.Open(filepath.Join(dir, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := jobs.New(jobs.Config{Queue: q, Token: token, EnrollToken: enrollTok, Heartbeat: 50 * time.Millisecond,
		MaxWait: 500 * time.Millisecond, Folders: fr, Blobs: bs})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: st, Token: token, Port: port, BaseURL: base, Queue: q, Jobs: js, Engine: eng, TurnWait: s.turnWait,
		Settings: cfgStore, Folders: fr}
	if s.turnsToken {
		cfg.TurnsToken = turnsTok
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = a
	srv.Start()
	ctx, cancel := context.WithCancel(context.Background())
	// As in agentbus up: a connector beside the runner, sharing the blob
	// store on disk.
	conn := &connector.Connector{Folders: fr, Store: bs, State: filepath.Join(dir, "connector")}
	cn, err := runner.Enroll(ctx, runner.Config{Queue: base, EnrollToken: enrollTok, Name: "connector", Capacity: 2,
		Role: queue.RoleConnector, FolderSide: connector.Jobs{Connector: conn}, Folders: fr, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := runner.Enroll(ctx, runner.Config{Queue: base, EnrollToken: enrollTok, Name: "test", Capacity: s.maxRuns,
		Engine: eng, Store: st, Workdir: filepath.Join(dir, "sessions"), Settings: cfgStore, Logf: t.Logf, Folders: fr,
		Lender: conn, Blobs: bs, BlobSink: bs})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 2)
	go func() { rn.Run(ctx); done <- struct{}{} }()
	go func() { cn.Run(ctx); done <- struct{}{} }()
	t.Cleanup(func() {
		cancel()
		<-done
		<-done
		eng.StopAll()
		srv.Close()
		q.Close()
		st.Close()
	})
	return &env{srv: srv, api: a, store: st, port: port, q: q, eng: eng, workdir: filepath.Join(dir, "sessions")}
}
