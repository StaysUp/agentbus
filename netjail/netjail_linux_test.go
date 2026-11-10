package netjail

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// With testAgent set, the test binary is the agent: it tries every way out
// and prints what happened, one line each.
const testAgent = "NETJAIL_TEST_AGENT"

func TestMain(m *testing.M) {
	if v := os.Getenv(testAgent); v != "" {
		agent(v)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// agent is the closed-off agent. v is "<forwarded addr> <site port>".
func agent(v string) {
	fwd, sitePort, _ := strings.Cut(v, " ")
	out := func(name string, err error) {
		if err != nil {
			fmt.Printf("%s: no\n", name)
			return
		}
		fmt.Printf("%s: yes\n", name)
	}
	get := func(c *http.Client, u string) error {
		resp, err := c.Get(u)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("%d", resp.StatusCode)
		}
		return nil
	}
	direct := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}, Timeout: 5 * time.Second}
	// localhost is left out of the proxy by NO_PROXY; name it by its
	// proxy URL so the request goes through the proxy.
	pu, _ := url.Parse(os.Getenv("HTTP_PROXY"))
	forced := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}

	out("forwarded", get(direct, "http://"+fwd+"/"))
	out("allowed-via-proxy", get(forced, "http://localhost:"+sitePort+"/"))
	out("site-direct", get(direct, "http://127.0.0.1:"+sitePort+"/"))
	out("not-listed", get(proxied, "http://blocked.example/"))
	_, err := net.DialTimeout("tcp", "1.1.1.1:443", 3*time.Second)
	out("raw-ip", err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = net.DefaultResolver.LookupHost(ctx, "example.com")
	cancel()
	out("dns", err)
	out("rejoin", rejoin())
	fmt.Printf("uid: %d\n", os.Getuid())
	fmt.Printf("ambient: %s\n", capAmb())
}

// A real agent closed off: it reaches the forwarded address and an
// allowed host through the proxy, and nothing else; the proxy reports the
// refusal. As a normal user and as root.
func TestAgentClosedOff(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("this machine cannot close an agent off: %v", err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	gw := httptest.NewServer(ok) // forwarded, as a gateway is
	defer gw.Close()
	site := httptest.NewServer(ok) // reached through the proxy only
	defer site.Close()
	gwAddr := strings.TrimPrefix(gw.URL, "http://")
	_, sitePort, _ := net.SplitHostPort(strings.TrimPrefix(site.URL, "http://"))
	var got refusals
	j := New(Spec{Forward: []string{gwAddr}, Allow: []string{"localhost"}, Refused: got.add})
	defer j.Close()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(append(os.Environ(), j.Env()...), testAgent+"="+gwAddr+" "+sitePort)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := j.Start(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("agent: %v\n%s", err, stderr.String())
	}
	want := map[string]string{"forwarded": "yes", "allowed-via-proxy": "yes", "site-direct": "no",
		"not-listed": "no", "raw-ip": "no", "dns": "no", "uid": fmt.Sprint(os.Getuid()), "ambient": "0000000000000000", "rejoin": "no"}
	sc := bufio.NewScanner(&stdout)
	seen := 0
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ": ")
		if w, ok := want[k]; ok {
			seen++
			if v != w {
				t.Errorf("%s: %s, want %s", k, v, w)
			}
		}
	}
	if seen != len(want) {
		t.Errorf("agent said:\n%s\n%s", stdout.String(), stderr.String())
	}
	r := got.all()
	if len(r) != 1 || r[0] != "blocked.example:80 blocked.example is not in [network] allow of permissions.toml" {
		t.Errorf("refusals: %q", r)
	}
}

// A helper that cannot ready the namespace refuses with why, and nothing
// runs.
func TestStartRefusesWithWhy(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("this machine cannot close an agent off: %v", err)
	}
	// An address no namespace has: the helper cannot listen there.
	j := New(Spec{Forward: []string{"192.0.2.1:9"}})
	defer j.Close()
	cmd := exec.Command("/bin/true")
	err := j.Start(cmd)
	if err == nil || !strings.Contains(err.Error(), "192.0.2.1:9 could not be listened on in the agent's namespace") {
		t.Fatalf("err = %v", err)
	}
	if cmd.ProcessState == nil {
		t.Error("the helper was not waited for")
	}
}

// capAmb is this process's ambient capabilities, as /proc says them.
func capAmb() string {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CapAmb:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

// rejoin tries to join the network of process 1, outside: as setns(2)
// does for nsenter -t 1 -n.
func rejoin() error {
	f, err := os.Open("/proc/1/ns/net")
	if err != nil {
		return err
	}
	defer f.Close()
	runtime.LockOSThread() // the thread ends with the agent
	return unix.Setns(int(f.Fd()), unix.CLONE_NEWNET)
}
