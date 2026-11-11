package run

// A shell command the person runs in a session's folder, with no model call
// (POST /api/v0/sessions/{id}/commands): the "!" of a chat. It runs where the
// session's turns run, in the session's own home when it has one, and closed
// off from the network when the policy says so, as a turn is. The agents'
// allow and forbid lists are for agents, so they do not apply: it is the
// person's own command. It gets no API token and no model credential.
//
// Each command is a command.ran event in the record. The session's next turn
// gets the commands since its previous turn in its prompt, once (shellContext).

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/netjail"
)

// The limits of a command.
const (
	// MaxCommandLine bounds a command line, in bytes.
	MaxCommandLine = 8 << 10
	// CommandTimeout is a command's time limit when the request names none;
	// MaxCommandTimeout is the most a request may name.
	CommandTimeout    = 2 * time.Minute
	MaxCommandTimeout = 10 * time.Minute
	// CommandOutputMax bounds what is kept of each of stdout and stderr: its
	// start. The rest is counted and dropped.
	CommandOutputMax = 64 << 10
)

// CheckCommand says what is wrong with a command line, or "".
func CheckCommand(line string) string {
	if strings.TrimSpace(line) == "" || len(line) > MaxCommandLine || strings.IndexByte(line, 0) >= 0 {
		return "command must be from 1 byte to 8 KiB, not only spaces, with no NUL byte"
	}
	return ""
}

// Command is one command to run.
type Command struct {
	// Session is the session it runs in, and Folder where: the session's
	// workspace on this runner.
	Session, Folder string
	// ID is the command's id, which its command.ran carries.
	ID   string
	Line string
	// Timeout is its time limit; zero means CommandTimeout.
	Timeout time.Duration
}

// RunCommand runs c and stores its command.ran, which it returns. It returns
// once the command and everything in its process group have ended, or ctx
// ended it. A *Refusal says why nothing ran: the folder is not there, or the
// policy asks for network control this runner cannot give.
func (e *Engine) RunCommand(ctx context.Context, c Command) (event.Event, error) {
	if msg := CheckCommand(c.Line); msg != "" {
		return event.Event{}, refuse(http.StatusBadRequest, msg)
	}
	if !SessionShape.MatchString(c.Session) {
		return event.Event{}, refuse(http.StatusBadRequest, "session must match "+SessionShape.String())
	}
	if c.Timeout <= 0 {
		c.Timeout = CommandTimeout
	}
	if c.Timeout > MaxCommandTimeout {
		return event.Event{}, refuse(http.StatusBadRequest, "the time limit is at most 10 minutes")
	}
	if !isDir(c.Folder) {
		return event.Event{}, refuse(http.StatusConflict, "the session's folder is not on its runner, so the command did not run")
	}
	perm, err := e.policy()
	if err != nil {
		return event.Event{}, e.unusable("shell", "permissions_file", err)
	}
	cmd := exec.Command("/bin/sh", "-c", c.Line)
	cmd.Dir = c.Folder
	cmd.Env = commandEnv(os.Environ())
	if e.cfg.Homes != "" {
		// The session's own home, as its turns get it. A session started
		// before homes has none; its turns, and so its commands, run with
		// this runner's.
		dir := filepath.Join(e.cfg.Homes, c.Session)
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			cmd.Env = slices.DeleteFunc(cmd.Env, func(kv string) bool {
				k, _, _ := strings.Cut(kv, "=")
				return slices.Contains(homeVars, k)
			})
			cmd.Env = append(cmd.Env, "HOME="+dir)
		}
	}
	var jail *netjail.Jail
	if on, why := e.contained(perm); why != "" {
		return event.Event{}, refuse(http.StatusBadRequest, strings.ReplaceAll(why, "the turn did not start", "the command did not run"))
	} else if on {
		session := c.Session
		jail = netjail.New(netjail.Spec{Allow: perm.Network.Allow, Refused: func(target, why string) {
			if _, _, err := e.cfg.Store.Append(event.Event{ID: newEventID(), Kind: event.NetworkRefused,
				Time: e.cfg.Now().UTC(), Session: session, Host: target, Error: why}); err != nil {
				e.cfg.Logf("store.error what=network_refused session=%s", session)
			}
		}})
		cmd.Env = setEnv(cmd.Env, jail.Env())
		defer jail.Close()
	}
	// Its own session, so its process tree is tracked as a turn's is
	// (procTree), even what calls setsid. Stdin stays empty. The output goes
	// through pipes of our own, so the wait ends when its process exits,
	// not when the last process holding its output closes it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, stderr := &capped{max: CommandOutputMax}, &capped{max: CommandOutputMax}
	pipes, err := newOutputPipes()
	if err != nil {
		return event.Event{}, refuse(http.StatusInternalServerError, "the command could not be started")
	}
	cmd.Stdout, cmd.Stderr = pipes.outW, pipes.errW
	start := cmd.Start
	if jail != nil {
		start = func() error { return jail.Start(cmd) }
	}
	begun := e.cfg.Now()
	// The session's slot, as a turn takes it: one turn or command of a
	// session runs on this engine at a time, whatever the queue believes.
	if code, msg := e.runs.take(c.Session); code != 0 {
		pipes.closeWriters()
		pipes.closeReaders()
		return event.Event{}, refuse(code, msg)
	}
	id := newEventID()
	code, msg, lt := e.runs.start(cmd, start, c.Session, id)
	pipes.closeWriters() // the child holds its own copies
	if code != 0 {
		pipes.closeReaders()
		e.runs.release(nil, c.Session)
		return event.Event{}, refuse(code, strings.ReplaceAll(msg, "the agent could not be started", "the command could not be started"))
	}
	copied := pipes.copy(stdout, stderr)
	e.cfg.Logf("command.start session=%s pid=%d", c.Session, cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	limit := time.NewTimer(c.Timeout)
	ran := event.Command{ID: c.ID, Line: c.Line}
	var werr error
	select {
	case werr = <-done:
	case <-limit.C:
		ran.TimedOut = true
	case <-ctx.Done():
	}
	limit.Stop()
	if ran.TimedOut || ctx.Err() != nil {
		lt.tree.signal(syscall.SIGKILL)
		werr = <-done
	}
	lt.exit()
	// A command ends with everything it started.
	lt.tree.exited()
	lt.tree.end(time.Second)
	copied(OutputGrace)
	e.runs.release(cmd, c.Session)
	ran.ExitCode = ExitCode(cmd.ProcessState, werr)
	ran.Stdout, ran.Stderr = stdout.text(), stderr.text()
	ran.OutputBytes = stdout.n + stderr.n
	ran.Truncated = stdout.cut() || stderr.cut()
	ended := e.cfg.Now()
	ran.DurationMS = ended.Sub(begun).Milliseconds()
	e.cfg.Logf("command.exit session=%s exit=%d timed_out=%t duration=%s", c.Session, ran.ExitCode, ran.TimedOut,
		ended.Sub(begun).Round(time.Millisecond))
	ev := event.Event{ID: id, Kind: event.CommandRan, Time: ended.UTC(), Session: c.Session, Command: &ran}
	out, _, err := e.cfg.Store.Append(ev)
	if err != nil {
		return event.Event{}, storeFailed
	}
	return out, nil
}

// commandEnv is the environment a command gets: this runner's, as a turn's
// is, without the AgentBus variables a turn is given. A command has no API
// token.
func commandEnv(parent []string) []string {
	return slices.DeleteFunc(slices.Clone(parent), func(kv string) bool {
		return strings.HasPrefix(kv, "AGENTBUS_")
	})
}

// capped keeps the first max bytes written to it and counts them all.
type capped struct {
	max int
	b   []byte
	n   int64
}

func (c *capped) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	if room := c.max - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (c *capped) cut() bool { return c.n > int64(len(c.b)) }

// text is what was kept, as UTF-8 with no NUL: a character the cut split,
// bytes that are not UTF-8, and NUL bytes are replaced, so the text can go
// in a next turn's prompt, which a program takes as an argument.
func (c *capped) text() string {
	return strings.ReplaceAll(strings.ToValidUTF8(string(c.b), "\uFFFD"), "\x00", "\uFFFD")
}

// The bounds of the commands a next turn is told of.
const (
	// ShellContextMax bounds the whole text: the newest commands that fit.
	ShellContextMax = 32 << 10
	// ShellStreamMax bounds each command's stdout and stderr in it.
	ShellStreamMax = 8 << 10
)

const shellHead = "The person ran these shell commands in this session's folder since your previous turn. " +
	"You did not run them. Each is shown with its exit code and output."

// shellContext is the text that tells a session's next turn what the person
// ran since its previous turn, or "" when nothing. room is what the prompt
// has left. The same for every harness: it goes before the person's words.
func (e *Engine) shellContext(session string, room int) (string, error) {
	cmds, err := e.cfg.Store.CommandsSince(session)
	if err != nil || len(cmds) == 0 {
		return "", err
	}
	budget := min(ShellContextMax, room) - len(shellHead) - 128
	var parts []string
	left := 0
	// The newest first, so the budget keeps them; then oldest first.
	for i := len(cmds) - 1; i >= 0; i-- {
		p := shellEntry(*cmds[i].Command)
		if len(p) > budget {
			left = i + 1
			break
		}
		budget -= len(p) + 1
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "", nil
	}
	slices.Reverse(parts)
	text := shellHead + "\n\n"
	if left > 0 {
		text += fmt.Sprintf("(%d earlier commands are left out.)\n\n", left)
	}
	return text + strings.Join(parts, "\n") + "\n", nil
}

// shellEntry is one command as the next turn sees it.
func shellEntry(c event.Command) string {
	var b strings.Builder
	b.WriteString("<shell-command>" + c.Line + "</shell-command>\n")
	status := fmt.Sprintf("exit code %d", c.ExitCode)
	if c.TimedOut {
		status += ", ended at its time limit"
	}
	if c.Truncated {
		status += ", output cut at 64 KiB"
	}
	b.WriteString("<shell-status>" + status + "</shell-status>\n")
	for _, s := range []struct{ tag, text string }{{"shell-stdout", c.Stdout}, {"shell-stderr", c.Stderr}} {
		if s.text == "" {
			continue
		}
		t := s.text
		if len(t) > ShellStreamMax {
			t = strings.ToValidUTF8(t[:ShellStreamMax], "") + "\n[cut]"
		}
		b.WriteString("<" + s.tag + ">\n" + strings.TrimSuffix(t, "\n") + "\n</" + s.tag + ">\n")
	}
	return b.String()
}
