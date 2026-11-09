package main

// One program, three ways to run it:
//
//	agentbus up      the API, the job queue, the record, the gateway and a
//	                 runner, in one process: one person on one machine
//	agentbus server  the API, the job queue, the record and the gateway,
//	                 with no runner: runners on other machines pull from it
//	agentbus runner  a runner, with its own gateway and record, that takes
//	                 jobs from a server
//
// Every runner, the built-in one of up too, talks to its queue through the
// runner protocol over HTTP.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/staysup/agentbus/jobs"
	engine "github.com/staysup/agentbus/run"
)

// The modes. up is the zero value of config.mode.
const (
	modeUp     = ""
	modeServer = "server"
	modeRunner = "runner"
)

// Where a runner's agents send model calls (agentbus runner -gateway).
const (
	gatewayLocal  = "local"  // the runner's own gateway, on its machine
	gatewayServer = "server" // the server's gateway: not built yet
)

// hasQueue: the service keeps the job queue (up, server).
func (c config) hasQueue() bool { return c.mode != modeRunner }

// hasEngine: the service runs turns on this machine (up, runner).
func (c config) hasEngine() bool { return c.mode != modeServer }

func (c config) modeName() string {
	if c.mode == modeUp {
		return "up"
	}
	return c.mode
}

const usage = `usage: agentbus up     [-listen 127.0.0.1:8787] [-origin http://localhost:5173] [-replay=false] [-max-runs 4]
       agentbus server [-listen 127.0.0.1:8787] [-origin http://localhost:5173] [-replay=false]
       agentbus runner -server URL [-enroll-token-file F] [-gateway local] [-capacity 4] [-name NAME] [-listen 127.0.0.1:0] [-replay=false]
       agentbus api METHOD PATH [BODY]   (inside a turn: calls this service with the turn's token)
       agentbus version`

// parse reads a mode's command line into a config. It returns the exit code
// 2 and writes why to stderr when the line is not one it takes.
func parse(args []string, home string, stderr io.Writer) (config, bool) {
	var c config
	switch args[0] {
	case "up":
		c.mode = modeUp
	case modeServer, modeRunner:
		c.mode = args[0]
	default:
		fmt.Fprintln(stderr, usage)
		return c, false
	}
	name := "agentbus " + args[0]
	fl := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fl.SetOutput(stderr)
	listenDefault := "127.0.0.1:8787"
	if c.mode == modeRunner {
		// A runner's own service is for its agents only: any free port.
		listenDefault = "127.0.0.1:0"
	}
	listen := fl.String("listen", listenDefault, "address to listen on: an IP, with or without a port (8787 if none); 0.0.0.0 is reachable from the network, with the token guarding the API")
	replayOn := fl.Bool("replay", true, "continue claude and codex sessions by own replay (the gateway sends the earlier turn's history); false uses the vendor's resume")
	var origin *string
	if c.mode != modeRunner {
		origin = fl.String("origin", "", "lock the API to this one browser origin, such as http://localhost:5173; without it any origin may call the API, and the token still guards every call")
	}
	maxRuns := new(int)
	switch c.mode {
	case modeUp:
		fl.IntVar(maxRuns, "max-runs", engine.DefaultMaxRuns, "how many turns the built-in runner runs at once, 1 to 64; more wait in the queue")
	case modeRunner:
		fl.IntVar(maxRuns, "capacity", engine.DefaultMaxRuns, "how many jobs this runner runs at once, 1 to 64")
	}
	var server, enrollFile, runnerName, gatewayMode *string
	if c.mode == modeRunner {
		server = fl.String("server", "", "the server's URL, such as http://192.168.1.10:8787 (required)")
		enrollFile = fl.String("enroll-token-file", "", "the server's enroll_token, copied here with mode 0600; default <data folder>/enroll_token")
		runnerName = fl.String("name", "", "the runner's name; default the host name")
		gatewayMode = fl.String("gateway", gatewayLocal, "where agents send model calls: local (this runner's own gateway) or server (not built yet)")
	}
	if err := fl.Parse(args[1:]); err != nil {
		return c, false
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", name, fl.Arg(0))
		return c, false
	}
	if c.mode != modeServer && (*maxRuns < 1 || *maxRuns > jobs.MaxCapacity) {
		flagName := "-max-runs"
		if c.mode == modeRunner {
			flagName = "-capacity"
		}
		fmt.Fprintf(stderr, "%s: %s must be 1 to 64\n", name, flagName)
		return c, false
	}
	c.listen, c.noReplay, c.maxRuns = *listen, !*replayOn, *maxRuns
	if origin != nil {
		c.origin = *origin
	}
	if c.mode == modeRunner {
		u, err := url.Parse(*server)
		if *server == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fmt.Fprintf(stderr, "%s: -server must be the server's URL, such as http://192.168.1.10:8787\n", name)
			return c, false
		}
		switch *gatewayMode {
		case gatewayLocal:
		case gatewayServer:
			// TODO(central gateway): agents of a runner send model calls to
			// the server's gateway, which adds API keys on the way out and
			// keeps the record central, so runners hold no secret. It needs
			// the server's gateway to take calls from other machines, the
			// key store, and the runner's turns to name the server's base URL.
			fmt.Fprintf(stderr, "%s: -gateway server is not built yet; agents send model calls to this runner's own gateway (-gateway local)\n", name)
			return c, false
		default:
			fmt.Fprintf(stderr, "%s: -gateway must be local or server\n", name)
			return c, false
		}
		c.server, c.runnerName, c.gatewayMode = *server, *runnerName, *gatewayMode
		c.enrollFile = *enrollFile
		if c.enrollFile == "" {
			c.enrollFile = home + string(os.PathSeparator) + "enroll_token"
		}
	}
	return c, true
}

// readTokenFile reads a token file that must exist and only its owner may
// read, such as a server's enroll_token copied to a runner. Its value is
// never printed or put in an error.
func readTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("the token file %s does not exist; copy the server's enroll_token there with mode 0600", path)
	}
	if err != nil {
		return "", fmt.Errorf("the token file %s could not be read", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the token file %s can be read by others (mode %v); make it 0600", path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the token file %s could not be read", path)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("the token file %s is empty", path)
	}
	return tok, nil
}
