package credentials

// Connecting a subscription: control runs the vendor's own login (such as
// claude setup-token), passes on what the person types (the code the
// browser shows), and keeps the token the login prints in Subscriptions.
//
// One rule keeps the token out of sight: only links leave the login. Of
// everything it prints, the person is shown the web addresses alone, where
// they sign in; no other text, so no form of the token, reaches a reply,
// the terminal or a log.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
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
}

// Connect states.
const (
	Running   = "running"
	Connected = "connected"
	Failed    = "failed"
)

// View is what may be shown of a connect: its state and the links the
// login printed.
type View struct {
	Harness string   `json:"harness" doc:"The harness being connected."`
	State   string   `json:"state" doc:"running, connected or failed."`
	Output  []string `json:"output" doc:"The links the login printed, where to sign in; nothing else it prints is shown. Send the code the browser shows with POST .../connect/input."`
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
	text    []byte // what it printed, escapes taken out, for the token
	state   string
	why     string
	stdin   io.WriteCloser
	cmd     *exec.Cmd
}

func (c *connecting) view() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return View{Harness: c.harness, State: c.state, Output: append([]string{}, c.links...), Error: c.why}
}

// take reads the login's output until it ends: each ended line's links are
// kept for showing (a link that names the token's prefix never is), and
// the text, for the token. It calls shown with each new link.
func (c *connecting) take(r io.Reader, shown func(string)) {
	var line []byte
	buf := make([]byte, 4096)
	end := func() {
		plain := escapes.ReplaceAll(line, nil)
		c.mu.Lock()
		c.text = append(append(c.text, plain...), '\n')
		var added []string
		for _, l := range link.FindAll(plain, -1) {
			if !bytes.Contains(l, []byte(c.login.Prefix)) && len(c.links) < maxLines {
				c.links = append(c.links, string(l))
				added = append(added, string(l))
			}
		}
		c.mu.Unlock()
		for _, l := range added {
			shown(l)
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

// token is the token the login printed, or "".
func (c *connecting) token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.login.Token.Find(c.text))
}

// Manager runs at most one login per harness, and keeps what they print.
type Manager struct {
	Store  *Subscriptions
	Logins map[string]Login // by harness; one missing has no login here

	mu      sync.Mutex
	running map[string]*connecting
}

// ErrNoLogin is a harness whose subscription cannot be connected here.
var ErrNoLogin = errors.New("credentials: no login for this harness")

// Connect starts harness's login, or answers the one running. A login that
// ended is started again.
func (m *Manager) Connect(harness string) (View, error) {
	l, ok := m.Logins[harness]
	if !ok {
		return View{}, ErrNoLogin
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.running[harness]; c != nil && c.view().State == Running {
		return c.view(), nil
	}
	c := &connecting{harness: harness, login: l, state: Running}
	cmd := exec.Command(l.Path, l.Args...)
	cmd.Env = l.Env
	r, w, err := os.Pipe()
	if err != nil {
		return View{}, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	if c.stdin, err = cmd.StdinPipe(); err != nil {
		r.Close()
		w.Close()
		return View{}, err
	}
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return View{}, errors.New("credentials: the login could not be started")
	}
	w.Close() // the login holds its own copy
	c.cmd = cmd
	if m.running == nil {
		m.running = map[string]*connecting{}
	}
	m.running[harness] = c
	read := make(chan struct{})
	go func() { c.take(r, func(string) {}); r.Close(); close(read) }()
	go func() {
		err := cmd.Wait()
		// What it printed last may still be in the pipe; a child it left
		// holding the pipe is given up on after a moment.
		select {
		case <-read:
		case <-time.After(2 * time.Second):
		}
		tok := c.token()
		c.mu.Lock()
		c.text = nil
		c.mu.Unlock()
		state, why := Failed, ""
		switch {
		case tok != "":
			if m.Store.Put(harness, tok, l.Valid) != nil {
				why = "the token could not be kept in the data folder"
			} else {
				state = Connected
			}
		case err != nil:
			why = "the login ended without a token (exit " + strconv.Itoa(cmd.ProcessState.ExitCode()) + ")"
		default:
			why = "the login ended without printing a token"
		}
		c.mu.Lock()
		c.state, c.why = state, why
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

// Input sends one line to harness's running login, such as the code the
// browser showed. It is never kept or shown.
func (m *Manager) Input(harness, line string) error {
	m.mu.Lock()
	c := m.running[harness]
	m.mu.Unlock()
	if c == nil || c.view().State != Running {
		return errors.New("credentials: no login is running")
	}
	_, err := io.WriteString(c.stdin, line+"\n")
	return err
}

// Stop stops every login still running, when control stops.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.running {
		if c.view().State == Running && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
}

// Run runs l in a terminal: in is the person's typing; out is shown the
// links the login prints, and a prompt for the code, never anything else.
// It returns the token the login printed.
func Run(l Login, in io.Reader, out io.Writer) (string, error) {
	c := &connecting{login: l}
	cmd := exec.Command(l.Path, l.Args...)
	cmd.Env, cmd.Stdin = l.Env, in
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return "", errors.New("the login could not be started: is the harness's program on PATH?")
	}
	w.Close()
	c.take(r, func(l string) {
		io.WriteString(out, "Sign in here: "+l+"\nThen paste the code the browser shows, and press Enter.\n")
	})
	r.Close()
	werr := cmd.Wait()
	tok := c.token()
	if tok == "" {
		if werr != nil {
			return "", errors.New("the login ended without a token (exit " + strconv.Itoa(cmd.ProcessState.ExitCode()) + ")")
		}
		return "", errors.New("the login ended without printing a token")
	}
	return tok, nil
}
