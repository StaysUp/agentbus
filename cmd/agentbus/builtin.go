package main

// The built-in runner of agentbus up runs as a process of its own, so a restart
// of up (an upgrade, many times a day while developing AgentBus) does not stop
// the turns it runs. It is a runner as one on another machine is: it takes jobs
// through the runner protocol, its agents call its own gateway, which relays to
// control, and it keeps a record of its own that it forwards. It shares up's
// data folder, all but that record (runner/events.db) and its log
// (logs/runner.log): the folders (folders.json, which both open), the session
// copies, the homes, the run logs and its identity (runner.json).
//
// up starts it and watches it (superviseRunner): when none runs, up starts one
// from its own binary. A runner already running, from an earlier up, is kept:
// its turns go on while control is away, and finish once it is back
// (runner/runner.go). It leaves on its own (watchRetire): when it has no job and
// control does not answer, so a stopped up leaves no process behind; and when up
// is started from another build, once its running jobs end, so the next runner
// is the new build. Jobs queued meanwhile wait for that next runner.
//
// A test of up in this process gets the same runner in this process
// (config.spawn false): the same code, its own record and log, no child.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/staysup/agentbus/gateway"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/runner"
)

// builtinEnv carries "<enrollment token> <build>" from up to the runner it
// starts. The runner takes it out of its environment at once, so no agent
// inherits it.
const builtinEnv = "AGENTBUS_BUILTIN_RUNNER"

// builtinDir holds, in up's data folder, what the built-in runner keeps
// apart: its record, and the build up wants (buildFile).
const builtinDir = "runner"

// buildFile names the build of the up now running: a runner of another build
// retires.
const buildFile = "build"

// builtinRunner is what makes a runner up's built-in one.
type builtinRunner struct {
	enroll string // up's enrollment token, made new at each start of up
	build  string // the build of the up that started it (buildOf)
	// process: it runs as a process of its own (builtinEnv), so it leaves on
	// its own (watchRetire). One in a test's process does not.
	process bool
}

// builtinConfig is the config of up's built-in runner, for control at server:
// a runner in up's data folder, as many turns at once as up's -max-runs.
func (c config) builtinConfig(server, enroll, build string) config {
	rc := c
	rc.mode, rc.server = modeRunner, server
	rc.listen = "127.0.0.1:0"
	rc.origin, rc.hosts, rc.folders, rc.spawn = "", nil, nil, false
	rc.builtin = &builtinRunner{enroll: enroll, build: build}
	return rc
}

// storePath is the record's file: up's built-in runner keeps its own beside
// control's.
func (c config) storePath() string {
	if c.builtin != nil {
		return filepath.Join(c.home, builtinDir, "events.db")
	}
	return filepath.Join(c.home, "events.db")
}

// logName is the service log's file in logs/.
func (c config) logName() string {
	if c.builtin != nil {
		return "runner.log"
	}
	return "service.log"
}

// runnerOptions names the options up starts its runner with (spawnRunner),
// as part of its build: an up started again with other options is another
// build, so the runner it finds retires for one of its own (review
// t_f1724c4b98986fcb P2: up started again with -replay=false kept replay).
func runnerOptions(c config) string {
	if c.noReplay {
		return "+replay=false"
	}
	return ""
}

// buildOf names the program at path by its content, so a rebuild in place
// (go build over the installed binary) is another build. "" when it cannot be
// read.
func buildOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// readBuiltinEnv takes builtinEnv out of the environment and answers it.
func readBuiltinEnv(getenv func(string) string) *builtinRunner {
	v := getenv(builtinEnv)
	if v == "" {
		return nil
	}
	os.Unsetenv(builtinEnv)
	enroll, build, _ := strings.Cut(v, " ")
	return &builtinRunner{enroll: enroll, build: build, process: true}
}

// seedRunnerRecord gives a built-in runner that has no record yet a copy of
// control's: the sessions it continues have their history there (own replay,
// a harness's resume, an earlier turn's agent), as when up ran the runner in
// its own process. Once only: from then on the runner records its turns
// itself and forwards them to control.
func (s *service) seedRunnerRecord(home string) error {
	dir := filepath.Join(home, builtinDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the runner's folder %s could not be made: %w", dir, err)
	}
	p := filepath.Join(dir, "events.db")
	if _, err := os.Stat(p); err == nil || !os.IsNotExist(err) {
		return nil
	}
	if err := s.store.CopyTo(p); err != nil {
		return fmt.Errorf("the runner's record could not be made: %w", err)
	}
	s.log.Printf("runner.record_seeded")
	return nil
}

// startBuiltin starts up's built-in runner for control at server: watched,
// as a process of its own (superviseRunner), or in this process for a test.
func (s *service) startBuiltin(c config, server, enroll string) error {
	if err := s.seedRunnerRecord(c.home); err != nil {
		return err
	}
	build := ""
	if c.self != "" {
		build = buildOf(c.self) + runnerOptions(c)
	}
	rc := c.builtinConfig(server, enroll, build)
	if !c.spawn {
		in, err := start(rc)
		if err != nil {
			return err
		}
		s.inRunner = in
		return nil
	}
	if err := os.WriteFile(filepath.Join(c.home, builtinDir, buildFile), []byte(build), 0o600); err != nil {
		return fmt.Errorf("the runner's build could not be written: %w", err)
	}
	stop := make(chan struct{})
	s.stopSupervise = func() { close(stop) }
	go s.superviseRunner(rc, stop)
	return nil
}

// superviseRunner starts the built-in runner whenever none runs, until stop
// closes. One runs while its record's lock is held. Stopping up does not stop
// it.
func (s *service) superviseRunner(rc config, stop <-chan struct{}) {
	lock := rc.storePath() + ".lock"
	said := false
	for {
		switch {
		case !held(lock):
			if err := s.spawnRunner(rc); err != nil {
				s.log.Printf("runner.spawn_failed error=%q", err.Error())
			}
			said = false
		case !said:
			s.log.Printf("runner.running")
			said = true
		}
		select {
		case <-stop:
			return
		case <-time.After(time.Second):
		}
	}
}

// held says another process holds the lock at path.
func held(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false
	}
	defer f.Close() // closing releases a lock taken here
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

// spawnRunner starts the built-in runner from this program, in a session of
// its own, so the person's Ctrl-C to up and the terminal closing reach up
// only. Its own lines go to logs/runner.out.
func (s *service) spawnRunner(rc config) error {
	// No -capacity: the settings decide, as for any runner ([limits]).
	args := []string{modeRunner, "-server", rc.server}
	if rc.noReplay {
		args = append(args, "-replay=false")
	}
	cmd := exec.Command(rc.self, args...)
	cmd.Env = append(os.Environ(), "AGENTBUS_HOME="+rc.home, builtinEnv+"="+rc.builtin.enroll+" "+rc.builtin.build)
	out, err := os.OpenFile(filepath.Join(rc.home, "logs", "runner.out"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.log.Printf("runner.spawned pid=%d", cmd.Process.Pid)
	go func() { _ = cmd.Wait() }() // while up runs, it reaps its child
	// The runner takes its record's lock as it starts: until then it is not
	// seen as running, so wait a moment rather than start a second one.
	for range 50 {
		if held(rc.storePath() + ".lock") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// retireEvery is how often the built-in runner checks whether to leave.
const retireEvery = 500 * time.Millisecond

// watchRetire closes retire, once, when up's built-in runner should leave:
// control is away and no agent of its runs, or up now runs another build, or
// the same build with other options (runnerOptions). The
// runner then takes no more jobs and leaves once its agents ended. A turn's end
// it could not send while control is away is kept on disk for the next runner
// (runner/pending.go), so it never waits on control to leave.
func (s *service) watchRetire(c config, agents, busy func() int, away func() time.Duration, retire chan struct{}) {
	file := filepath.Join(c.home, builtinDir, buildFile)
	for {
		time.Sleep(retireEvery)
		if away() > 0 && agents() == 0 {
			s.log.Printf("runner.retiring why=control_away running=%d", busy())
			close(retire)
			return
		}
		if b, err := os.ReadFile(file); err == nil && len(b) > 0 && string(b) != c.builtin.build {
			s.log.Printf("runner.retiring why=new_build running=%d", busy())
			close(retire)
			return
		}
	}
}

// finishesDir holds, in a runner's data folder, the turns' ends waiting for
// control (runner.Config.Finishes).
const finishesDir = "finishes"

// turnGate is a runner's gateway entrance check, for every runner, up's
// built-in one too: a call's path must name a turn the runner runs now, and
// the call must carry that turn's stand-in. Else it is refused before the
// gateway holds or records it, and nothing goes to control. It adds no
// credential: the stand-in goes on to control's relay, which swaps it for the
// real one. (Before 0.6.7 up made this check in its own process, in
// jobs.TurnGate; a runner on another machine left it to the relay, which binds
// a call to its stand-in's turn whatever its path.) Nil runner (not enrolled
// yet): every call is refused.
type turnGate struct {
	mu sync.RWMutex
	rn *runner.Runner
}

func (g *turnGate) set(rn *runner.Runner) {
	g.mu.Lock()
	g.rn = rn
	g.mu.Unlock()
}

// Why a runner's gateway refuses a call, in fixed words.
const (
	gateNoTurn    = "the call carries no stand-in of a turn running now; control adds credentials only to a running turn's calls"
	gateOtherTurn = "the call's stand-in is another turn's than its path names; control adds a credential only to a turn's own calls"
)

func (g *turnGate) credential() gateway.Credential {
	return gateway.Credential{
		Turn: func(r *http.Request, session, turn string) *gateway.Refusal {
			g.mu.RLock()
			rn := g.rn
			g.mu.RUnlock()
			if rn == nil {
				return gateRefusal(http.StatusUnauthorized, gateNoTurn)
			}
			switch own, known := rn.CallOwner(session, turn, callToken(r)); {
			case !known:
				return gateRefusal(http.StatusUnauthorized, gateNoTurn)
			case !own:
				return gateRefusal(http.StatusForbidden, gateOtherTurn)
			}
			return nil
		},
		Read: func(*http.Request) (func(http.Header), gateway.Signin, *gateway.Refusal) {
			return nil, gateway.Signin{}, nil
		},
	}
}

func gateRefusal(status int, why string) *gateway.Refusal {
	b, _ := json.Marshal(jobs.JobError{Error: why})
	return &gateway.Refusal{Status: status, ContentType: "application/json", Body: b}
}

// callToken is the stand-in a model call carries: as "Authorization: Bearer",
// or, for agy on the Gemini API, in X-Goog-Api-Key, as control's relay reads
// it (jobs/relay.go).
func callToken(r *http.Request) string {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return tok
	}
	return r.Header.Get("X-Goog-Api-Key")
}
