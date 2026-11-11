package credentials

// A sign-in the harness keeps and renews itself, in a home of control's own
// (agy's Google sign-in). The login's folder is kept as that home, as the
// harness wrote it (Login.Keep), and control never holds a second copy:
//
//   - <harness>.json says it is connected, when, and whether a renewal was
//     refused. It holds no token.
//   - The harness's own file in the home holds the sign-in. Control reads
//     only its access token and the token's expiry from it, never the
//     refresh token.
//   - When less than Own.Within is left, control runs the unmodified
//     harness in that home (Own.Args, such as agy models), and the harness
//     renews its own file. Nothing of the vendor's renewal is done by
//     control, and the harness's client secret is never read.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Own is how one harness's own sign-in is kept and renewed.
type Own struct {
	// Home is the home's folder name in the subscriptions folder; File the
	// sign-in's file in it; TokenKey and ExpiryKey the dotted JSON paths of
	// the access token and its RFC 3339 expiry.
	Home, File, TokenKey, ExpiryKey string
	// Within: the harness renews its sign-in only when less than this is
	// left, so control runs it then.
	Within time.Duration
	// Path and Args run the harness so it renews its sign-in, with HOME the
	// home, the variables of Set and none of Unset. Env is the environment it starts from;
	// nil: control's own.
	Path       string
	Args       []string
	Env        []string
	Unset, Set []string
	// Refused matches what it prints when the vendor refused the renewal.
	Refused *regexp.Regexp
	// Wait bounds one renewal. Zero: ownWait.
	Wait time.Duration
}

// ownWait bounds a renewal by default. agy models took under two seconds.
const ownWait = time.Minute

// renewOut bounds what a renewal's output is matched in.
const renewOut = 64 << 10

func (s *Subscriptions) home(o Own) string { return filepath.Join(s.Dir, o.Home) }

// ownSignin reads the access token and its expiry from o's file in its home.
// It holds the home's read lock: a renewal writes the file under the write
// lock.
func (s *Subscriptions) ownSignin(o Own) (string, time.Time, error) {
	s.homes.RLock()
	defer s.homes.RUnlock()
	b, err := os.ReadFile(filepath.Join(s.home(o), o.File))
	if err != nil {
		return "", time.Time{}, errOwnUnusable
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return "", time.Time{}, errOwnUnusable
	}
	tok, exp, ok := ownFields(v, o.TokenKey, o.ExpiryKey)
	if !ok {
		return "", time.Time{}, errOwnUnusable
	}
	return tok, exp, nil
}

var errOwnUnusable = errors.New("credentials: the sign-in in the harness's home on control cannot be used; connect again")

// keepHome makes home, a login's folder in the subscriptions folder, o's
// home, in place of the one there before (a new connect replaces the old
// sign-in). The caller holds s.mu.
func (s *Subscriptions) keepHome(o Own, home string) error {
	if home == "" || filepath.Dir(home) != filepath.Clean(s.Dir) {
		return errors.New("credentials: the login's folder is not in the subscriptions folder")
	}
	s.homes.Lock()
	defer s.homes.Unlock()
	dst := s.home(o)
	old := ""
	if _, err := os.Lstat(dst); err == nil {
		f, err := os.MkdirTemp(s.Dir, ".old-*")
		if err != nil {
			return errors.New("credentials: the old sign-in could not be moved aside")
		}
		old = filepath.Join(f, "home")
		if os.Rename(dst, old) != nil {
			_ = os.Remove(f)
			return errors.New("credentials: the old sign-in could not be moved aside")
		}
		defer os.RemoveAll(f)
	}
	if err := os.Rename(home, dst); err != nil {
		if old != "" {
			_ = os.Rename(old, dst)
		}
		return errors.New("credentials: the sign-in could not be kept")
	}
	return nil
}

// removeHome removes o's home: the person disconnected it. The caller holds
// s.mu.
func (s *Subscriptions) removeOwnHome(o Own) error {
	s.homes.Lock()
	defer s.homes.Unlock()
	if err := os.RemoveAll(s.home(o)); err != nil {
		return errors.New("credentials: the subscription could not be removed")
	}
	return nil
}

// errRenewFailed is a renewal that did not renew the sign-in, for now.
var errRenewFailed = errors.New("credentials: the harness did not renew its sign-in just now")

// runOwn runs the harness in o's home so it renews its own sign-in. refused
// says it printed o.Refused. Its output is only matched, never kept or
// shown: it may name the account.
func (s *Subscriptions) runOwn(o Own) (refused bool, err error) {
	wait := o.Wait
	if wait == 0 {
		wait = ownWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	l := Login{Home: "HOME", Env: o.Env, Unset: o.Unset, Set: o.Set}
	cmd := exec.CommandContext(ctx, o.Path, o.Args...)
	cmd.Env = l.envWith(s.home(o))
	cmd.Dir = s.home(o)
	// Its own process group, so a timeout stops whatever it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var out bytes.Buffer
	w := &capped{b: &out, n: renewOut}
	cmd.Stdout, cmd.Stderr = w, w
	s.homes.Lock()
	err = cmd.Run()
	s.homes.Unlock()
	if o.Refused != nil && o.Refused.Match(out.Bytes()) {
		return true, nil
	}
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return false, errors.New("credentials: the harness could not be started to renew its sign-in; is it on control's PATH?")
		}
		return false, errRenewFailed
	}
	return false, nil
}

// capped writes to b up to n bytes and drops the rest.
type capped struct {
	b *bytes.Buffer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.n - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

var _ io.Writer = (*capped)(nil)

// renewOwn renews harness's own sign-in when it is still due once no other
// renewal runs. It returns the sign-in as kept after, and whether it ran the
// harness. The caller holds s.renewing.
func (s *Subscriptions) renewOwn(harness string, o Own, k kept) (kept, bool, error) {
	refused, err := s.runOwn(o)
	after, rerr := s.read(harness)
	switch {
	case errors.Is(rerr, ErrNotConnected):
		return after, true, rerr // disconnected meanwhile
	case rerr == nil && after.ExpiresAt.After(k.ExpiresAt):
		next := after
		next.RenewedAt = s.now().UTC()
		return s.settle(harness, k, next, nil)
	case refused:
		next := k
		next.Refused = true
		return s.settle(harness, k, next, errRefused)
	case err != nil:
		return k, true, err
	}
	return k, true, errRenewFailed
}
