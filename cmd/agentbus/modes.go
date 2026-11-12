package main

// One program, three ways to run it:
//
//	agentbus up      the API, the job queue, the record, the gateway and a
//	                 runner, in one process: one person on one machine
//	agentbus server  the API, the job queue, the record, the gateway and the
//	                 blob store, with no runner: the runners of other
//	                 machines pull from it
//	agentbus runner  the one program of a machine that is not control: it
//	                 holds the machine's folders, makes each session's copy
//	                 from its folder, runs its turns, and applies changes
//	                 back into the folder
//
// agentbus connector, a machine's folders as a program of their own, was
// retired in 0.5: it now says what to run instead (connectorRetired).
//
// Every runner, the built-in one of up too, talks to its queue through the
// runner protocol over HTTP. File data goes through the server's blob store;
// in up the two share it on disk.

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	engine "github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/runner"
	"github.com/staysup/agentbus/sandbox"
)

// The modes. up is the zero value of config.mode.
const (
	modeUp     = ""
	modeServer = "server"
	modeRunner = "runner"
	// modeConnector is no mode since 0.5: parse refuses it.
	modeConnector = "connector"
)

// Where a runner's agents' model calls get their credential (agentbus runner
// -gateway). The runner's own gateway records them either way.
const (
	// gatewayServer: the runner holds no secret. Its gateway sends each call
	// on to the server's, which adds the credential (jobs/relay.go).
	gatewayServer = "server"
	// gatewayLocal: the call goes to the vendor with the sign-in or key of
	// the runner's own machine, as before.
	gatewayLocal = "local"
)

// noSecrets: a process that runs turns whose agents' credentials come from
// control: a runner of a server, through the server's relay, and agentbus
// up, through its own. Only a runner with -gateway local uses its machine's
// own sign-in and keys.
func (c config) noSecrets() bool { return c.hasEngine() && c.gatewayMode != gatewayLocal }

// canSandbox is this process's sandbox report: whether a session's wall can
// be built here, and why not. A runner that gives sessions no home of its
// own (-gateway local: noSecrets is false) cannot, whatever the machine
// can, since the wall needs the session's home: every walled turn would be
// refused (engine.NoHomes), so it says so before any turn.
func (c config) canSandbox() (bool, string) {
	if !c.noSecrets() {
		return false, engine.NoHomes
	}
	return sandbox.Can()
}

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
       agentbus runner -server URL [-gateway server|local] [-capacity 4] [-name NAME] [-folder DIR]... [-labels a,b] [-listen 127.0.0.1:0] [-replay=false]
       agentbus login URL [runner's flags]   (on a machine, once: sign it in to the control at URL; approve it on the Runners page)
       agentbus version

For agents and scripts:
       agentbus api METHOD PATH [BODY]   (inside a turn: calls this service with the turn's token)`

// connectRemoved is what agentbus connect says since 0.5: the cockpit's
// Connect keeps a subscription's sign-in on control instead.
const connectRemoved = "agentbus connect was removed in 0.5: connect a subscription in the cockpit (Agents, the agent's Connect step)."

// parse reads a mode's command line into a config. It returns the exit code
// 2 and writes why to stderr when the line is not one it takes.
func parse(args []string, home string, stderr io.Writer) (config, bool) {
	var c config
	switch args[0] {
	case "up":
		c.mode = modeUp
	case modeServer, modeRunner:
		c.mode = args[0]
	case modeConnector:
		fmt.Fprintln(stderr, connectorRetired(args[1:]))
		return c, false
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
	var hostList folderFlags
	if c.mode != modeRunner {
		origin = fl.String("origin", "", "lock the API to this one browser origin, such as http://localhost:5173; without it any origin may call the API, and the token still guards every call")
		fl.Var(&hostList, "host", "another name the API answers to on loopback, such as host.lima.internal, the name a Lima VM reaches this machine's loopback by; repeat it for each one")
	}
	maxRuns := new(int)
	switch c.mode {
	case modeUp:
		fl.IntVar(maxRuns, "max-runs", engine.DefaultMaxRuns, "how many turns the built-in runner runs at once, 1 to 64; more wait in the queue")
	case modeRunner:
		fl.IntVar(maxRuns, "capacity", engine.DefaultMaxRuns, "how many jobs this runner runs at once, 1 to 64")
	}
	var server, runnerName, gatewayMode, labels *string
	var folderList folderFlags
	if c.mode == modeRunner {
		fl.Var(&folderList, "folder", "a folder on this machine that sessions may run in, by its absolute path; repeat it for each one. The server knows it only by its reference (GET /api/v0/folders). A folder a person types for this machine is added when its session starts, so none is required")
		labels = fl.String("labels", "", "words a job can ask for in spec.labels, comma-separated, such as gpu,docker; default agentbus")
		server = fl.String("server", "", "the server's URL, such as http://192.168.1.10:8787 (required)")
		runnerName = fl.String("name", "", "the runner's name; default the host name")
		gatewayMode = fl.String("gateway", gatewayServer, "where agents' model calls get their credential: server (the server's gateway adds it; this runner holds no key or sign-in) or local (this machine's own sign-in and keys)")
	}
	if c.mode == modeRunner {
		for _, a := range args[1:] {
			if a == "-enroll-token-file" || strings.HasPrefix(a, "-enroll-token-file=") || strings.HasPrefix(a, "--enroll-token-file") {
				fmt.Fprintln(stderr, enrollFileRemoved)
				return c, false
			}
		}
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
		c.hosts = hostList
	}
	if c.mode == modeRunner {
		u, err := url.Parse(*server)
		if *server == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fmt.Fprintf(stderr, "%s: -server must be the server's URL, such as http://192.168.1.10:8787\n", name)
			return c, false
		}
		switch *gatewayMode {
		case gatewayLocal, gatewayServer:
		default:
			fmt.Fprintf(stderr, "%s: -gateway must be local or server\n", name)
			return c, false
		}
		c.server, c.runnerName, c.gatewayMode = *server, *runnerName, *gatewayMode
		for _, f := range folderList {
			if !filepath.IsAbs(f) || len(f) > 4096 {
				fmt.Fprintf(stderr, "%s: -folder must be an absolute path\n", name)
				return c, false
			}
		}
		c.folders = folderList
		if *labels != "" {
			for _, l := range strings.Split(*labels, ",") {
				if !queue.LabelOK(l) {
					fmt.Fprintf(stderr, "%s: each of -labels is 1 to 64 of A-Z, a-z, 0-9 and . _ : -, starting with a letter or digit\n", name)
					return c, false
				}
				c.labels = append(c.labels, l)
			}
		}
	}
	return c, true
}

// enrollFileRemoved is what agentbus runner says to -enroll-token-file
// since 0.5.1: control writes no enrollment token, and a machine joins only
// by agentbus login, which the person approves.
const enrollFileRemoved = "agentbus runner: -enroll-token-file was removed in 0.5.1: sign this machine in with agentbus login URL, and approve it on control's Runners page"

// folderFlags is -folder, given once per folder.
type folderFlags []string

func (f *folderFlags) String() string     { return strings.Join(*f, ",") }
func (f *folderFlags) Set(v string) error { *f = append(*f, v); return nil }

// connectorRetired is what agentbus connector says since 0.5: what to run
// instead, with the flags it was given. agentbus runner takes each of them.
// Nothing is changed or removed: a runner started on the same data folder
// keeps this machine's id and its folders' references, so the sessions and
// applies that name them reach it.
func connectorRetired(args []string) string {
	line := "agentbus runner"
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		line += " " + a
	}
	return "agentbus connector was retired in 0.5: one program, agentbus runner, runs on each machine, holds its folders, runs their sessions and applies their changes.\n" +
		"Run instead, with the same data folder (it keeps this machine's id and its folders' references):\n\n    " + line + "\n\n" +
		"If this data folder is also control's (agentbus server's), give the runner one of its own: set AGENTBUS_HOME to a new folder and copy folders.json into it first. " +
		"The runner also runs the agents of every session in this machine's folders, so the agents' programs must be installed here. " +
		"Nothing was changed. See Upgrading to 0.5 in README.md."
}

// signedIn says the data folder holds a runner's identity (runner.json), as
// an enroll or agentbus login saved it.
func signedIn(home string) bool { return runner.HasIdentity(filepath.Join(home, runnerState)) }

// runnerState is the runner's identity in its data folder.
const runnerState = "runner.json"
