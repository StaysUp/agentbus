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
// so no form of the token, reaches a reply or a log.

import (
	"bytes"
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
	State   string   `json:"state" doc:"running, connected or failed. A login still running after 10 minutes is stopped and fails."`
	Output  []string `json:"output" doc:"While it runs: the links the login printed, where to sign in; nothing else it prints is shown. Empty once it ended."`
	Code    string   `json:"code,omitempty" doc:"While it runs: a one-time code to enter at the link (codex's device login). Then nothing is sent back: the login ends by itself once the browser accepts it. Gone once it ended."`
	Paste   bool     `json:"paste" doc:"While it runs: the login waits for the code the browser shows, sent with POST .../connect/input (claude and agy). False for a device login, and once it ended."`
	// Rejected and Cancelled say why a failed connect ended, for a client
	// that words it itself; Error says it in full.
	Rejected  bool   `json:"rejected,omitempty" doc:"It failed because the login did not take the code sent: it said so, or gave no token within 30 seconds of it. Connect again for a new link."`
	Cancelled bool   `json:"cancelled,omitempty" doc:"It failed because it was cancelled (DELETE .../connect)."`
	Error     string `json:"error,omitempty" doc:"Why it failed."`
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
	// unanswered: it was still running AnswerWait after a code was typed,
	// and was stopped. claude setup-token 2.1.321 (2026-11-12) given a code
	// with the right state and a wrong value printed nothing and kept
	// running; the person saw "Checking it." until Wait.
	unanswered bool
	cancelled  bool        // Cancel stopped it
	exited     bool        // the login's process ended
	ending     bool        // its end is being decided: too late to cancel
	answer     *time.Timer // stops it AnswerWait after the latest code
	// onRefused is called once, when the login prints a Refused line.
	onRefused func()
	tty       *os.File // the login's terminal: what is written is its typing
	cmd       *exec.Cmd
}

func (c *connecting) view() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return View{Harness: c.harness, State: c.state, Output: append([]string{}, c.links...), Code: c.code,
		Paste: c.state == Running && c.login.Code == nil, Error: c.why,
		Rejected: c.state == Failed && (c.refused || c.unanswered) && !c.cancelled, Cancelled: c.state == Failed && c.cancelled}
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
	// AnswerWait bounds how long a login may keep running after a code is
	// typed into it: a login that took the code prints its token and ends
	// within a few seconds. Past it, the code is taken as rejected and the
	// login is stopped. Zero: DefaultAnswerWait.
	AnswerWait time.Duration
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

// DefaultAnswerWait is how long a login may run after a code by default.
const DefaultAnswerWait = 30 * time.Second

// rejected is why a connect whose login did not take the code failed.
const rejected = "the code was rejected: it may be incomplete, expired, already used, or from an earlier sign-in; try again, Connect gives a new link"

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
		// Ended by itself: what it printed decides, not the wait after a
		// code, which stops counting now.
		c.mu.Lock()
		c.exited = true
		if c.answer != nil {
			c.answer.Stop()
		}
		c.mu.Unlock()
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
		wasLate, wasRefused, wasUnanswered, wasCancelled := c.late, c.refused, c.unanswered, c.cancelled
		// From here a Cancel finds nothing to stop: what was read above
		// decides how the connect ends.
		c.ending = true
		c.mu.Unlock()
		state, why := Failed, ""
		// A cancelled connect keeps nothing, even a token the login printed
		// just before: the person asked for it to stop.
		ok = ok && !wasCancelled
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
		case wasCancelled:
			why = "the connect was cancelled"
		case wasRefused:
			why = rejected
		case wasUnanswered:
			why = rejected + " (the login gave no token within " + m.answerWait().String() + " of the code)"
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
		return ErrNotRunning
	}
	// Enter is a carriage return, as a terminal sends it: a login that
	// reads the terminal raw (claude's) takes it as Enter, and one that
	// reads it by lines gets a newline.
	if _, err := io.WriteString(c.tty, line+"\r"); err != nil {
		return err
	}
	m.logf("subscription.input_received harness=%s", harness)
	// A login that took the code ends soon: one still running past
	// AnswerWait did not take it. A next code starts the wait again.
	c.mu.Lock()
	if c.answer != nil {
		c.answer.Stop()
	}
	c.answer = time.AfterFunc(m.answerWait(), func() {
		c.mu.Lock()
		running := c.state == Running && !c.exited
		if running {
			c.unanswered = true
		}
		c.mu.Unlock()
		if running {
			stopGroup(c.cmd)
		}
	})
	c.mu.Unlock()
	return nil
}

func (m *Manager) answerWait() time.Duration {
	if m.AnswerWait > 0 {
		return m.AnswerWait
	}
	return DefaultAnswerWait
}

// ErrNotRunning is a cancel or an input with no login running.
var ErrNotRunning = errors.New("credentials: no login is running")

// Cancel stops harness's running login. The connect then fails as
// cancelled, keeping nothing, and a new Connect starts a new login. A login
// whose end is already being decided is not running: ErrNotRunning.
func (m *Manager) Cancel(harness string) error {
	m.mu.Lock()
	c := m.running[harness]
	m.mu.Unlock()
	if c == nil {
		return ErrNotRunning
	}
	c.mu.Lock()
	running := c.state == Running && !c.ending
	if running {
		c.cancelled = true
	}
	c.mu.Unlock()
	if !running {
		return ErrNotRunning
	}
	stopGroup(c.cmd)
	m.logf("subscription.cancelled harness=%s", harness)
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

// removeHome removes a login's own folder, which may hold a copy of the
// sign-in; "" is none.
func removeHome(home string) {
	if home != "" {
		_ = os.RemoveAll(home)
	}
}
