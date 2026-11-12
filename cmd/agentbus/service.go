package main

// The runner as a service, set up by agentbus login: a systemd user unit on
// Linux, a launchd agent on macOS. A unit or agent of that name already
// there is never changed: login says so and prints the command instead, so a
// runner set up by hand keeps its own.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// The service's names.
const (
	systemdUnit   = "agentbus-runner.service"
	launchdLabel  = "art.lorebug.agentbus.runner"
	launchdPlist  = launchdLabel + ".plist"
	serviceLogDir = "logs"
)

// serviceSetup sets the runner up as a service on this machine.
type serviceSetup struct {
	goos string
	// userHome is the person's home; configHome is $XDG_CONFIG_HOME, or
	// empty for ~/.config.
	userHome, configHome string
	uid                  int
	// run runs a system program (systemctl, launchctl) and says whether it
	// ended well; lookPath finds one.
	run      func(name string, args ...string) error
	lookPath func(string) (string, error)
}

func systemService() serviceSetup {
	home, _ := os.UserHomeDir()
	return serviceSetup{goos: runtime.GOOS, userHome: home, configHome: os.Getenv("XDG_CONFIG_HOME"), uid: os.Getuid(),
		run: func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			return cmd.Run()
		},
		lookPath: exec.LookPath}
}

// install writes the service for command (the runner's whole command line,
// this program first), run with AGENTBUS_HOME at home and PATH at path, and
// starts it. It answers what it did, in lines to print; an error says in a
// few words why it could not, and nothing was started.
func (s serviceSetup) install(command []string, home, path string) (string, error) {
	switch s.goos {
	case "linux":
		return s.systemd(command, home, path)
	case "darwin":
		return s.launchd(command, home, path)
	}
	return "", errors.New("this system has no service AgentBus sets up (systemd or launchd)")
}

func (s serviceSetup) systemd(command []string, home, path string) (string, error) {
	if s.userHome == "" {
		return "", errors.New("the home folder is not known")
	}
	if _, err := s.lookPath("systemctl"); err != nil {
		return "", errors.New("systemctl is not on this machine")
	}
	// The user's own service manager must answer: not so in a container,
	// or over ssh without a login session.
	if s.run("systemctl", "--user", "show-environment") != nil {
		return "", errors.New("systemd's user service manager does not answer here")
	}
	dir := s.configHome
	if dir == "" {
		dir = filepath.Join(s.userHome, ".config")
	}
	dir = filepath.Join(dir, "systemd", "user")
	unit := filepath.Join(dir, systemdUnit)
	if _, err := os.Lstat(unit); err == nil {
		return "", fmt.Errorf("a unit %s is already there and was left as it is", unit)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errors.New("the folder for systemd user units could not be made")
	}
	if err := writeNew(unit, []byte(unitText(command, home, path)), 0o644); err != nil {
		return "", errors.New("the unit file could not be written")
	}
	if s.run("systemctl", "--user", "daemon-reload") != nil || s.run("systemctl", "--user", "enable", "--now", systemdUnit) != nil {
		return "", fmt.Errorf("systemctl could not start the unit it wrote (%s); see systemctl --user status %s", unit, systemdUnit)
	}
	return "Set up the runner as a systemd user service and started it:\n" +
		"    unit:   " + unit + "\n" +
		"    status: systemctl --user status " + systemdUnit + "\n" +
		"    logs:   journalctl --user -u " + systemdUnit + "\n" +
		"It starts when you log in. To keep it running when you are logged out, run once: loginctl enable-linger\n", nil
}

// unitText is the systemd unit: each argument quoted, and % and $ escaped,
// as systemd reads them.
func unitText(command []string, home, path string) string {
	q := func(v string) string {
		v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(v)
		return `"` + v + `"`
	}
	exec := make([]string, len(command))
	for i, a := range command {
		exec[i] = q(a)
	}
	var b strings.Builder
	b.WriteString("# Written by agentbus login. Remove it with: systemctl --user disable --now " + systemdUnit + "\n")
	b.WriteString("[Unit]\nDescription=AgentBus runner\nWants=network-online.target\nAfter=network-online.target\n\n")
	b.WriteString("[Service]\nExecStart=" + strings.Join(exec, " ") + "\n")
	b.WriteString("Environment=" + q("AGENTBUS_HOME="+home) + "\n")
	if path != "" {
		b.WriteString("Environment=" + q("PATH="+path) + "\n")
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func (s serviceSetup) launchd(command []string, home, path string) (string, error) {
	if s.userHome == "" {
		return "", errors.New("the home folder is not known")
	}
	dir := filepath.Join(s.userHome, "Library", "LaunchAgents")
	plist := filepath.Join(dir, launchdPlist)
	if _, err := os.Lstat(plist); err == nil {
		return "", fmt.Errorf("a launchd agent %s is already there and was left as it is", plist)
	}
	logs := filepath.Join(home, serviceLogDir)
	if err := os.MkdirAll(logs, 0o700); err != nil {
		return "", errors.New("the folder for the runner's logs could not be made")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errors.New("the LaunchAgents folder could not be made")
	}
	if err := writeNew(plist, []byte(plistText(command, home, path, filepath.Join(logs, "runner-launchd.log"))), 0o644); err != nil {
		return "", errors.New("the launchd agent could not be written")
	}
	if s.run("launchctl", "bootstrap", "gui/"+strconv.Itoa(s.uid), plist) != nil {
		return "", fmt.Errorf("launchctl could not start the agent it wrote (%s)", plist)
	}
	return "Set up the runner as a launchd agent and started it:\n" +
		"    agent:  " + plist + "\n" +
		"    status: launchctl print gui/" + strconv.Itoa(s.uid) + "/" + launchdLabel + "\n" +
		"    logs:   " + filepath.Join(logs, "runner-launchd.log") + " and " + filepath.Join(home, "logs") + "\n" +
		"It starts when you log in.\n", nil
}

// plistText is the launchd agent, every value escaped as XML.
func plistText(command []string, home, path, log string) string {
	x := func(v string) string {
		return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(v)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by agentbus login. Remove it with: launchctl bootout gui/$(id -u)/` + launchdLabel + `, then delete this file. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range command {
		b.WriteString("\t\t<string>" + x(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	b.WriteString("\t\t<key>AGENTBUS_HOME</key>\n\t\t<string>" + x(home) + "</string>\n")
	if path != "" {
		b.WriteString("\t\t<key>PATH</key>\n\t\t<string>" + x(path) + "</string>\n")
	}
	b.WriteString("\t</dict>\n\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + x(log) + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + x(log) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// writeNew writes a file that must not be there yet.
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
