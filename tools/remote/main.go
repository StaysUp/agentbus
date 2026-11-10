// Command remote runs one complete remote path, across two machines: the
// control plane (agentbus server) and a connector on this Mac, a runner in
// a Lima VM. A stand-in agent edits files, so no model and no credential is
// involved. It checks every step and prints how long each took:
//
//	new session on the connector's folder → snapshot → blobs to control →
//	the VM runner builds its copy from them → turn → checkpoint and
//	changeset back → the server's record shows the turn → apply up to turn
//	N on the Mac (all or nothing) → the Mac folder has the change; then a
//	refused apply over the person's own edit, with nothing written.
//
// Then it measures snapshot transfer for folders of each size. At the end it
// stops what it started and removes the scratch folders, on both machines.
//
//	go run ./tools/remote
//
// See tools/remote/README.md.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

// standIn is claude for the runner. A turn appends its prompt to a.txt, adds
// new.txt when it is missing and removes it when it is there, and prints the
// result line claude prints. It answers the version and sign-in probes.
const standIn = `#!/bin/sh
case "$1" in
--version) echo "0.0.0 (agentbus remote stand-in)"; exit 0 ;;
auth) exit 0 ;;
esac
for a; do last=$a; done
echo "$last" >> a.txt
if [ -e new.txt ]; then rm new.txt; else echo "$last" > new.txt; fi
echo '{"type":"result","subtype":"success","result":"done","session_id":"remote-'$$'"}'
`

type options struct {
	vm, vmDir, dir string
	port           int
	runs           int
	sizes          []int
	keep, hold     bool
	real, connect  bool
	keyEnv, model  string
}

func main() {
	var o options
	var sizes string
	flag.StringVar(&o.vm, "vm", "agentbus-dev", "the Lima VM the runner runs in")
	flag.StringVar(&o.vmDir, "vm-dir", "/work/remote-runner", "the runner's scratch folder in the VM: made, and removed at the end")
	flag.StringVar(&o.dir, "dir", "", "the scratch folder on this Mac, new or empty (default: a new temp folder); removed at the end")
	flag.IntVar(&o.port, "port", 8797, "the server's port on 127.0.0.1")
	flag.IntVar(&o.runs, "runs", 3, "how many times the path runs, each in a new session")
	flag.StringVar(&sizes, "sizes", "300,3000,50000", "folder sizes, in files, whose snapshot transfer is measured; empty for none")
	flag.BoolVar(&o.keep, "keep", false, "keep the scratch folders")
	flag.BoolVar(&o.hold, "hold", false, "after the checks, keep everything running until Ctrl-C (to look at it in a cockpit)")
	flag.BoolVar(&o.real, "real", false, "after the checks, run the VM's real claude on a runner that holds no secret: an OpenRouter agent, then a subscription agent")
	flag.BoolVar(&o.connect, "connect", false, "with -real: first connect your claude subscription on the scratch server (runs agentbus connect claude here)")
	flag.StringVar(&o.keyEnv, "key-env", "OPENROUTER_API_KEY", "with -real: the variable the server reads the OpenRouter key from, by name")
	flag.StringVar(&o.model, "model", "deepseek/deepseek-v4.1-flash", "with -real: the OpenRouter agent's model")
	flag.Parse()
	for _, s := range strings.Split(sizes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "remote: -sizes: %q is not a number of files\n", s)
				os.Exit(2)
			}
			o.sizes = append(o.sizes, n)
		}
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "remote:", err)
		os.Exit(1)
	}
}

// world is everything the run started, to stop at the end.
type world struct {
	o              options
	dir            string
	server, conn   *proc
	api            *client
	serverLog      string
	vmStarted      bool
	ref, connector string
	folders        map[string]string // folder name → ref
	skewLo, skewHi time.Duration     // VM clock minus Mac clock, bounds
	interrupted    chan os.Signal
	released       chan struct{}
	cleaned        sync.Once
}

func run(o options) (err error) {
	w := &world{o: o, folders: map[string]string{}, interrupted: make(chan os.Signal, 1), released: make(chan struct{})}
	signal.Notify(w.interrupted, os.Interrupt, syscall.SIGTERM)
	holding := make(chan struct{})
	go func() {
		<-w.interrupted
		select {
		case <-holding: // -hold is waiting for this
			close(w.released)
		default:
			fmt.Fprintln(os.Stderr, "remote: interrupted; stopping what it started")
			w.cleanup()
			os.Exit(130)
		}
	}()
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", o.port)); err != nil {
		return fmt.Errorf("port %d is in use: pick another with -port", o.port)
	} else {
		ln.Close()
	}
	if w.dir = o.dir; w.dir == "" {
		if w.dir, err = os.MkdirTemp("", "agentbus-remote-"); err != nil {
			return err
		}
	} else if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	defer w.cleanup()
	if err := w.setup(); err != nil {
		return err
	}
	if o.hold {
		defer func() {
			fmt.Printf("\nholding: server http://127.0.0.1:%d, token in %s. Ctrl-C ends it.\n", o.port, filepath.Join(w.dir, "server", "token"))
			close(holding)
			<-w.released
		}()
	}
	var runs []phases
	for i := range o.runs {
		p, err := w.path(i)
		if err != nil {
			return fmt.Errorf("run %d: %w", i+1, err)
		}
		runs = append(runs, p...)
	}
	w.report(runs)
	if len(o.sizes) > 0 {
		var tr []transfer
		for _, n := range o.sizes {
			t, err := w.transfer(n)
			if err != nil {
				return fmt.Errorf("transfer of %d files: %w", n, err)
			}
			tr = append(tr, t...)
		}
		reportTransfer(tr)
	}
	if o.real {
		if err := w.real(); err != nil {
			return fmt.Errorf("real: %w", err)
		}
	}
	return nil
}

// setup builds both binaries, writes the test project and the fixtures,
// and starts the server and the connector on this Mac and the runner in
// the VM.
func (w *world) setup() error {
	step := clock("setup")
	defer step()
	bin := filepath.Join(w.dir, "agentbus")
	if err := sh("", nil, "go", "build", "-o", bin, "./cmd/agentbus"); err != nil {
		return fmt.Errorf("build (run this from the framework's folder): %w", err)
	}
	linux := filepath.Join(w.dir, "agentbus-linux")
	arch, err := lima(w.o.vm, "uname -m")
	if err != nil {
		return fmt.Errorf("the VM %s cannot be reached: %w", w.o.vm, err)
	}
	goarch := map[string]string{"aarch64": "arm64", "x86_64": "amd64"}[strings.TrimSpace(arch)]
	if goarch == "" {
		return fmt.Errorf("the VM's architecture %q is not supported", strings.TrimSpace(arch))
	}
	if err := sh("", []string{"GOOS=linux", "GOARCH=" + goarch, "CGO_ENABLED=0"}, "go", "build", "-o", linux, "./cmd/agentbus"); err != nil {
		return err
	}
	// The person's folder: a small git project.
	proj := filepath.Join(w.dir, "project")
	for p, c := range map[string]string{"a.txt": "one\n", "b.txt": "keep\n", "src/main.go": "package main\n\nfunc main() {}\n", "README.md": "# test project\n"} {
		if err := os.MkdirAll(filepath.Join(proj, filepath.Dir(p)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(proj, p), []byte(c), 0o644); err != nil {
			return err
		}
	}
	if err := sh(proj, nil, "git", "init", "-q", "-b", "main"); err != nil {
		return err
	}
	if err := sh(proj, nil, "git", "add", "."); err != nil {
		return err
	}
	if err := sh(proj, nil, "git", "-c", "user.name=remote", "-c", "user.email=remote@localhost", "commit", "-q", "-m", "one"); err != nil {
		return err
	}
	folderArgs := []string{"-folder", proj}
	for _, n := range w.o.sizes {
		f := filepath.Join(w.dir, fmt.Sprintf("files-%d", n))
		if err := fixture(f, n); err != nil {
			return err
		}
		folderArgs = append(folderArgs, "-folder", f)
	}
	// The server, answering the VM by host.lima.internal, and the connector.
	srvHome, conHome := filepath.Join(w.dir, "server"), filepath.Join(w.dir, "connector")
	addr := fmt.Sprintf("127.0.0.1:%d", w.o.port)
	w.serverLog = filepath.Join(srvHome, "logs", "service.log")
	if w.server, err = start(bin, srvHome, filepath.Join(w.dir, "server.out"), "server", "-listen", addr, "-host", "host.lima.internal", "-replay=false"); err != nil {
		return err
	}
	if w.api, err = waitService("http://"+addr, srvHome, w.server); err != nil {
		return err
	}
	enroll := filepath.Join(srvHome, "enroll_token")
	if w.conn, err = start(bin, conHome, filepath.Join(w.dir, "connector.out"),
		append([]string{"connector", "-server", "http://" + addr, "-enroll-token-file", enroll, "-name", "mac"}, folderArgs...)...); err != nil {
		return err
	}
	// The runner in the VM: its own folder, the stand-in on its PATH, and
	// an environment of nothing but these, so no key or sign-in of the VM's
	// user reaches it.
	d := w.o.vmDir
	if out, err := lima(w.o.vm, "test ! -e "+d+" && mkdir -p "+d+"/bin "+d+"/home && chmod 700 "+d); err != nil {
		return fmt.Errorf("%s in the VM exists already or cannot be made: %s", d, out)
	}
	w.vmStarted = true
	standInFile := filepath.Join(w.dir, "claude")
	if err := os.WriteFile(standInFile, []byte(standIn), 0o755); err != nil {
		return err
	}
	for src, dst := range map[string]string{linux: d + "/bin/agentbus", standInFile: d + "/bin/claude", enroll: d + "/home/enroll_token"} {
		if err := sh("", nil, "limactl", "copy", src, w.o.vm+":"+dst); err != nil {
			return err
		}
	}
	// Only the runner goes to the background, with every output to its
	// file: what holds the shell's output open holds the ssh call open.
	runCmd := "cd " + d + " && chmod 755 bin/agentbus bin/claude && chmod 600 home/enroll_token || exit 1; " +
		"env -i HOME=" + d + "/home PATH=" + d + "/bin:/usr/bin:/bin AGENTBUS_HOME=" + d + "/home " +
		"setsid nohup bin/agentbus runner -server http://host.lima.internal:" + strconv.Itoa(w.o.port) +
		" -enroll-token-file home/enroll_token -name vm -replay=false > runner.out 2>&1 < /dev/null & echo $! > runner.pid"
	if out, err := lima(w.o.vm, runCmd); err != nil {
		return fmt.Errorf("the runner did not start in the VM: %s", out)
	}
	// The connector's folders, by reference, once it reports them, and the
	// VM runner once it has enrolled.
	if err := waitFor(60*time.Second, "the connector's folders", func() bool {
		var fs struct {
			Folders []struct{ Ref, Name, Connector string }
		}
		w.api.call("GET", "/api/v0/folders", nil, &fs)
		for _, f := range fs.Folders {
			w.folders[f.Name] = f.Ref
			w.connector = f.Connector
		}
		return len(w.folders) == 1+len(w.o.sizes)
	}); err != nil {
		return err
	}
	w.ref = w.folders["project"]
	if err := waitFor(60*time.Second, "the VM runner", func() bool {
		out, _ := lima(w.o.vm, "grep -c 'runner taking jobs' "+d+"/runner.out")
		return strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0"
	}); err != nil {
		out, _ := lima(w.o.vm, "cat "+d+"/runner.out")
		return fmt.Errorf("%w: %s", err, out)
	}
	w.skewLo, w.skewHi = skew(w.o.vm)
	fmt.Printf("VM clock minus Mac clock: %s to %s\n", w.skewLo.Round(time.Millisecond), w.skewHi.Round(time.Millisecond))
	return nil
}

// phases is one turn's times, in ms, by name.
type phases struct {
	name string
	ms   map[string]float64
}

// path runs the whole path once: two turns of a new session, its checks,
// apply up to the second turn, and a refused apply in a second session.
func (w *world) path(i int) ([]phases, error) {
	proj := filepath.Join(w.dir, "project")
	// The folder as each run starts.
	if err := os.WriteFile(filepath.Join(proj, "a.txt"), []byte("one\n"), 0o644); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(proj, "new.txt"))
	body := map[string]any{"agent": "claude", "folder_ref": w.ref, "connector": w.connector, "isolate": true}
	t1, p1, err := w.turn(merge(body, map[string]any{"prompt": "p1"}), "first turn")
	if err != nil {
		return nil, err
	}
	t2, p2, err := w.turn(map[string]any{"session": t1.ID, "prompt": "p2"}, "later turn")
	if err != nil {
		return nil, err
	}
	// The person's folder is untouched until an apply.
	if got := text(filepath.Join(proj, "a.txt")) + text(filepath.Join(proj, "new.txt")); got != "one\n<missing>" {
		return nil, fmt.Errorf("the folder changed before any apply: %q", got)
	}
	// What the cockpit reads, from the server: the session, both turns
	// ended, their prompts, and each turn's diff.
	t0 := time.Now()
	if err := w.cockpitView(t1.ID, []started{t1, t2}); err != nil {
		return nil, err
	}
	view := ms(time.Since(t0))
	// Apply up to the later turn: both turns, in one step.
	t0 = time.Now()
	code, a := w.api.apply(t2.Turn)
	applyMS := ms(time.Since(t0))
	if code != 200 || a.State != "applied" {
		return nil, fmt.Errorf("apply up to the later turn: %d %+v", code, a)
	}
	if got := text(filepath.Join(proj, "a.txt")) + text(filepath.Join(proj, "new.txt")); got != "one\np1\np2\n<missing>" {
		return nil, fmt.Errorf("after the apply: %q", got)
	}
	// Asked again: the same apply, nothing written.
	if code, a := w.api.apply(t2.Turn); code != 200 || !a.Already {
		return nil, fmt.Errorf("the apply asked again: %d %+v", code, a)
	}
	// A new session; the person edits a.txt before the apply: refused, with
	// the file and why, and nothing written.
	t3, _, err := w.turn(merge(body, map[string]any{"prompt": "p3"}), "")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(proj, "a.txt"), []byte("mine\n"), 0o644); err != nil {
		return nil, err
	}
	t0 = time.Now()
	code, a = w.api.apply(t3.Turn)
	refuseMS := ms(time.Since(t0))
	if code != http.StatusConflict || a.State != "refused" || len(a.Conflicts) != 1 || a.Conflicts[0].Path != "a.txt" {
		return nil, fmt.Errorf("the apply over the person's edit: %d %+v", code, a)
	}
	if got := text(filepath.Join(proj, "a.txt")) + text(filepath.Join(proj, "new.txt")); got != "mine\n<missing>" {
		return nil, fmt.Errorf("after the refused apply: %q", got)
	}
	p1.ms["cockpit view (both turns)"] = view
	p1.ms["apply up to turn 2"] = applyMS
	p1.ms["apply refused"] = refuseMS
	fmt.Printf("run %d: session %s, turns %s and %s applied; %s refused over a.txt\n", i+1, t1.ID, t1.Turn, t2.Turn, t3.Turn)
	return []phases{p1, p2}, nil
}

type started struct{ ID, Turn, Job string }

// turn starts a turn, waits for its job to end finished, and reads its
// phases from the job, its attempt's events and the snapshot job.
func (w *world) turn(body map[string]any, name string) (started, phases, error) {
	var s started
	t0 := time.Now()
	if code, out := w.api.call("POST", "/api/v0/turns", body, &s); code/100 != 2 || s.Job == "" {
		return s, phases{}, fmt.Errorf("POST /api/v0/turns: %d %s", code, out)
	}
	var j job
	if err := waitFor(5*time.Minute, "the turn's job", func() bool {
		j = job{}
		w.api.call("GET", "/api/v0/jobs/"+s.Job, nil, &j)
		return j.Ended != nil
	}); err != nil {
		return s, phases{}, err
	}
	total := time.Since(t0)
	if j.State != "finished" || len(j.Attempts) == 0 {
		return s, phases{}, fmt.Errorf("job %s ended %s %s", s.Job, j.State, j.Reason)
	}
	s.Turn = j.Attempt
	p := phases{name: name, ms: map[string]float64{"total (client)": ms(total)}}
	at := j.Attempts[len(j.Attempts)-1]
	p.ms["queue (created → taken)"] = ms(at.Taken.Sub(j.Created))
	ev, snapID, err := w.api.events(s.Turn)
	if err != nil {
		return s, p, err
	}
	gap := func(label, from, to string) {
		if a, ok := ev[from]; ok {
			if b, ok := ev[to]; ok {
				p.ms[label] = ms(b.Sub(a))
			}
		}
	}
	if snap, ok := w.api.snapshotJob(snapID); ok {
		p.ms["snapshot (connector: hash, send)"] = ms(snap.Ended.Sub(snap.Created))
		gap("copy (runner: fetch, build)", "workspace.snapshot", "workspace.ready")
	} else {
		gap("copy", "attempt.started", "workspace.ready")
	}
	start := "workspace.ready"
	if _, ok := ev[start]; !ok {
		start = "attempt.started"
	}
	gap("start (to agent started)", start, "turn.created")
	gap("agent (stand-in)", "turn.created", "turn.ended")
	gap("checkpoint (end, diff)", "turn.ended", "workspace.changes")
	if c, ok := ev["workspace.changes"]; ok {
		// Two clocks: the VM's last event, the server's end. Corrected by
		// the middle of the skew's bounds.
		p.ms["finish (changeset, record; two clocks)"] = ms(j.Ended.Sub(c.Add(-(w.skewLo + w.skewHi) / 2)))
	}
	return s, p, nil
}

// cockpitView reads what the cockpit reads, from the server, and checks it
// holds the whole of each turn: GET /api/v0/sessions, /turns, /turns/{id},
// /turns/{id}/diff.
func (w *world) cockpitView(session string, turns []started) error {
	var ss struct {
		Sessions []struct{ ID string }
	}
	w.api.call("GET", "/api/v0/sessions", nil, &ss)
	if !slices.ContainsFunc(ss.Sessions, func(s struct{ ID string }) bool { return s.ID == session }) {
		return fmt.Errorf("GET /api/v0/sessions does not list session %s", session)
	}
	var list struct {
		Turns []struct {
			ID, State, Text string
		}
	}
	if code, out := w.api.call("GET", "/api/v0/turns?session="+session, nil, &list); code != 200 || len(list.Turns) != len(turns) {
		return fmt.Errorf("GET /api/v0/turns: %d %s", code, out)
	}
	want := []map[string]string{
		{"a.txt": "modified", "new.txt": "added"},
		{"a.txt": "modified", "new.txt": "deleted"},
	}
	for i, t := range turns {
		var one struct {
			Turn struct{ State string }
		}
		if code, out := w.api.call("GET", "/api/v0/turns/"+t.Turn, nil, &one); code != 200 || one.Turn.State != "finished" {
			return fmt.Errorf("GET /api/v0/turns/%s: %d %s", t.Turn, code, out)
		}
		var d struct {
			State string
			Files *struct {
				Isolated bool
				Changes  []struct{ Path, Change string }
			}
		}
		if code, out := w.api.call("GET", "/api/v0/turns/"+t.Turn+"/diff", nil, &d); code != 200 || d.Files == nil || !d.Files.Isolated {
			return fmt.Errorf("GET /api/v0/turns/%s/diff: %d %s", t.Turn, code, out)
		}
		got := map[string]string{}
		for _, c := range d.Files.Changes {
			got[c.Path] = c.Change
		}
		if i < len(want) && fmt.Sprint(got) != fmt.Sprint(want[i]) {
			return fmt.Errorf("turn %d's diff %v, want %v", i+1, got, want[i])
		}
	}
	return nil
}

// transfer is one snapshot transfer of a folder of files files.
type transfer struct {
	files                 int
	kind                  string
	snapMS, copyMS        float64
	totalMS               float64
	requests, packs, gets int
}

// transfer measures a new session's first turn on the folder of n files
// twice: cold (no blob held anywhere but the connector), then warm (the
// server and the runner hold them all).
func (w *world) transfer(n int) ([]transfer, error) {
	ref := w.folders[fmt.Sprintf("files-%d", n)]
	if ref == "" {
		return nil, fmt.Errorf("no folder of %d files", n)
	}
	var out []transfer
	for _, kind := range []string{"cold", "warm"} {
		before := lines(w.serverLog)
		s, p, err := w.turn(map[string]any{"agent": "claude", "folder_ref": ref, "connector": w.connector, "isolate": true, "prompt": "t"}, "")
		if err != nil {
			return nil, err
		}
		t := transfer{files: n, kind: kind, snapMS: p.ms["snapshot (connector: hash, send)"], copyMS: p.ms["copy (runner: fetch, build)"], totalMS: p.ms["total (client)"]}
		for _, l := range lines(w.serverLog)[len(before):] {
			switch {
			case strings.Contains(l, "path=/api/v0/packs/fetch "):
				t.gets++
			case strings.Contains(l, "path=/api/v0/packs "):
				t.packs++
			}
			if strings.Contains(l, "path=/api/v0/blobs") || strings.Contains(l, "path=/api/v0/packs") {
				t.requests++
			}
		}
		out = append(out, t)
		// The session closed: its copy goes from the VM.
		w.api.call("POST", "/api/v0/workspaces/"+s.ID+"/close", nil, nil)
	}
	return out, nil
}

func (w *world) report(runs []phases) {
	byName := map[string][]phases{}
	var order []string
	for _, p := range runs {
		if _, ok := byName[p.name]; !ok {
			order = append(order, p.name)
		}
		byName[p.name] = append(byName[p.name], p)
	}
	fmt.Printf("\nThe path, %d runs: median (worst), ms\n", w.o.runs)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, name := range order {
		ps := byName[name]
		var keys []string
		for k := range ps[0].ms {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool { return phaseOrder(keys[a]) < phaseOrder(keys[b]) })
		fmt.Fprintf(tw, "%s\t\n", name)
		for _, k := range keys {
			var v []float64
			for _, p := range ps {
				v = append(v, p.ms[k])
			}
			med, worst := stats(v)
			fmt.Fprintf(tw, "  %s\t%s (%s)\n", k, num(med), num(worst))
		}
	}
	tw.Flush()
}

func phaseOrder(k string) int {
	for i, p := range []string{"total", "queue", "snapshot", "copy", "start", "agent", "checkpoint", "finish", "cockpit", "apply up", "apply refused"} {
		if strings.HasPrefix(k, p) {
			return i
		}
	}
	return 99
}

func reportTransfer(tr []transfer) {
	fmt.Printf("\nSnapshot transfer, Mac → control → VM runner, first turn of a new session, ms\n")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "files\tstore\tsnapshot (hash, send)\tcopy (fetch, build)\tfirst turn total\tblob requests (packs sent / fetched)\n")
	for _, t := range tr {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d (%d / %d)\n", t.files, t.kind, num(t.snapMS), num(t.copyMS), num(t.totalMS), t.requests, t.packs, t.gets)
	}
	tw.Flush()
}

// cleanup stops the runner in the VM, the connector and the server, and
// removes the scratch folders, unless -keep.
// cleanup runs once. A second caller, such as run's defer while a Ctrl-C's
// cleanup runs, waits in Once.Do until the first has finished, so main does
// not exit midway.
func (w *world) cleanup() { w.cleaned.Do(w.stopAll) }

func (w *world) stopAll() {
	if w.vmStarted {
		d := w.o.vmDir
		// The runner leads its own process group (setsid): the group gets
		// TERM, then KILL after 20 s, and the folder goes only once it ended.
		for _, pid := range []string{"runner.pid", "runner2.pid"} {
			out, _ := lima(w.o.vm, "test -f "+d+"/"+pid+" || exit 0; p=$(cat "+d+"/"+pid+"); kill -TERM -- -$p 2>/dev/null; "+
				"for i in $(seq 1 200); do kill -0 -- -$p 2>/dev/null || exit 0; sleep 0.1; done; kill -KILL -- -$p 2>/dev/null; "+
				"for i in $(seq 1 50); do kill -0 -- -$p 2>/dev/null || exit 0; sleep 0.1; done; echo still running")
			if strings.Contains(out, "still running") {
				fmt.Fprintf(os.Stderr, "remote: a runner in the VM did not stop; %s is kept\n", d)
				w.o.keep = true
			}
		}
		if !w.o.keep {
			if out, err := lima(w.o.vm, "rm -rf "+d); err != nil {
				fmt.Fprintf(os.Stderr, "remote: %s in the VM was not removed: %s\n", d, out)
			}
		}
	}
	for _, p := range []*proc{w.conn, w.server} {
		if p != nil {
			p.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-p.done:
			case <-time.After(20 * time.Second):
				syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
				<-p.done
			}
		}
	}
	if !w.o.keep && w.dir != "" {
		os.RemoveAll(w.dir)
	} else if w.dir != "" {
		fmt.Println("kept", w.dir)
	}
}

// client is the server's API, with its main token, read from the data
// folder and never printed.
type client struct {
	base, token string
}

type job struct {
	ID, State, Reason, Attempt string
	Created                    time.Time
	Ended                      *time.Time
	Attempts                   []struct {
		ID    string
		Taken time.Time
	}
	Result json.RawMessage
}

type applyAnswer struct {
	State     string
	Already   bool
	Files     []string
	Conflicts []struct{ Path, Why string }
}

func (c *client) call(method, path string, body, out any) (int, string) {
	return c.callMatch(method, path, body, "", out)
}

// callMatch is call with If-Match set, for a settings write.
func (c *client) callMatch(method, path string, body any, match string, out ...any) (int, string) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if match != "" {
		req.Header.Set("If-Match", match)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return 0, "the server could not be reached"
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if len(out) > 0 && out[0] != nil {
		_ = json.Unmarshal(b, out[0])
	}
	return resp.StatusCode, string(b)
}

func (c *client) apply(turn string) (int, applyAnswer) {
	var a applyAnswer
	code, _ := c.call("POST", "/api/v0/turns/"+turn+"/apply", nil, &a)
	return code, a
}

// events are the first time of each kind of the attempt's events, and the
// snapshot job its workspace.snapshot names.
func (c *client) events(attempt string) (map[string]time.Time, string, error) {
	var r struct {
		Events []struct {
			Kind string
			Time time.Time
			Data struct{ Job string }
		}
	}
	if code, out := c.call("GET", "/api/v0/attempts/"+attempt+"/events", nil, &r); code != 200 {
		return nil, "", fmt.Errorf("the attempt's events: %d %s", code, out)
	}
	m := map[string]time.Time{}
	snap := ""
	for _, e := range r.Events {
		if _, ok := m[e.Kind]; !ok {
			m[e.Kind] = e.Time
		}
		if e.Kind == "workspace.snapshot" {
			snap = e.Data.Job
		}
	}
	return m, snap, nil
}

// snapshotJob is the snapshot job id, ended.
func (c *client) snapshotJob(id string) (job, bool) {
	if id == "" {
		return job{}, false
	}
	var j job
	if code, _ := c.call("GET", "/api/v0/jobs/"+id, nil, &j); code != 200 || j.Ended == nil {
		return job{}, false
	}
	return j, true
}

// proc is a process that start started: done closes once it has ended.
type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// start runs the Mac's agentbus in its own process group, with home as its
// data folder.
func start(bin, home, logPath string, args ...string) (*proc, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	logf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "AGENTBUS_HOME" {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "AGENTBUS_HOME="+home)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); logf.Close(); close(p.done) }()
	return p, nil
}

func waitService(base, home string, p *proc) (*client, error) {
	c := &client{base: base}
	err := waitFor(30*time.Second, "the server", func() bool {
		b, err := os.ReadFile(filepath.Join(home, "token"))
		if err != nil {
			return false
		}
		c.token = strings.TrimSpace(string(b))
		code, _ := c.call("GET", "/api/v0/version", nil, nil)
		return code == 200
	})
	return c, err
}

func waitFor(limit time.Duration, what string, ok func() bool) error {
	for deadline := time.Now().Add(limit); ; time.Sleep(20 * time.Millisecond) {
		if ok() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: not within %s", what, limit)
		}
	}
}

// sh runs a command in dir, with env added, and answers its error with its
// output.
func sh(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

// lima runs a shell script in the VM and answers its output.
func lima(vm, script string) (string, error) {
	out, err := exec.Command("limactl", "shell", "--workdir", "/", vm, "--", "bash", "-c", script).CombinedOutput()
	return string(out), err
}

// skew bounds the VM's clock minus the Mac's: the VM's time read between two
// readings of the Mac's.
func skew(vm string) (lo, hi time.Duration) {
	lo, hi = -time.Hour, time.Hour
	for range 5 {
		before := time.Now()
		out, err := lima(vm, "date +%s%N")
		after := time.Now()
		ns, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil || perr != nil {
			continue
		}
		vmt := time.Unix(0, ns)
		lo, hi = max(lo, vmt.Sub(after)), min(hi, vmt.Sub(before))
	}
	return lo, hi
}

// fixture writes n text files of 200 B to 16 KB under dir, as tools/bench
// does: the same on every machine, each one different.
func fixture(dir string, n int) error {
	rng := rand.New(rand.NewPCG(uint64(n), 1))
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("g%02d", i/1000), fmt.Sprintf("d%d", i/100%10), fmt.Sprintf("f%03d.txt", i%100))
		if i%100 == 0 {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
		}
		size := 200 + rng.IntN(16*1024-200)
		b := bytes.Repeat(line, size/len(line)+1)[:size]
		b = append(b[:size-1], '\n')
		copy(b, fmt.Sprintf("file %d\n", i))
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	// The stand-in edits a.txt.
	return os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
}

func lines(p string) []string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func text(p string) string {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "<missing>"
	}
	return string(b)
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func stats(v []float64) (median, worst float64) {
	s := slices.Clone(v)
	slices.Sort(s)
	return s[len(s)/2], s[len(s)-1]
}

func num(v float64) string {
	switch {
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// clock prints how long a step took when the returned func runs.
func clock(name string) func() {
	t0 := time.Now()
	return func() { fmt.Printf("%s: %s\n", name, time.Since(t0).Round(time.Millisecond)) }
}
