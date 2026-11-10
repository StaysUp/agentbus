// Command bench measures AgentBus's own overhead on one machine: the time a
// turn takes around its agent, the diff and apply calls, the service's memory
// and its data folder's growth. It starts its own `agentbus up` on a scratch
// data folder and a free port, with a stand-in agent that edits a few files
// and exits at once, so model time is not in any number. It stops that
// service and removes the scratch folder when it ends.
//
//	go run ./tools/bench -agentbus ./agentbus
//
// See tools/bench/README.md for what each number means.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

// The stand-in for claude. It answers the version and sign-in checks, and on
// a turn appends the prompt to three files, adds bench-new.txt when it is
// missing and removes it when it is there, and prints the result line claude
// prints, so the session's next turn can resume it. The three files are in
// every fixture (see fixture).
const standIn = `#!/bin/sh
case "$1" in
--version) echo "0.0.0 (agentbus bench stand-in)"; exit 0 ;;
auth) exit 0 ;;
esac
for a; do last=$a; done
for f in g00/d0/f000.txt g00/d1/f050.txt g00/d2/f099.txt; do
	echo "$last" >> "$f"
done
if [ -e bench-new.txt ]; then rm bench-new.txt; else echo "$last" > bench-new.txt; fi
echo '{"type":"result","subtype":"success","result":"done","session_id":"bench-'$$'"}'
`

type options struct {
	bin, dir string
	port     int
	sizes    []int
	runs     int
	warmup   int
	keep     bool
	jsonOut  string
}

func main() {
	var o options
	var sizes string
	flag.StringVar(&o.bin, "agentbus", "", "the agentbus binary to measure (required)")
	flag.StringVar(&sizes, "sizes", "300,3000,50000", "folder sizes, in files, comma-separated")
	flag.IntVar(&o.runs, "runs", 5, "runs per size; each run is a new session of two turns")
	flag.IntVar(&o.warmup, "warmup", 1, "sessions run first on each size and left out of the numbers")
	flag.IntVar(&o.port, "port", 8790, "the port of the scratch service, on 127.0.0.1")
	flag.StringVar(&o.dir, "dir", "", "the scratch folder for fixtures and the data folder: a new or empty folder, removed at the end; default a new temp folder")
	flag.BoolVar(&o.keep, "keep", false, "keep the scratch folder")
	flag.StringVar(&o.jsonOut, "json", "", "also write every run's numbers to this file")
	flag.Parse()
	if o.bin == "" || o.runs < 1 || o.warmup < 0 {
		flag.Usage()
		os.Exit(2)
	}
	for _, s := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 300 || slices.Contains(o.sizes, n) {
			fmt.Fprintln(os.Stderr, "bench: every size must be a number of files, 300 or more, named once")
			os.Exit(2)
		}
		o.sizes = append(o.sizes, n)
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(o options) (err error) {
	bin, err := filepath.Abs(o.bin)
	if err != nil {
		return err
	}
	if o.dir == "" {
		if o.dir, err = os.MkdirTemp("", "agentbus-bench-"); err != nil {
			return err
		}
	} else if err = newDir(o.dir); err != nil {
		return err
	}
	if o.dir, err = filepath.Abs(o.dir); err != nil {
		return err
	}
	// The copies are clones only on the disk that holds the folder, so the
	// fixtures and the data folder share one.
	for _, d := range []string{"bin", "home", "fixtures", "folders"} {
		if err := os.MkdirAll(filepath.Join(o.dir, d), 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(o.dir, "bin", "claude"), []byte(standIn), 0o755); err != nil {
		return err
	}
	svc, err := startService(bin, o.dir, o.port)
	if err != nil {
		return err
	}
	defer func() {
		svc.stop()
		if !o.keep {
			if rerr := os.RemoveAll(o.dir); rerr != nil && err == nil {
				err = fmt.Errorf("the scratch folder %s could not be removed: %w", o.dir, rerr)
			}
		} else {
			fmt.Fprintln(os.Stderr, "bench: kept", o.dir)
		}
	}()
	// Stop cleanly on Ctrl-C, so the deferred stop and removal run.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); svc.interrupt() }()

	load, _ := exec.Command("uptime").Output()
	fmt.Printf("agentbus %s, %s/%s, %d CPUs, scratch %s\n%s", svc.version, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), o.dir, load)
	var all []sizeResult
	for _, n := range o.sizes {
		fix := filepath.Join(o.dir, "fixtures", strconv.Itoa(n))
		bytesWritten, err := fixture(fix, n)
		if err != nil {
			return fmt.Errorf("fixture of %d files: %w", n, err)
		}
		fmt.Fprintf(os.Stderr, "bench: fixture of %d files, %.1f MB\n", n, float64(bytesWritten)/1e6)
		sr := sizeResult{Files: n, Bytes: bytesWritten}
		warm := o.warmup
		for i := -warm; i < o.runs; i++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			folder := filepath.Join(o.dir, "folders", fmt.Sprintf("%d-%d", n, i+warm))
			if err := cloneFolder(fix, folder); err != nil {
				return fmt.Errorf("the run's folder could not be cloned: %w", err)
			}
			r, err := svc.session(folder, fmt.Sprintf("bench %d run %d", n, i))
			if err != nil {
				return fmt.Errorf("%d files, run %d: %w", n, i+1, err)
			}
			if i < 0 {
				fmt.Fprintf(os.Stderr, "bench: warm-up session, left out\n")
				continue
			}
			fmt.Fprintf(os.Stderr, "bench: %d files, run %d: first turn %s, later turn %s (%s)\n",
				n, i+1, ms(r.First.Total), ms(r.Later.Total), r.Method)
			sr.Runs = append(sr.Runs, r)
		}
		all = append(all, sr)
	}
	printTable(os.Stdout, all)
	if o.jsonOut != "" {
		b, _ := json.MarshalIndent(all, "", "  ")
		if err := os.WriteFile(o.jsonOut, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// newDir makes dir, or takes it when it is an empty folder. The bench removes
// it at the end, so it never takes a folder that holds anything.
func newDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err == nil || !errors.Is(err, fs.ErrExist) {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("-dir %s is not empty: name a new or empty folder, since the bench removes it at the end", dir)
	}
	return nil
}

// fixture writes n text files under dir, as g<NN>/d<N>/f<NNN>.txt: 100 files
// a folder, 10 folders a group. Sizes are 200 B to 16 KB, the same on every
// machine. It writes the files once; each run clones them.
func fixture(dir string, n int) (int64, error) {
	if _, err := os.Lstat(dir); err == nil {
		return 0, fmt.Errorf("%s already exists", dir)
	}
	rng := rand.New(rand.NewPCG(uint64(n), 1))
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	var total int64
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("g%02d", i/1000), fmt.Sprintf("d%d", i/100%10), fmt.Sprintf("f%03d.txt", i%100))
		if i%100 == 0 {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return 0, err
			}
		}
		size := 200 + rng.IntN(16*1024-200)
		b := bytes.Repeat(line, size/len(line)+1)[:size]
		b = append(b[:size-1], '\n')
		copy(b, fmt.Sprintf("file %d\n", i))
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return 0, err
		}
		total += int64(size)
	}
	return total, nil
}

// cloneFolder copies src to dst by clones: clonefile(2) on macOS, reflinks
// where the Linux disk has them (a plain copy where it has not).
func cloneFolder(src, dst string) error {
	// cp copies into dst when it exists: refuse rather than nest.
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("cp", "-c", "-R", src, dst)
	case "linux":
		cmd = exec.Command("cp", "-a", "--reflink=auto", src, dst)
	default:
		return errors.New("only macOS and Linux are supported")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// service is the scratch `agentbus up` the bench starts.
type service struct {
	cmd     *exec.Cmd
	base    string
	token   string
	home    string
	bin     string // the stand-in's folder, to tell its processes from the service's
	version string
	done    chan struct{}
	once    sync.Once
	client  *http.Client
}

func startService(bin, dir string, port int) (*service, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("port %d is in use: pick another with -port", port)
	}
	s := &service{base: "http://" + addr, home: filepath.Join(dir, "home"), bin: filepath.Join(dir, "bin"),
		done: make(chan struct{}), client: &http.Client{Timeout: 2 * time.Minute}}
	logf, err := os.Create(filepath.Join(dir, "up.log"))
	if err != nil {
		return nil, err
	}
	// -replay=false: own replay continues a session from its stored model
	// calls, and the stand-in makes none, so its next turn resumes by the
	// result line it prints. The file work of a turn is the same either way.
	s.cmd = exec.Command(bin, "up", "-listen", addr, "-replay=false")
	s.cmd.Stdout, s.cmd.Stderr = logf, logf
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "AGENTBUS_HOME", "ANTHROPIC_BASE_URL", "OPENAI_BASE_URL":
			continue
		}
		env = append(env, kv)
	}
	s.cmd.Env = append(env, "PATH="+s.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "AGENTBUS_HOME="+s.home)
	if err := s.cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	go func() { s.cmd.Wait(); logf.Close(); close(s.done) }()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		select {
		case <-s.done:
			return nil, fmt.Errorf("agentbus up ended at start; see %s", filepath.Join(dir, "up.log"))
		default:
		}
		if time.Now().After(deadline) {
			s.stop()
			return nil, errors.New("agentbus up did not answer within 30 seconds")
		}
		b, err := os.ReadFile(filepath.Join(s.home, "token"))
		if err != nil {
			continue
		}
		s.token = strings.TrimSpace(string(b))
		var v struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		}
		if code, err := s.call("GET", "/api/v0/version", nil, &v); err == nil && code == 200 {
			s.version = strings.TrimSpace(v.Version + " " + v.Commit)
			return s, nil
		}
	}
}

// interrupt asks the service to stop, for a Ctrl-C.
func (s *service) interrupt() { s.cmd.Process.Signal(syscall.SIGTERM) }

// stop ends the service: SIGTERM, then SIGKILL to its process group after
// 15 seconds.
func (s *service) stop() {
	s.once.Do(func() {
		s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(15 * time.Second):
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			<-s.done
		}
	})
}

func (s *service) call(method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %d, a reply that is not JSON", method, path, resp.StatusCode)
		}
	}
	return resp.StatusCode, nil
}

// turnTimes is one turn, from the client and from the record's own times.
type turnTimes struct {
	Total      time.Duration `json:"total"`      // POST /turns sent to the job seen ended
	Reply      time.Duration `json:"reply"`      // POST /turns sent to its reply
	Queue      time.Duration `json:"queue"`      // job created to taken by the runner
	Copy       time.Duration `json:"copy"`       // attempt started to workspace.ready: the copy and its first checkpoint; 0 on a later turn
	Start      time.Duration `json:"start"`      // to turn.created: the turn's start checkpoint and the agent started
	Agent      time.Duration `json:"agent"`      // turn.created to turn.ended: the stand-in runs for a few ms
	Checkpoint time.Duration `json:"checkpoint"` // turn.ended to workspace.changes: the end checkpoint and the diff
	Finish     time.Duration `json:"finish"`     // workspace.changes to the job ended
	Files      int           `json:"files"`      // changed files the job reported
}

type runResult struct {
	Method      string        `json:"method"` // how the copy was made, from workspace.ready
	First       turnTimes     `json:"first"`
	Later       turnTimes     `json:"later"`
	DiffFirst   time.Duration `json:"diff_first"`
	DiffLater   time.Duration `json:"diff_later"`
	ApplyFirst  time.Duration `json:"apply_first"`
	ApplyLater  time.Duration `json:"apply_later"`
	RSSIdleKB   int64         `json:"rss_idle_kb"`
	RSSPeakKB   int64         `json:"rss_peak_kb"`
	DiskSession int64         `json:"disk_session_bytes"` // allocated, before the first turn to after the later one
	DiskFirst   int64         `json:"disk_first_bytes"`   // allocated, by the first turn
	DiskLater   int64         `json:"disk_later_bytes"`   // allocated, by the later turn
	FreeSession int64         `json:"free_session_bytes"` // the disk's free space lost over the session
}

type sizeResult struct {
	Files int         `json:"files"`
	Bytes int64       `json:"bytes"`
	Runs  []runResult `json:"runs"`
}

// session runs one new session of two turns in folder, then diffs and
// applies both.
func (s *service) session(folder, tag string) (runResult, error) {
	var r runResult
	var err error
	if r.RSSIdleKB, err = s.rss(); err != nil {
		return r, err
	}
	d0, f0 := s.disk(), free(s.home)
	stopSampling := s.sample(&r.RSSPeakKB)
	first, session, err := s.turn(map[string]any{"agent": "claude", "folder": folder, "prompt": tag + " turn 1"}, &r.Method)
	if err != nil {
		stopSampling()
		return r, fmt.Errorf("first turn: %w", err)
	}
	d1 := s.disk()
	later, _, err := s.turn(map[string]any{"session": session, "prompt": tag + " turn 2"}, nil)
	stopSampling()
	if err != nil {
		return r, fmt.Errorf("later turn: %w", err)
	}
	d2, f2 := s.disk(), free(s.home)
	r.First, r.Later = first.times, later.times
	r.DiskFirst, r.DiskLater, r.DiskSession, r.FreeSession = d1-d0, d2-d1, d2-d0, f0-f2
	for _, t := range []struct {
		id   string
		diff *time.Duration
	}{{first.id, &r.DiffFirst}, {later.id, &r.DiffLater}} {
		var diff struct {
			Files *struct {
				Count int `json:"count"`
			} `json:"files"`
		}
		t0 := time.Now()
		code, err := s.call("GET", "/api/v0/turns/"+t.id+"/diff", nil, &diff)
		*t.diff = time.Since(t0)
		if err != nil || code != 200 || diff.Files == nil || diff.Files.Count == 0 {
			return r, fmt.Errorf("diff of %s: %d %v, no changed files", t.id, code, err)
		}
	}
	for _, t := range []struct {
		id   string
		appl *time.Duration
	}{{first.id, &r.ApplyFirst}, {later.id, &r.ApplyLater}} {
		var ap struct {
			State string `json:"state"`
		}
		t0 := time.Now()
		code, err := s.call("POST", "/api/v0/turns/"+t.id+"/apply", nil, &ap)
		*t.appl = time.Since(t0)
		if err != nil || code != 200 || ap.State != "applied" {
			return r, fmt.Errorf("apply of %s: %d %v, state %q", t.id, code, err, ap.State)
		}
	}
	// The stand-in added bench-new.txt in the first turn and removed it in
	// the later one; both applied, the folder must not hold it.
	if _, err := os.Stat(filepath.Join(folder, "bench-new.txt")); !errors.Is(err, fs.ErrNotExist) {
		return r, errors.New("after both applies the folder still holds bench-new.txt")
	}
	return r, nil
}

type turnDone struct {
	id    string
	times turnTimes
}

// turn starts a turn and waits for its job to end.
func (s *service) turn(body map[string]any, method *string) (turnDone, string, error) {
	var st struct {
		ID     string `json:"id"`
		Turn   string `json:"turn"`
		Job    string `json:"job"`
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}
	t0 := time.Now()
	code, err := s.call("POST", "/api/v0/turns", body, &st)
	reply := time.Since(t0)
	if err != nil || code != http.StatusAccepted || st.Job == "" {
		return turnDone{}, "", fmt.Errorf("POST /turns: %d %v %s", code, err, st.Error)
	}
	var job struct {
		State    string          `json:"state"`
		Created  time.Time       `json:"created"`
		Ended    *time.Time      `json:"ended"`
		Result   json.RawMessage `json:"result"`
		Attempts []struct {
			ID    string    `json:"id"`
			Taken time.Time `json:"taken"`
		} `json:"attempts"`
	}
	var total time.Duration
	for deadline := t0.Add(5 * time.Minute); ; time.Sleep(2 * time.Millisecond) {
		if code, err := s.call("GET", "/api/v0/jobs/"+st.Job, nil, &job); err != nil || code != 200 {
			return turnDone{}, "", fmt.Errorf("GET /jobs: %d %v", code, err)
		}
		if job.Ended != nil {
			total = time.Since(t0)
			break
		}
		if time.Now().After(deadline) {
			return turnDone{}, "", errors.New("the turn did not end within 5 minutes")
		}
	}
	if job.State != "finished" || len(job.Attempts) == 0 {
		return turnDone{}, "", fmt.Errorf("the turn's job ended %s", job.State)
	}
	at := job.Attempts[len(job.Attempts)-1]
	var evs struct {
		Events []struct {
			Time time.Time       `json:"time"`
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if code, err := s.call("GET", "/api/v0/attempts/"+at.ID+"/events", nil, &evs); err != nil || code != 200 {
		return turnDone{}, "", fmt.Errorf("GET /attempts/events: %d %v", code, err)
	}
	when := map[string]time.Time{}
	for _, e := range evs.Events {
		when[e.Kind] = e.Time
		if e.Kind == "workspace.ready" && method != nil {
			var d struct{ Method, Kind string }
			json.Unmarshal(e.Data, &d)
			*method = d.Method
			if *method == "" {
				*method = d.Kind
			}
		}
	}
	for _, k := range []string{"attempt.started", "turn.created", "turn.ended", "workspace.changes"} {
		if when[k].IsZero() {
			return turnDone{}, "", fmt.Errorf("the attempt has no %s event", k)
		}
	}
	var res struct {
		Changes struct {
			Count int `json:"count"`
		} `json:"changes"`
	}
	json.Unmarshal(job.Result, &res)
	t := turnTimes{Total: total, Reply: reply, Queue: at.Taken.Sub(job.Created), Files: res.Changes.Count,
		Agent: when["turn.ended"].Sub(when["turn.created"]), Checkpoint: when["workspace.changes"].Sub(when["turn.ended"]),
		Finish: job.Ended.Sub(when["workspace.changes"])}
	startFrom := when["attempt.started"]
	if ready, ok := when["workspace.ready"]; ok {
		t.Copy, startFrom = ready.Sub(when["attempt.started"]), ready
	}
	t.Start = when["turn.created"].Sub(startFrom)
	// The attempt's id is the turn's, also when the reply was queued and
	// named no turn.
	return turnDone{id: at.ID, times: t}, st.ID, nil
}

// rss is the resident memory of the service's own processes, in KB: the one
// the bench started and its descendants, less the stand-in's. On Linux `up`
// runs in a user namespace of its own, a second agentbus process.
func (s *service) rss() (int64, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=,args=").Output()
	if err != nil {
		return 0, err
	}
	type row struct {
		ppid int
		rss  int64
		args string
	}
	rows := map[int]row{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		kb, _ := strconv.ParseInt(f[2], 10, 64)
		rows[pid] = row{ppid, kb, strings.Join(f[3:], " ")}
	}
	root := s.cmd.Process.Pid
	var total int64
	for pid, r := range rows {
		if strings.Contains(r.args, s.bin) {
			continue
		}
		for p, hops := pid, 0; p > 1 && hops < 64; p, hops = rows[p].ppid, hops+1 {
			if p == root {
				total += r.rss
				break
			}
		}
	}
	if total == 0 {
		return 0, errors.New("the service's processes are not in the process table")
	}
	return total, nil
}

// sample reads rss every 50 ms into peak until the returned func is called.
func (s *service) sample(peak *int64) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			if kb, err := s.rss(); err == nil && kb > *peak {
				*peak = kb
			}
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	return func() { close(stop); <-done }
}

// disk is the space allocated under the data folder, in bytes, without the
// session copies themselves (worktrees/<id>): a clone or an overlay shares
// the folder's blocks, so counting its files would count the folder again.
// Shared blocks elsewhere (an overlay's reflinked lower layer) are counted in
// full; free is the disk's own figure.
func (s *service) disk() int64 {
	work := filepath.Join(s.home, "worktrees")
	var total int64
	filepath.WalkDir(s.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && filepath.Dir(p) == work && d.Name() != ".isolation" {
			return filepath.SkipDir
		}
		if fi, err := d.Info(); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				total += int64(st.Blocks) * 512
			}
		}
		return nil
	})
	return total
}

// free is the free space of the disk that holds dir, in bytes.
func free(dir string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// stat is the median and the worst of xs.
func stat[T int64 | time.Duration](xs []T) (med, worst T) {
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s) == 0 {
		return 0, 0
	}
	if len(s)%2 == 1 {
		med = s[len(s)/2]
	} else {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return med, s[len(s)-1]
}

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1f ms", float64(d)/1e6)
	}
	return fmt.Sprintf("%d ms", d.Round(time.Millisecond).Milliseconds())
}

func mb(b int64) string { return fmt.Sprintf("%.1f MB", float64(b)/1e6) }

// printTable writes one row per number and one column per size, each cell
// "median (worst)".
func printTable(w io.Writer, all []sizeResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprint(tw, "median (worst)\t")
	for _, sr := range all {
		fmt.Fprintf(tw, "%d files\t", sr.Files)
	}
	fmt.Fprintln(tw)
	durRow := func(name string, get func(runResult) time.Duration) {
		fmt.Fprintf(tw, "%s\t", name)
		for _, sr := range all {
			var xs []time.Duration
			for _, r := range sr.Runs {
				xs = append(xs, get(r))
			}
			m, x := stat(xs)
			fmt.Fprintf(tw, "%s (%s)\t", ms(m), ms(x))
		}
		fmt.Fprintln(tw)
	}
	sizeRow := func(name string, get func(runResult) int64, f func(int64) string) {
		fmt.Fprintf(tw, "%s\t", name)
		for _, sr := range all {
			var xs []int64
			for _, r := range sr.Runs {
				xs = append(xs, get(r))
			}
			m, x := stat(xs)
			fmt.Fprintf(tw, "%s (%s)\t", f(m), f(x))
		}
		fmt.Fprintln(tw)
	}
	for _, t := range []struct {
		name string
		get  func(runResult) turnTimes
	}{{"first turn", func(r runResult) turnTimes { return r.First }}, {"later turn", func(r runResult) turnTimes { return r.Later }}} {
		durRow(t.name+": total", func(r runResult) time.Duration { return t.get(r).Total })
		durRow("  POST reply", func(r runResult) time.Duration { return t.get(r).Reply })
		durRow("  queue", func(r runResult) time.Duration { return t.get(r).Queue })
		if t.name == "first turn" {
			durRow("  copy", func(r runResult) time.Duration { return t.get(r).Copy })
		}
		durRow("  start", func(r runResult) time.Duration { return t.get(r).Start })
		durRow("  agent", func(r runResult) time.Duration { return t.get(r).Agent })
		durRow("  checkpoint", func(r runResult) time.Duration { return t.get(r).Checkpoint })
		durRow("  finish", func(r runResult) time.Duration { return t.get(r).Finish })
	}
	durRow("diff, first turn", func(r runResult) time.Duration { return r.DiffFirst })
	durRow("diff, later turn", func(r runResult) time.Duration { return r.DiffLater })
	durRow("apply, first turn", func(r runResult) time.Duration { return r.ApplyFirst })
	durRow("apply, later turn", func(r runResult) time.Duration { return r.ApplyLater })
	kb := func(v int64) string { return mb(v * 1000) }
	sizeRow("RSS idle", func(r runResult) int64 { return r.RSSIdleKB }, kb)
	sizeRow("RSS peak in turns", func(r runResult) int64 { return r.RSSPeakKB }, kb)
	sizeRow("disk, first turn", func(r runResult) int64 { return r.DiskFirst }, mb)
	sizeRow("disk, later turn", func(r runResult) int64 { return r.DiskLater }, mb)
	sizeRow("disk, session", func(r runResult) int64 { return r.DiskSession }, mb)
	sizeRow("free space lost, session", func(r runResult) int64 { return r.FreeSession }, mb)
	tw.Flush()
	methods := map[string]bool{}
	for _, sr := range all {
		for _, r := range sr.Runs {
			methods[r.Method] = true
		}
	}
	keys := make([]string, 0, len(methods))
	for k := range methods {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	fmt.Fprintf(w, "copies made by: %s\n", strings.Join(keys, ", "))
}
