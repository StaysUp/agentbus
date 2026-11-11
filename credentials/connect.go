package credentials

// Connecting a subscription: control runs the vendor's own login (such as
// claude setup-token), passes on what the person types (the code the
// browser shows), and keeps the token the login prints in Subscriptions. A
// login that keeps its sign-in in a file (codex login --device-auth) is read
// from a folder of control's own instead (signin.go).
//
// One rule keeps the token out of sight: only links leave the login, and a
// one-time code the person enters at the link, matched by its exact shape.
// Of everything it prints, the person is shown those alone; no other text,
// so no form of the token, reaches a reply, the terminal or a log.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Login is a harness's own login that prints a token, as its harness
// describes it (harness.Login).
type Login struct {
	Path string   // the program, found on control's PATH
	Args []string // such as setup-token
	Env  []string // the login's environment; nil: control's own
	// Prefix starts every token it prints; Token is the whole token.
	Prefix string
	Token  *regexp.Regexp
	// Valid is how long a token works, as the vendor says.
	Valid time.Duration
	// Refused matches a line the login prints when it did not take the
	// code and waits to be asked again. Nil: it ends by itself.
	Refused *regexp.Regexp
	// Code matches a whole line, spaces trimmed, that is a one-time code
	// the person enters at the link (a device login). It is shown; nothing
	// is typed back.
	Code *regexp.Regexp
	// Home, when set, names the variable that sets the folder the login
	// keeps its sign-in in. It is then read from File there, an OAuth
	// sign-in: TokenKey, RefreshKey and AccountKey are the dotted JSON
	// paths of the access token, the refresh token and the account. The
	// access token's own expiry says how long it works.
	Home, File                       string
	TokenKey, RefreshKey, AccountKey string
	// Keep, when set, keeps the login's folder as the harness's own home on
	// control, named Keep in the subscriptions folder, where the harness
	// renews its own sign-in (own.go). Only TokenKey and ExpiryKey, the
	// dotted path of the token's RFC 3339 expiry, are read from File.
	Keep, ExpiryKey string
	// Unset are variables the login runs without; Set are NAME=value pairs
	// it runs with.
	Unset, Set []string
	// OnlyOn is the one GOOS the login can be used on, or "" for any;
	// OnlyOnWhy says why, for the person.
	OnlyOn, OnlyOnWhy string
}

// usable says why l cannot be used on this machine, or "".
func (l Login) usable() string {
	if l.OnlyOn != "" && l.OnlyOn != runtime.GOOS {
		return l.OnlyOnWhy
	}
	return ""
}

// Connect states.
const (
	Running   = "running"
	Connected = "connected"
	Failed    = "failed"
)

// View is what may be shown of a connect: its state and, while it runs,
// the links the login printed. Once it ended, its links and one-time code
// are gone: only the state and why it failed are left.
type View struct {
	Harness string   `json:"harness" doc:"The harness being connected."`
	State   string   `json:"state" doc:"running, connected or failed."`
	Output  []string `json:"output" doc:"While it runs: the links the login printed, where to sign in; nothing else it prints is shown. Empty once it ended."`
	Code    string   `json:"code,omitempty" doc:"While it runs: a one-time code to enter at the link (codex's device login). Then nothing is sent back: the login ends by itself once the browser accepts it. Gone once it ended."`
	Paste   bool     `json:"paste" doc:"While it runs: the login waits for the code the browser shows, sent with POST .../connect/input (claude and agy). False for a device login, and once it ended."`
	Error   string   `json:"error,omitempty" doc:"Why it failed."`
}

// maxLines bounds the links kept.
const maxLines = 20

// escapes are terminal escape sequences, taken out before a token is looked
// for, so one inside a token does not hide it.
var escapes = regexp.MustCompile("\x1b(\\[[0-9;?]*[ -/]*[@-~]|\\][^\x07\x1b]*(\x07|\x1b\\\\)|[()*+./-][ -~]|[ -/]*[0-~])")

// link is a web address in what the login prints.
var link = regexp.MustCompile(`https?://[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]+`)

// connecting is one login running.
type connecting struct {
	harness string
	login   Login
	mu      sync.Mutex
	links   []string
	code    string // the one-time code it printed, to enter at the link
	text    []byte // what it printed, escapes taken out, for the token
	state   string
	why     string
	late    bool // it ran past the Manager's Wait and was stopped
	refused bool // it printed a Refused line
	// onRefused is called once, when the login prints a Refused line.
	onRefused func()
	tty       *os.File // the login's terminal: what is written is its typing
	cmd       *exec.Cmd
}

func (c *connecting) view() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return View{Harness: c.harness, State: c.state, Output: append([]string{}, c.links...), Code: c.code,
		Paste: c.state == Running && c.login.Code == nil, Error: c.why}
}

// take reads the login's output until it ends: each ended line's links are
// kept for showing while it runs (a link that names the token's prefix
// never is), and the text, for the token. It calls shown with each new link.
func (c *connecting) take(r io.Reader, shown func(string)) {
	var line []byte
	buf := make([]byte, 4096)
	end := func() {
		plain := escapes.ReplaceAll(line, nil)
		c.mu.Lock()
		c.text = append(append(c.text, plain...), '\n')
		refused := !c.refused && c.onRefused != nil && c.login.Refused != nil && c.login.Refused.Match(plain)
		if refused {
			c.refused = true
		}
		var added []string
		// A connect that ended shows nothing more, even when its login
		// still prints.
		ended := c.state == Connected || c.state == Failed
		if t := bytes.TrimSpace(plain); !ended && c.login.Code != nil && c.code == "" && c.login.Code.Match(t) {
			c.code = string(t)
			added = append(added, c.code)
		}
		for _, l := range link.FindAll(plain, -1) {
			if !ended && (c.login.Prefix == "" || !bytes.Contains(l, []byte(c.login.Prefix))) && len(c.links) < maxLines {
				c.links = append(c.links, string(l))
				added = append(added, string(l))
			}
		}
		c.mu.Unlock()
		for _, l := range added {
			shown(l)
		}
		if refused {
			c.onRefused()
		}
		line = line[:0]
	}
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if b == '\n' || b == '\r' {
				end()
			} else {
				line = append(line, b)
			}
		}
		if err != nil {
			break
		}
	}
	end()
}

// Manager runs at most one login per harness, and keeps what they print.
// Each login runs in its own terminal (startInPty), as when a person runs
// it by hand.
type Manager struct {
	Store  *Subscriptions
	Logins map[string]Login // by harness; one missing has no login here
	// Wait bounds how long a login may run, such as one left waiting for a
	// code no one sends. Zero: MaxWait.
	Wait time.Duration
	// Logf writes one line to the service log for each step of a connect:
	// started, link or code shown, input received, connected or failed. Never a
	// link, a code or a token. Nil: none.
	Logf func(format string, args ...any)
	// Ended is told how each connect ended: Connected, or Failed and why.
	// Nil: no one.
	Ended func(harness, state, why string)

	mu      sync.Mutex
	running map[string]*connecting
}

// MaxWait is how long a login may run by default.
const MaxWait = 10 * time.Minute

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// ErrNoLogin is a harness whose subscription cannot be connected here.
var ErrNoLogin = errors.New("credentials: no login for this harness")

// notStarted says why l could not be started, in fixed wording: a program
// not found names itself and the PATH searched.
func notStarted(l Login, err error) error {
	if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
		return &Missing{Why: "the login " + filepath.Base(l.Path) + " could not be started"}
	}
	// exec.Command looks a bare name up on this process's PATH.
	name := filepath.Base(l.Path)
	return &Missing{Why: name + " is not on PATH (searched: " + os.Getenv("PATH") + "); add the folder that holds " + name + " to PATH"}
}

// Connect starts harness's login, or answers the one running. A login that
// ended is started again. A login that cannot be started is a *Missing.
func (m *Manager) Connect(harness string) (View, error) {
	l, ok := m.Logins[harness]
	if !ok {
		return View{}, ErrNoLogin
	}
	if why := l.usable(); why != "" {
		return View{}, &Missing{Why: why}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.running[harness]; c != nil && c.view().State == Running {
		return c.view(), nil
	}
	c := &connecting{harness: harness, login: l, state: Running}
	cmd := exec.Command(l.Path, l.Args...)
	env, home, err := l.home(m.Store.Dir)
	if err != nil {
		return View{}, err
	}
	cmd.Env = env
	tty, err := startInPty(cmd)
	if err != nil {
		removeHome(home)
		e := notStarted(l, err)
		m.logf("subscription.failed harness=%s why=%q", harness, e.Error())
		return View{}, e
	}
	m.logf("subscription.login_started harness=%s", harness)
	c.cmd, c.tty = cmd, tty
	c.onRefused = func() { stopGroup(cmd) }
	if m.running == nil {
		m.running = map[string]*connecting{}
	}
	m.running[harness] = c
	wait := m.Wait
	if wait == 0 {
		wait = MaxWait
	}
	late := time.AfterFunc(wait, func() {
		c.mu.Lock()
		c.late = true
		c.mu.Unlock()
		stopGroup(cmd)
	})
	read := make(chan struct{})
	go func() {
		c.take(tty, func(s string) {
			if s == c.view().Code {
				m.logf("subscription.code_shown harness=%s", harness)
			} else {
				m.logf("subscription.link_shown harness=%s", harness)
			}
		})
		close(read)
	}()
	go func() {
		err := cmd.Wait()
		late.Stop()
		// What it printed last may still be in the terminal. A child it
		// left holding the terminal is stopped after a moment, and given
		// up on after another.
		select {
		case <-read:
		case <-time.After(2 * time.Second):
			stopGroup(cmd)
			select {
			case <-read:
			case <-time.After(2 * time.Second):
			}
		}
		tty.Close()
		c.mu.Lock()
		si, ok := l.signin(c.text, home, m.Store.now())
		c.text = nil
		wasLate, wasRefused := c.late, c.refused
		c.mu.Unlock()
		state, why := Failed, ""
		if ok {
			// Keep takes a kept login's folder; what is left is removed.
			if m.Store.Keep(harness, si) != nil {
				why = "the token could not be kept in the data folder"
			} else {
				state = Connected
			}
		}
		removeHome(home)
		switch {
		case ok:
		case wasRefused:
			why = "the login did not accept the code: it may be incomplete, expired, already used, or from an earlier sign-in; connect again for a new link"
		case wasLate:
			why = "the login waited too long (" + wait.String() + ") and was stopped; connect again"
		case err != nil:
			why = l.ended() + " (exit " + strconv.Itoa(cmd.ProcessState.ExitCode()) + ")"
		default:
			why = l.ended()
		}
		// Logged and recorded before it shows, so a client that sees the
		// end finds it in the record.
		if state == Connected {
			m.logf("subscription.connected harness=%s", harness)
		} else {
			m.logf("subscription.failed harness=%s why=%q", harness, why)
		}
		if m.Ended != nil {
			m.Ended(harness, state, why)
		}
		// Ended, its links and one-time code are used up: the state is
		// what is left to show.
		c.mu.Lock()
		c.state, c.why = state, why
		c.links, c.code = nil, ""
		c.mu.Unlock()
	}()
	return c.view(), nil
}

// View is harness's login as it is now. ok is false when none was started
// since control started.
func (m *Manager) View(harness string) (View, bool) {
	m.mu.Lock()
	c := m.running[harness]
	m.mu.Unlock()
	if c == nil {
		return View{}, false
	}
	return c.view(), true
}

// Input types one line into harness's running login, such as the code the
// browser showed, and presses Enter. It is never kept or shown.
func (m *Manager) Input(harness, line string) error {
	m.mu.Lock()
	c := m.running[harness]
	m.mu.Unlock()
	if c == nil || c.view().State != Running {
		return errors.New("credentials: no login is running")
	}
	// Enter is a carriage return, as a terminal sends it: a login that
	// reads the terminal raw (claude's) takes it as Enter, and one that
	// reads it by lines gets a newline.
	if _, err := io.WriteString(c.tty, line+"\r"); err != nil {
		return err
	}
	m.logf("subscription.input_received harness=%s", harness)
	return nil
}

// Stop stops every login still running, when control stops.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.running {
		if c.view().State == Running {
			stopGroup(c.cmd)
		}
	}
}

// Run runs l in a terminal: in is the person's typing; out is shown the
// links the login prints, and a prompt for the code or the one-time code to
// enter, never anything else. dir is the subscriptions folder, where a login
// that keeps its sign-in in a file gets its own folder. It returns the
// sign-in the login gave. When ctx ends (the person stopped it), the login
// is stopped and its folder removed.
func Run(ctx context.Context, l Login, dir string, now time.Time, in io.Reader, out io.Writer) (Signin, error) {
	if why := l.usable(); why != "" {
		return Signin{}, errors.New(why)
	}
	c := &connecting{login: l}
	cmd := exec.CommandContext(ctx, l.Path, l.Args...)
	// Stopped, the login is asked to end, as Ctrl-C asks it, and a child it
	// left holding its output no longer holds this read.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	env, home, err := l.home(dir)
	if err != nil {
		return Signin{}, err
	}
	// A kept login's folder goes to the caller with the sign-in, for
	// Subscriptions.Keep; any other is removed here.
	handed := false
	defer func() {
		if !handed {
			removeHome(home)
		}
	}()
	cmd.Env, cmd.Stdin = env, in
	r, w, err := os.Pipe()
	if err != nil {
		return Signin{}, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return Signin{}, notStarted(l, err)
	}
	w.Close()
	unread := context.AfterFunc(ctx, func() { r.Close() })
	defer unread()
	c.take(r, func(s string) {
		switch {
		case s == c.view().Code:
			io.WriteString(out, "Enter this one-time code there: "+s+"\nThe login ends by itself once the browser accepts it.\n")
		case l.Code != nil:
			io.WriteString(out, "Sign in here: "+s+"\n")
		default:
			io.WriteString(out, "Sign in here: "+s+"\nThen paste the code the browser shows, and press Enter.\n")
		}
	})
	r.Close()
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return Signin{}, errors.New("the login was stopped")
	}
	si, ok := l.signin(c.text, home, now)
	if !ok {
		if werr != nil {
			return Signin{}, errors.New(l.ended() + " (exit " + strconv.Itoa(cmd.ProcessState.ExitCode()) + ")")
		}
		return Signin{}, errors.New(l.ended())
	}
	handed = si.Home != ""
	return si, nil
}

// removeHome removes a login's own folder, which may hold a copy of the
// sign-in; "" is none.
func removeHome(home string) {
	if home != "" {
		_ = os.RemoveAll(home)
	}
}
