package main

// agentbus doctor: checks this machine before a first run, and prints one
// line per finding, each failed one with its fix. It reads the machine and
// the data folder only: it starts no service and needs none running, leaves
// nothing in the data folder, and never prints a token or a key.
//
// A line is ok, note or fail. A note is something that works, slower or not
// for every agent, such as a harness not installed; only a fail makes the
// exit code 1. On Linux it runs in a user namespace of its own, as up and
// runner do (userns_linux.go), so the overlay line says what up would get.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/staysup/agentbus/cockpit"
	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/harness"
)

const doctorUsage = "usage: agentbus doctor   (checks this machine before the first run; needs no service running)"

// minFree is the free space under which the data folder's disk fails: each
// Copy session and the record take their room there.
const minFree = 2 << 30

// finding is one line of the report.
type finding struct {
	status string // ok, note or fail
	line   string
}

const (
	statusOK   = "ok"
	statusNote = "note"
	statusFail = "fail"
)

// doctor is what the checks read; a test fills it with fakes.
type doctor struct {
	home     string
	getenv   func(string) string
	lookPath func(string) (string, error)
	packed   bool
	now      func() time.Time
	// disk is the free space, in bytes, of the disk that holds dir.
	disk func(dir string) (uint64, error)
	// copyWay says how Copy makes a session's copy of a folder on the data
	// folder's disk (doctor_<os>.go).
	copyWay func(home string) finding
	// appArmor is Linux's AppArmor line; nil where there is none.
	appArmor func() *finding
}

func runDoctor(args []string, home string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, doctorUsage)
		return 2
	}
	d := doctor{home: home, getenv: getenv, lookPath: exec.LookPath, packed: cockpit.Packed(), now: time.Now,
		disk: freeSpace, copyWay: copyWay, appArmor: appArmorFinding}
	return d.report(stdout)
}

// report prints every finding and answers the exit code.
func (d doctor) report(w io.Writer) int {
	state := "existing"
	if freshFolder(d.home) {
		state = "new"
	}
	fmt.Fprintf(w, "Data folder: %s (%s)\n\n", tildePath(d.home), state)
	failed := 0
	for _, f := range d.findings() {
		fmt.Fprintf(w, "%-4s  %s\n", f.status, f.line)
		if f.status == statusFail {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(w, "\n%s failed.\n", counted(failed, "check"))
		return 1
	}
	fmt.Fprintln(w, "\nNo check failed.")
	return 0
}

func (d doctor) findings() []finding {
	out := []finding{d.folder()}
	installed := map[string]bool{}
	for _, h := range harness.All() {
		i := h.Info()
		p, err := d.lookPath(i.Name)
		if err != nil {
			out = append(out, finding{statusNote, i.Name + " is not on PATH: install " + i.Product + " to use it"})
			continue
		}
		installed[i.Name] = true
		out = append(out, finding{statusOK, i.Name + " is on PATH: " + tildePath(p)})
	}
	if len(installed) == 0 {
		out = append(out, finding{statusFail, "no agent is installed: install " + harness.NameList() +
			", then start agentbus from a shell where it runs"})
	}
	out = append(out, d.signins(installed)...)
	out = append(out, d.endpoints(installed)...)
	out = append(out, d.copyWay(d.home))
	if d.appArmor != nil {
		if f := d.appArmor(); f != nil {
			out = append(out, *f)
		}
	}
	out = append(out, d.free())
	if d.packed {
		out = append(out, finding{statusOK, "the cockpit is packed in"})
	} else {
		out = append(out, finding{statusFail, "the cockpit is not packed in this build: install a release " +
			"(brew install staysup/tap/agentbus, or the install script in the README), or from a clone run tools/pack-cockpit.sh, then go build"})
	}
	return out
}

// connectWhere is where the person connects a subscription or sets a key.
const connectWhere = "in the cockpit's Settings, Subscriptions"

// signins are the subscriptions connected on this machine's control and the
// keys set there: one line for each harness that has a subscription and is
// installed or connected, one for the keys, and a fail when a turn would
// have nothing to sign in with.
func (d doctor) signins(installed map[string]bool) []finding {
	subs := subscriptionStore(d.home)
	subs.Now = d.now
	var out []finding
	usable := 0
	for _, h := range harness.All() {
		i := h.Info()
		if i.Login == nil {
			continue
		}
		st, err := subs.Status(i.Name)
		switch {
		case err != nil:
			out = append(out, finding{statusFail, "the " + i.Name + " subscription cannot be read (" +
				strings.TrimPrefix(err.Error(), "credentials: ") + "): connect it again " + connectWhere})
		case st.Expired:
			out = append(out, finding{statusFail, "the " + i.Name + " subscription cannot be used: " + st.Error + " " + connectWhere})
		case st.Connected:
			usable++
			when := "control renews it"
			if !st.Renews && st.ExpiresAt != nil {
				when = "it works until " + st.ExpiresAt.Local().Format(time.DateOnly)
			}
			out = append(out, finding{statusOK, "the " + i.Name + " subscription is connected; " + when})
		case installed[i.Name]:
			out = append(out, finding{statusNote, "the " + i.Name + " subscription is not connected: connect it " + connectWhere})
		}
	}
	var set []string
	for _, name := range credentials.KeyNames {
		st, err := subs.KeyStatus(name)
		switch {
		case err != nil:
			out = append(out, finding{statusFail, "the " + name + " key cannot be read: set it again " + connectWhere})
		case st.Set:
			set = append(set, name)
		}
	}
	usable += len(set)
	if len(set) > 0 {
		out = append(out, finding{statusOK, "keys set: " + strings.Join(set, ", ")})
	}
	if usable == 0 && len(installed) > 0 {
		out = append(out, finding{statusFail, "no subscription is connected and no key is set, so every turn is refused: " +
			"start agentbus up, then connect a subscription or set a key " + connectWhere})
	}
	return out
}

// endpoints are the variables, set where agentbus starts, that already send
// an installed harness to an endpoint of its own: a turn of it is refused
// (wire.go).
func (d doctor) endpoints(installed map[string]bool) []finding {
	var out, names []string
	for _, h := range harness.All() {
		i := h.Info()
		if i.OwnEndpointEnv == "" || !installed[i.Name] {
			continue
		}
		names = append(names, i.OwnEndpointEnv)
		if d.getenv(i.OwnEndpointEnv) != "" {
			out = append(out, i.OwnEndpointEnv+" is set, so every "+i.Name+" turn is refused: unset it in the shell that starts agentbus")
		}
	}
	if len(names) == 0 {
		return nil
	}
	if len(out) == 0 {
		return []finding{{statusOK, "no variable sends an agent elsewhere (" + strings.Join(names, ", ") + ")"}}
	}
	fs := make([]finding, len(out))
	for i, l := range out {
		fs[i] = finding{statusFail, l}
	}
	return fs
}

// folder says whether the data folder can be written, or made where it is
// not yet: up makes it at its first start.
func (d doctor) folder() finding {
	fi, err := os.Stat(d.home)
	switch {
	case err == nil && !fi.IsDir():
		return finding{statusFail, "the data folder is a file: move it, or set AGENTBUS_HOME to a folder"}
	case err == nil && writable(d.home):
		return finding{statusOK, "the data folder can be written"}
	case err == nil:
		return finding{statusFail, "the data folder cannot be written: make it yours, or set AGENTBUS_HOME to a folder you can write"}
	case !errors.Is(err, os.ErrNotExist):
		return finding{statusFail, "the data folder cannot be read: make it yours, or set AGENTBUS_HOME to a folder you can write"}
	}
	if up := existingDir(d.home); !writable(up) {
		return finding{statusFail, "the data folder cannot be made, since " + tildePath(up) +
			" cannot be written: set AGENTBUS_HOME to a folder you can write"}
	}
	return finding{statusOK, "the data folder can be made"}
}

// writable says a folder can be made in dir, by making one and removing it.
func writable(dir string) bool {
	tmp, err := os.MkdirTemp(dir, ".agentbus-doctor-")
	if err != nil {
		return false
	}
	return os.Remove(tmp) == nil
}

// free is the data folder's disk's free space.
func (d doctor) free() finding {
	n, err := d.disk(existingDir(d.home))
	switch {
	case err != nil:
		return finding{statusNote, "the free space of the data folder's disk could not be read"}
	case n < minFree:
		return finding{statusFail, fmt.Sprintf("%s free on the data folder's disk, under %s: free some, or set AGENTBUS_HOME to a folder on another disk",
			gib(n), gib(minFree))}
	}
	return finding{statusOK, gib(n) + " free on the data folder's disk"}
}

func gib(n uint64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// existingDir is p, or the nearest folder above it that exists: the disk a
// data folder not made yet will be on.
func existingDir(p string) string {
	for {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return p
		}
		up := filepath.Dir(p)
		if up == p {
			return p
		}
		p = up
	}
}
