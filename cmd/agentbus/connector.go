package main

// The folder side in each mode. agentbus up starts its built-in connector
// beside its built-in runner, sharing the control plane's blob store on the
// same disk, so nothing is sent over HTTP and copies are made by clone or
// overlay (the local fast path). agentbus runner -folder does the same
// beside a server's runner. agentbus connector is the connector alone: it
// owns folders, takes their snapshots and applies changes into them, runs
// no agent, and keeps no record, gateway or API of its own.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/connector"
	"github.com/staysup/agentbus/folders"
	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/logfile"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/runner"
)

// connectorCapacity is how many snapshot and apply jobs a connector takes at
// once. Applies of one folder take turns (package connector).
const connectorCapacity = 4

// blobClient is the control plane's blob store over HTTP, with a runner's
// token: no overall timeout, since a blob is as big as a file.
func blobClient(server, token string) blobs.Client {
	return blobs.Client{URL: server, Token: token, HTTP: &http.Client{Transport: http.DefaultTransport}}
}

// connectorConfig is what starting a connector needs.
type connectorConfig struct {
	server, enrollToken, name string
	home                      string
	folders                   *folders.Registry
	// store, in agentbus up, is the control plane's own: the connector
	// shares it and sends nothing over HTTP. Nil: its own, under home,
	// with the server's behind it.
	store *blobs.Store
	logf  func(string, ...any)
}

// startConnector enrolls a connector and runs it until ctx ends. It answers
// the connector, for a runner in the same process to lend from.
func startConnector(ctx context.Context, cc connectorConfig, done func()) (*connector.Connector, error) {
	c := &connector.Connector{Folders: cc.folders, State: filepath.Join(cc.home, "connector"), Link: isolation.Clone}
	// A folder inside a session's overlay copy on this machine (a session
	// started in another's copy) is mounted again before it is read or
	// written: after a restart the copy's mount is gone.
	workdir := filepath.Join(cc.home, "worktrees")
	c.Prepare = func(path string) error {
		s, err := isolation.Load(workdir, path)
		if err != nil {
			return nil // not in a copy
		}
		return isolation.Remount(s)
	}
	remote := cc.store == nil
	if remote {
		st, err := blobs.Open(filepath.Join(cc.home, "blobs"))
		if err != nil {
			return nil, err
		}
		c.Store = st
	} else {
		c.Store = cc.store
	}
	rn, err := runner.Enroll(ctx, runner.Config{Queue: cc.server, EnrollToken: cc.enrollToken, Name: cc.name, Capacity: connectorCapacity,
		Role: queue.RoleConnector, FolderSide: connector.Jobs{Connector: c}, Folders: cc.folders, Logf: cc.logf,
		StateFile: filepath.Join(cc.home, "connector.json")})
	if err != nil {
		return nil, fmt.Errorf("the connector could not enroll with %s: %w", cc.server, err)
	}
	if remote {
		c.Remote = blobClient(cc.server, rn.Token())
	}
	cc.logf("connector.start id=%s connector=%s folders=%d", rn.ID(), cc.folders.Connector(), len(cc.folders.List(queue.MaxFolders)))
	go func() {
		defer done()
		rn.Run(ctx)
	}()
	return c, nil
}

// runConnector is agentbus connector: the connector alone, until a signal.
func runConnector(c config, own func(string, ...any)) int {
	if err := os.MkdirAll(c.home, 0o700); err != nil {
		own("data folder %s could not be made: %v", c.home, err)
		return 1
	}
	enroll, err := readTokenFile(c.enrollFile)
	if err != nil {
		own("%v", err)
		return 1
	}
	fr, err := folders.Open(filepath.Join(c.home, "folders.json"))
	if err != nil {
		own("%v", err)
		return 1
	}
	for _, f := range c.folders {
		if _, err := fr.Register(f); err != nil {
			own("-folder %s: %v", f, err)
			return 1
		}
	}
	if err := os.MkdirAll(filepath.Join(c.home, "logs"), 0o700); err != nil {
		own("%v", err)
		return 1
	}
	svcLog, err := logfile.Open(filepath.Join(c.home, "logs", "service.log"), c.logMax, nil)
	if err != nil {
		own("%v", err)
		return 1
	}
	defer svcLog.Close()
	name := c.runnerName
	if name == "" {
		name, _ = os.Hostname()
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	if _, err := startConnector(ctx, connectorConfig{server: strings.TrimSuffix(c.server, "/"), enrollToken: enroll, name: name,
		home: c.home, folders: fr, logf: svcLog.Printf}, func() { close(stopped) }); err != nil {
		cancel()
		own("%v", err)
		return 1
	}
	own("connector taking snapshot and apply jobs from %s for %d folder(s); data folder %s", c.server, len(c.folders), c.home)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	cancel()
	select {
	case <-stopped:
	case <-time.After(jobs.MaxWait + 5*time.Second):
	}
	return 0
}
