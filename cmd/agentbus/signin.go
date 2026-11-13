package main

// The cockpit's sign-in link, like Jupyter's: agentbus up prints
// http://127.0.0.1:<port>/?t=<code> and opens it on a desktop; the cockpit
// trades the code once for the token (api/signin.go). A new link comes from
// starting agentbus up again. The token itself is never printed.

import (
	"encoding/json"
	"html"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// signInLink is the link that signs the cockpit in, on the address the
// service listens on: loopback for a wildcard listener (0.0.0.0, ::), since
// the browser is on this machine, and the address itself otherwise, so a
// service on [::1] gets an [::1] link.
func signInLink(listen, code string) string {
	return "http://" + linkHost(listen) + "/?t=" + url.QueryEscape(code)
}

// linkHost is the host and port a browser on this machine reaches a
// service listening on listen by.
func linkHost(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "", defaultPort
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// browserCommand is the command that opens a link on this desktop, or nil
// when there is none to open it on: no display, or a shell over SSH, where
// the browser would open on another person's screen or nowhere.
func browserCommand(goos string, getenv func(string) string, lookPath func(string) (string, error)) []string {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return nil
	}
	switch goos {
	case "darwin":
		return []string{"open"}
	case "linux":
		if getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
			return nil
		}
		if p, err := lookPath("xdg-open"); err == nil {
			return []string{p}
		}
	}
	return nil
}

// openFile is the page, in the data folder, that the browser is opened on:
// it sends the browser on to the sign-in link. The link is never a
// program's argument, which other users can read in the process list; the
// file is the person's own (0600, in the 0700 data folder).
const openFile = "open.html"

// openFileKept is how long the page stays: long enough for a browser that
// starts cold to read it.
const openFileKept = 2 * time.Minute

// openBrowser opens link on a desktop, through openFile in home. It reports
// whether it tried; the browser's own answer is not waited on.
func openBrowser(home, link string, getenv func(string) string) bool {
	argv := browserCommand(runtime.GOOS, getenv, exec.LookPath)
	if argv == nil {
		return false
	}
	page, err := writeOpenPage(home, link)
	if err != nil {
		return false
	}
	cmd := exec.Command(argv[0], append(argv[1:], page)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		_ = os.Remove(page)
		return false
	}
	go func() { _ = cmd.Wait() }()
	time.AfterFunc(openFileKept, func() { removeOpenPage(page, link) })
	return true
}

// writeOpenPage writes the page that sends a browser on to link, readable
// by this user only, and answers its path.
func writeOpenPage(home, link string) (string, error) {
	page := filepath.Join(home, openFile)
	tmp := page + ".new"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(openPage(link)); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, page); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return page, nil
}

// removeOpenPage removes the page if it still sends the browser to link: a
// later start may have written its own.
func removeOpenPage(page, link string) {
	if b, err := os.ReadFile(page); err == nil && string(b) == openPage(link) {
		_ = os.Remove(page)
	}
}

// openPage is the page that sends the browser on to link, replacing itself
// in the history.
func openPage(link string) string {
	attr := html.EscapeString(link)
	js, _ := json.Marshal(link)
	return "<!doctype html>\n<html><head><meta charset=\"utf-8\"><meta name=\"referrer\" content=\"no-referrer\">" +
		"<meta http-equiv=\"refresh\" content=\"0;url=" + attr + "\"><title>AgentBus</title></head>\n" +
		"<body><script>location.replace(" + string(js) + ")</script>\n" +
		"<p><a href=\"" + attr + "\">Open the AgentBus cockpit</a></p></body></html>\n"
}
