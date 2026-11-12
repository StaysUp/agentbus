//go:build integration && (darwin || linux)

package sandbox

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/staysup/agentbus/netjail"
)

// With probeEnv set, the test binary is the probe: it runs each probe named
// in the variable, one per line "<name> <op> <arg>", and prints
// "<name> allowed" or "<name> denied". It never prints what it read.
const probeEnv = "SANDBOX_TEST_PROBES"

func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv(probeEnv); ok {
		probe(v)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func probe(list string) {
	direct := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	var proxied *http.Client
	if pu, err := url.Parse(os.Getenv("HTTP_PROXY")); err == nil && pu.Host != "" {
		proxied = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
	}
	get := func(c *http.Client, u string) error {
		if c == nil {
			return fmt.Errorf("no proxy")
		}
		resp, err := c.Get(u)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("%d", resp.StatusCode)
		}
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
		f := strings.SplitN(line, " ", 3)
		name, op, arg := f[0], f[1], f[2]
		var err error
		switch op {
		case "read":
			_, err = os.ReadFile(arg)
		case "write":
			err = os.WriteFile(arg, []byte("probe"), 0o600)
		case "list":
			_, err = os.ReadDir(arg)
		case "stat":
			_, err = os.Stat(arg)
		case "unix":
			var c net.Conn
			if c, err = net.DialTimeout("unix", arg, 3*time.Second); err == nil {
				c.Close()
			}
		case "tcp":
			var c net.Conn
			if c, err = net.DialTimeout("tcp", arg, 3*time.Second); err == nil {
				c.Close()
			}
		case "dns":
			_, err = net.LookupHost(arg)
		case "get":
			err = get(direct, arg)
		case "proxy":
			err = get(proxied, arg)
		case "signal":
			pid, _ := strconv.Atoi(arg)
			err = syscall.Kill(pid, 0)
		case "exec":
			err = exec.Command(arg).Run()
		default:
			err = fmt.Errorf("unknown op")
		}
		if err != nil {
			fmt.Println(name, "denied")
		} else {
			fmt.Println(name, "allowed")
		}
	}
}

// probeCase is one probe and what the wall must answer. A write is
// answered by this machine, not by the probe: allowed means the file holds
// what the probe wrote, here, after it ends. A write the wall lets happen
// in a folder of its own (Linux's private /tmp, and the folders bwrap makes
// there to bind a session's paths) never reaches this machine, so it is
// denied. open says the
// probe is also run with no wall and must be allowed there, which shows the
// probe can see what it tries; false where the answer outside depends on
// the machine (the internet) or means nothing (the proxy's own refusal).
type probeCase struct {
	name, op, arg string
	allowed       bool
	open          bool
}

// tree is the layout the probes run against: two sessions, a folder
// outside both, and a fake person's home with the files an agent should
// never reach. Nothing in it is a real secret.
type tree struct {
	root, homes, copyA, homeA, copyB, homeB, outside, user string
}

func makeTree(t *testing.T) tree { return makeTreeIn(t, t.TempDir()) }

// makeTreeIn lays the tree out in dir.
func makeTreeIn(t *testing.T, dir string) tree {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	tr := tree{root: root, homes: filepath.Join(root, "homes"), outside: filepath.Join(root, "outside"), user: filepath.Join(root, "user")}
	tr.copyA, tr.homeA = filepath.Join(root, "sessions", "A"), filepath.Join(tr.homes, "A")
	tr.copyB, tr.homeB = filepath.Join(root, "sessions", "B"), filepath.Join(tr.homes, "B")
	for _, d := range []string{tr.copyA, tr.homeA, tr.copyB, tr.homeB, tr.outside,
		filepath.Join(tr.user, ".ssh"), filepath.Join(tr.user, ".aws"), filepath.Join(tr.user, "Documents"),
		filepath.Join(tr.user, "Library", "Keychains")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{
		filepath.Join(tr.copyA, "file.txt"):          "a",
		filepath.Join(tr.copyB, "file.txt"):          "b",
		filepath.Join(tr.outside, "file.txt"):        "outside",
		filepath.Join(tr.user, ".ssh", "config"):     "Host example\n",
		filepath.Join(tr.user, ".gitconfig"):         "[user]\n\tname = probe\n",
		filepath.Join(tr.user, ".aws", "config"):     "[profile probe]\nregion = eu-west-1\n",
		filepath.Join(tr.user, "Documents", "a.txt"): "doc",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

// listenUnix is a Unix socket at p, accepting and closing, until the test
// ends. The path is short: a socket path is at most about 100 bytes.
func listenUnix(t *testing.T, p string) string {
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return p
}

// TestProbes runs the research's 33 probes (2026-11-11-session-sandbox.md
// in the notes) in a wall around session A.
func TestProbes(t *testing.T) {
	tr := makeTree(t)
	// The gateway, forwarded; a site the proxy lets through; another
	// loopback port, reached by nothing.
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer gw.Close()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer site.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer other.Close()
	sockDir, err := os.MkdirTemp("/tmp", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	agentSock := listenUnix(t, filepath.Join(sockDir, "agent.sock"))
	dockerSock := listenUnix(t, filepath.Join(sockDir, "docker.sock"))
	systemSock := "/var/run/mDNSResponder"
	if runtime.GOOS == "linux" {
		systemSock = "/run/dbus/system_bus_socket"
	}
	_, statErr := os.Stat(systemSock)
	hostTmp := filepath.Join("/tmp", "agentbus-sbx-probe-"+strconv.Itoa(os.Getpid()))
	defer os.Remove(hostTmp)

	if err := CopyGrants(tr.homeA, tr.homeA+".grants", tr.user, []string{"~/.aws/config"}); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gwAddr, siteAddr := gw.Listener.Addr().String(), site.Listener.Addr().String()
	mac := runtime.GOOS == "darwin"
	box, err := New(Spec{Copy: tr.copyA, Home: tr.homeA, Read: Program(self),
		Net: netjail.Spec{Forward: []string{gwAddr}, Pass: []string{siteAddr}}})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	cases := []probeCase{
		{"copy.read", "read", filepath.Join(tr.copyA, "file.txt"), true, true},
		{"copy.write", "write", filepath.Join(tr.copyA, "new.txt"), true, true},
		{"home.write", "write", filepath.Join(tr.homeA, "x"), true, true},
		{"home.tmp.write", "write", filepath.Join(tr.homeA, TmpDir, "x"), true, true},
		{"other.copy.read", "read", filepath.Join(tr.copyB, "file.txt"), false, true},
		{"other.copy.write", "write", filepath.Join(tr.copyB, "new.txt"), false, true},
		{"other.home.list", "list", tr.homeB, false, true},
		{"outside.read", "read", filepath.Join(tr.outside, "file.txt"), false, true},
		{"outside.write", "write", filepath.Join(tr.outside, "new.txt"), false, true},
		// Linux gives the session a /tmp of its own, macOS none; either
		// way nothing reaches this machine's /tmp.
		{"tmp.write", "write", hostTmp, false, true},
		{"homes.write", "write", filepath.Join(tr.homes, "beside"), false, true},
		{"grant.read", "read", filepath.Join(tr.homeA, ".aws", "config"), true, true},
		{"grant.original.read", "read", filepath.Join(tr.user, ".aws", "config"), false, true},
		{"grant.original.write", "write", filepath.Join(tr.user, ".aws", "config"), false, true},
		{"user.list", "list", tr.user, false, true},
		{"user.ssh.list", "list", filepath.Join(tr.user, ".ssh"), false, true},
		{"user.documents.list", "list", filepath.Join(tr.user, "Documents"), false, true},
		{"user.keychains.list", "list", filepath.Join(tr.user, "Library", "Keychains"), false, true},
		{"user.gitconfig.read", "read", filepath.Join(tr.user, ".gitconfig"), false, true},
		// macOS lets a session learn that a file exists, not read it: a
		// known gap of this proof of concept.
		{"user.ssh.stat", "stat", filepath.Join(tr.user, ".ssh"), mac, true},
		{"user.ssh.config.stat", "stat", filepath.Join(tr.user, ".ssh", "config"), mac, true},
		{"socket.agent", "unix", agentSock, false, true},
		{"socket.docker", "unix", dockerSock, false, true},
		{"socket.system", "unix", systemSock, false, statErr == nil},
		{"dns", "dns", "example.com", false, false},
		{"tcp.internet", "tcp", "1.1.1.1:443", false, false},
		{"tcp.loopback.other", "tcp", other.Listener.Addr().String(), false, true},
		{"https.direct", "get", "https://example.com/", false, false},
		{"tcp.gateway", "tcp", gwAddr, true, true},
		{"proxy.allowed", "proxy", "http://" + siteAddr + "/", true, false},
		{"proxy.refused", "proxy", "http://blocked.invalid/", false, false},
		{"signal.runner", "signal", strconv.Itoa(os.Getpid()), false, true},
	}
	if mac {
		// A setuid program: Seatbelt refuses to start it.
		cases = append(cases, probeCase{"exec.setuid", "exec", "/bin/ps", false, true})
	} else {
		// Another process's environment: in its own pid namespace the
		// runner is not there.
		cases = append(cases, probeCase{"proc.runner.environ", "read", "/proc/" + strconv.Itoa(os.Getpid()) + "/environ", false, true})
	}
	if len(cases) != 33 {
		t.Fatalf("%d probes, want the research's 33", len(cases))
	}
	var list strings.Builder
	for _, c := range cases {
		fmt.Fprintf(&list, "%s %s %s\n", c.name, c.op, c.arg)
	}
	// reset puts every write's target back as it was before a run.
	reset := func() {
		for _, c := range cases {
			if c.op == "write" {
				os.Remove(c.arg)
			}
		}
		if err := os.WriteFile(filepath.Join(tr.user, ".aws", "config"), []byte("[profile probe]\nregion = eu-west-1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	run := func(walled bool) map[string]bool {
		cmd := exec.Command(self)
		cmd.Dir = tr.copyA
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + tr.homeA, probeEnv + "=" + list.String()}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		start := cmd.Start
		if walled {
			cmd.Env = append(cmd.Env, box.Env()...)
			start = func() error { return box.Start(cmd) }
		}
		if err := start(); err != nil {
			t.Fatalf("start (walled %t): %v", walled, err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("probe (walled %t): %v\n%s", walled, err, out.String())
		}
		got := map[string]bool{}
		sc := bufio.NewScanner(&out)
		for sc.Scan() {
			name, res, _ := strings.Cut(sc.Text(), " ")
			got[name] = res == "allowed"
		}
		for _, c := range cases {
			if c.op == "write" {
				said := got[c.name]
				got[c.name] = wroteHere(c.arg)
				if said && !got[c.name] {
					t.Logf("%s: the write stayed inside the wall (walled %t)", c.name, walled)
				}
			}
		}
		return got
	}
	open := run(false)
	reset()
	walled := run(true)
	n := 0
	for _, c := range cases {
		if c.open && !open[c.name] {
			t.Errorf("%s: denied with no sandbox, so the probe shows nothing", c.name)
		}
		if walled[c.name] != c.allowed {
			t.Errorf("%s: allowed %t in the sandbox, want %t", c.name, walled[c.name], c.allowed)
		}
		if walled[c.name] {
			n++
		}
	}
	open0 := 0
	for _, v := range open {
		if v {
			open0++
		}
	}
	t.Logf("%s: %d of %d probes allowed in the sandbox, %d with none", runtime.GOOS, n, len(cases), open0)
}

// wroteHere says p, on this machine, holds what a write probe writes.
func wroteHere(p string) bool {
	b, err := os.ReadFile(p)
	return err == nil && string(b) == "probe"
}

// TestAgentsStart runs claude --version and codex --version in a wall, as
// the runner would start them: by the path on PATH, with only their own
// program readable. An agent not installed here is skipped.
func TestAgentsStart(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Skipf("%s is not installed here", name)
			}
			tr := makeTree(t)
			box, err := New(Spec{Copy: tr.copyA, Home: tr.homeA, Read: Program(path)})
			if err != nil && os.Geteuid() == 0 && strings.Contains(err.Error(), "run as root") {
				// Root's PATH names another user's agent, in a folder
				// root cannot reach in the wall: refused, plainly, which
				// is the answer wanted.
				t.Logf("refused as root: %v", err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer box.Close()
			cmd := exec.Command(path, "--version")
			cmd.Dir = tr.copyA
			cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + tr.homeA}, box.Env()...)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := box.Start(cmd); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("%s --version in the sandbox: %v\n%s", name, err, out.String())
			}
			t.Logf("%s", strings.TrimSpace(out.String()))
		})
	}
}

// A wall with ReadOnly: the copy is read, never written; the home still is.
func TestReadOnlyCopy(t *testing.T) {
	tr := makeTree(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(Spec{Copy: tr.copyA, ReadOnly: true, Home: tr.homeA, Read: Program(self)})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	cmd := exec.Command(self)
	cmd.Dir = tr.copyA
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + tr.homeA, probeEnv + "=" +
		"copy.read read " + filepath.Join(tr.copyA, "file.txt") + "\n" +
		"copy.write write " + filepath.Join(tr.copyA, "new.txt") + "\n" +
		"home.write write " + filepath.Join(tr.homeA, "x")}, box.Env()...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := box.Start(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, out.String())
	}
	if got, want := strings.TrimSpace(out.String()), "copy.read allowed\ncopy.write denied\nhome.write allowed"; got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if wroteHere(filepath.Join(tr.copyA, "new.txt")) || !wroteHere(filepath.Join(tr.homeA, "x")) {
		t.Errorf("on this machine: the copy written, or the home not")
	}
}

// A runner whose data folder is in /tmp: the session's copy and home are
// there too. Linux gives the session a private /tmp and binds the copy and
// home over it, so they are still this machine's folders; the folder that
// holds them is not.
func TestSessionInTmp(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sbx-data")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	tr := makeTreeIn(t, dir)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(Spec{Copy: tr.copyA, Home: tr.homeA, Read: Program(self)})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	want := map[string]bool{
		filepath.Join(tr.copyA, "new.txt"):   true,
		filepath.Join(tr.homeA, "x"):         true,
		filepath.Join(tr.homeA, TmpDir, "x"): true,
		filepath.Join(tr.homes, "beside"):    false,
		filepath.Join(tr.copyB, "new.txt"):   false,
		filepath.Join(tr.root, "beside"):     false,
	}
	var list strings.Builder
	for p := range want {
		fmt.Fprintf(&list, "w write %s\n", p)
	}
	cmd := exec.Command(self)
	cmd.Dir = tr.copyA
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + tr.homeA, probeEnv + "=" + list.String()}, box.Env()...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := box.Start(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, out.String())
	}
	for p, w := range want {
		if got := wroteHere(p); got != w {
			t.Errorf("%s: on this machine %t, want %t", p, got, w)
		}
	}
}

// A program granted as one file, through a link (as agentbus-api is), runs
// in the wall, and the other files of its folder stay unreadable.
func TestOneFileGrant(t *testing.T) {
	tr := makeTree(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A copy of the probe beside a file of its own, reached by a link.
	dir := filepath.Join(tr.root, "dl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "prog")
	if err := os.WriteFile(prog, b, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tr.root, "bin", "agentbus-api")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(prog, link); err != nil {
		t.Fatal(err)
	}
	box, err := New(Spec{Copy: tr.copyA, Home: tr.homeA, Read: []string{link}})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	cmd := exec.Command(link)
	cmd.Dir = tr.copyA
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + tr.homeA, probeEnv + "=" +
		"other read " + filepath.Join(dir, "other-file")}, box.Env()...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := box.Start(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the program did not run through its link: %v\n%s", err, out.String())
	}
	if got := strings.TrimSpace(out.String()); got != "other denied" {
		t.Errorf("got %q, want the folder's other file denied", got)
	}
}
