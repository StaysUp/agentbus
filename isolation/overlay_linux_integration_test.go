//go:build integration && linux

package isolation

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The overlay round trip in a user and mount namespace, as a runner that is
// not root makes its copies: the mount options must be taken there, and a
// folder removed and made again is marked opaque under user.
func TestOverlayInUserNamespace(t *testing.T) {
	if os.Getenv("AGENTBUS_TEST_USERNS") == "1" {
		t.Skip("already in the namespace")
	}
	if os.Geteuid() == 0 {
		t.Skip("root mounts without a namespace")
	}
	var u unix.Utsname
	unix.Uname(&u)
	var major, minor int
	for i, f := range strings.SplitN(unix.ByteSliceToString(u.Release[:]), ".", 3)[:2] {
		n := 0
		for _, c := range f {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		if i == 0 {
			major = n
		} else {
			minor = n
		}
	}
	if major < 5 || (major == 5 && minor < 11) {
		t.Skip("overlay in a user namespace needs Linux 5.11")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOverlayRoundTrip$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), "AGENTBUS_TEST_USERNS=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	if err != nil && cmd.ProcessState == nil {
		t.Skipf("no user namespace here: %v", err)
	}
	if err != nil || !strings.Contains(string(out), "--- PASS: TestOverlayRoundTrip") {
		t.Fatalf("the round trip in a user namespace: %v\n%s", err, out)
	}
}
