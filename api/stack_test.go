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
	wire run.Wire
	// capacity is the runner's own ceiling of delegated turns; 0: the
	// settings decide.
	capacity    int
	permissions string
	grace       time.Duration
	turnsToken  bool
	turnWait    time.Duration
	// before runs on the store before the engine opens it.
	before func(st *store.Store)
	// settings, when set, is the settings store, in place of permissions.
	settings settings.Store
	homes    string
	// record, when set, is the record the jobs server writes to, in place of
	// the store: a test wraps the store to make one of its writes fail.
	record func(st *store.Store) jobs.Recorder
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
	// permissions is a permissions.toml path: its folder is the settings'.
	var cfgStore settings.Store
	if s.permissions != "" {
		cfgStore = settings.NewFiles(filepath.Dir(s.permissions))
	}
	if s.settings != nil {
		cfgStore = s.settings
	}
	runCfg := run.Config{Store: st, BaseURL: base, Wire: s.wire, Settings: cfgStore,
		RunGrace: s.grace}
	if s.homes != "" {
		runCfg.Homes = s.homes
	}
	eng, err := run.New(runCfg)
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
	var rec jobs.Recorder = st
	if s.record != nil {
		rec = s.record(st)
	}
	js, err := jobs.New(jobs.Config{Queue: q, Token: token, EnrollToken: enrollTok, Heartbeat: 50 * time.Millisecond,
		MaxWait: 500 * time.Millisecond, Folders: fr, Blobs: bs, Record: rec, Settings: cfgStore})
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
	// As in agentbus up: one runner holds the folders, sharing the blob
	// store on disk.
	conn := &connector.Connector{Folders: fr, Store: bs, State: filepath.Join(dir, "connector")}
	rn, err := runner.Enroll(ctx, runner.Config{Queue: base, EnrollToken: enrollTok, Name: "test", Capacity: s.capacity,
		Engine: eng, Store: st, Workdir: filepath.Join(dir, "sessions"), Settings: cfgStore, Logf: t.Logf, Folders: fr,
		FolderSide: connector.Jobs{Connector: conn}, Lender: conn, Blobs: bs, BlobSink: bs})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	go func() { rn.Run(ctx); done <- struct{}{} }()
	// Control's finisher stops after the runner, as in the service: the
	// runner finishes its jobs as it stops.
	finStop, finDone := make(chan struct{}), make(chan struct{})
	go func() { js.RunFinisher(finStop); close(finDone) }()
	t.Cleanup(func() {
		cancel()
		<-done
		close(finStop)
		<-finDone
		eng.StopAll()
		srv.Close()
		q.Close()
		st.Close()
	})
	return &env{srv: srv, api: a, store: st, port: port, q: q, eng: eng, fr: fr, workdir: filepath.Join(dir, "sessions")}
}
