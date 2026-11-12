package credentials

// The person's own subscriptions, for runners they own: each harness's
// sign-in, connected once with the vendor's own login (Manager) and kept in
// control's data folder, one file per harness, readable by its owner only.
// Never the Mac Keychain, never a runner. Used for turns on every approved
// runner: a machine joins only when the person approves its code.
//
// Three kinds of sign-in. A long-lived token works until it expires (claude
// setup-token: a year). An OAuth sign-in (codex's ChatGPT sign-in) is an
// access token that expires soon, with a refresh token that gets a new one:
// control renews it itself, by the vendor's own rule, before it expires. A
// sign-in the harness keeps in a home of control's own (agy's) is renewed by
// the harness itself, which control runs there (own.go).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Subscriptions is the folder that holds them.
type Subscriptions struct {
	Dir string
	Now func() time.Time // nil: time.Now
	// OAuth is how each harness with an OAuth sign-in renews it. A harness
	// not in it keeps its token until the token expires.
	OAuth map[string]OAuth
	// Client makes the renewals' calls. Nil: a client with a 30 s limit.
	Client *http.Client
	// Own is each harness that keeps and renews its own sign-in in a home
	// of control's (own.go).
	Own map[string]Own

	renewing sync.Mutex   // one renewal at a time: a refresh token works once
	mu       sync.Mutex   // one write of a harness's file at a time
	homes    sync.RWMutex // a harness's own home: written by it under the write lock
}

// OAuth is how a harness's OAuth sign-in is renewed and sent.
type OAuth struct {
	// URL is the vendor's token endpoint; ClientID its client id for its
	// own CLI. A renewal posts the refresh token there.
	URL, ClientID string
	// AccountHeader names the header that carries the sign-in's account on
	// a model call, or "".
	AccountHeader string
}

// Signin is what a login gave: the token, when it stops working, and for an
// OAuth sign-in its refresh token and account.
type Signin struct {
	Token, Refresh, Account string
	Expires                 time.Time
	// Home is the login's folder, for a harness that keeps its own sign-in
	// there (Own): Keep makes it the harness's home on control.
	Home string
}

// kept is one harness's file.
type kept struct {
	Token       string    `json:"token"`
	Refresh     string    `json:"refresh,omitempty"`
	Account     string    `json:"account,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	RenewedAt   time.Time `json:"renewed_at,omitzero"`
	ExpiresAt   time.Time `json:"expires_at"`
	// Refused: the vendor refused to renew it, so only a new connect helps.
	Refused bool `json:"renew_refused,omitempty"`
}

// Status is what may be shown of one harness's subscription: never the token.
type Status struct {
	Harness     string     `json:"harness" doc:"The harness, such as claude."`
	Connected   bool       `json:"connected" doc:"A token is kept for it."`
	ConnectedAt *time.Time `json:"connected_at,omitempty" doc:"When it was connected."`
	ExpiresAt   *time.Time `json:"expires_at,omitempty" doc:"When the token stops working, as the vendor says. With renews, control gets a new one before then."`
	Renews      bool       `json:"renews" doc:"Control renews the token itself before it expires, with what the login gave it (codex's ChatGPT sign-in), or runs the harness to renew its own (agy's Google sign-in)."`
	Expired     bool       `json:"expired" doc:"The sign-in cannot be used: its time is up, or the vendor refused to renew it. Connect again."`
	Error       string     `json:"error,omitempty" doc:"Why it cannot be used, when it cannot."`
}

// harnessShape is a harness name as a file name.
var harnessShape = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// The vendor's own rule for when to renew (codex-cli 0.164.0,
// codex-rs/login/src/auth/manager.rs, read 2026-11-10): when the access token
// has less than five minutes left, or eight days after the last renewal.
const (
	renewWithin = 5 * time.Minute
	renewEvery  = 8 * 24 * time.Hour
)

func (s *Subscriptions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Subscriptions) file(harness string) (string, error) {
	if !harnessShape.MatchString(harness) {
		return "", errors.New("credentials: not a harness name")
	}
	return filepath.Join(s.Dir, harness+".json"), nil
}

// Put keeps harness's token, valid for valid from now.
func (s *Subscriptions) Put(harness, token string, valid time.Duration) error {
	return s.Keep(harness, Signin{Token: token, Expires: s.now().UTC().Add(valid)})
}

// Keep keeps the sign-in a login gave for harness. For a harness that keeps
// its own (Own), the login's folder becomes its home here, and only the
// connect's time is written beside it.
func (s *Subscriptions) Keep(harness string, si Signin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if o, ok := s.Own[harness]; ok {
		if err := s.keepHome(o, si.Home); err != nil {
			return err
		}
		return s.write(harness, kept{ConnectedAt: now})
	}
	return s.write(harness, kept{Token: si.Token, Refresh: si.Refresh, Account: si.Account, ConnectedAt: now, ExpiresAt: si.Expires.UTC()})
}

func (s *Subscriptions) write(harness string, k kept) error {
	p, err := s.file(harness)
	if err != nil {
		return err
	}
	if _, ok := s.Own[harness]; ok {
		// The sign-in stays in the harness's own file only.
		k.Token, k.ExpiresAt = "", time.Time{}
	}
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return errors.New("credentials: the subscriptions folder could not be made")
	}
	f, err := os.CreateTemp(s.Dir, ".tmp-*")
	if err != nil {
		return errors.New("credentials: the subscription could not be kept")
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(tmp, p) != nil {
		_ = os.Remove(tmp)
		return errors.New("credentials: the subscription could not be kept")
	}
	return nil
}

func (s *Subscriptions) read(harness string) (kept, error) {
	p, err := s.file(harness)
	if err != nil {
		return kept{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return kept{}, ErrNotConnected
	}
	if err != nil {
		return kept{}, errors.New("credentials: the subscription could not be read")
	}
	var k kept
	if json.Unmarshal(b, &k) != nil {
		return kept{}, errors.New("credentials: the subscription's file cannot be used; connect again")
	}
	if o, ok := s.Own[harness]; ok {
		if k.Token, k.ExpiresAt, err = s.ownSignin(o); err != nil {
			return kept{}, err
		}
		return k, nil
	}
	if k.Token == "" {
		return kept{}, errors.New("credentials: the subscription's file cannot be used; connect again")
	}
	return k, nil
}

// renews says k is renewed by control: it has a refresh token, and harness a
// way to renew it; or the harness renews its own sign-in, which control runs.
func (s *Subscriptions) renews(harness string, k kept) bool {
	if _, ok := s.Own[harness]; ok {
		return true
	}
	_, ok := s.OAuth[harness]
	return ok && k.Refresh != ""
}

// due says harness's k should be renewed now, by the vendor's rule.
func (s *Subscriptions) due(harness string, k kept) bool {
	if o, ok := s.Own[harness]; ok {
		return k.ExpiresAt.Sub(s.now()) < o.Within
	}
	last := k.RenewedAt
	if last.IsZero() {
		last = k.ConnectedAt
	}
	now := s.now()
	return k.ExpiresAt.Sub(now) < renewWithin || now.Sub(last) >= renewEvery
}

// Status is harness's status.
func (s *Subscriptions) Status(harness string) (Status, error) {
	st := Status{Harness: harness}
	k, err := s.read(harness)
	if errors.Is(err, ErrNotConnected) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Connected, st.ConnectedAt, st.ExpiresAt = true, &k.ConnectedAt, &k.ExpiresAt
	st.Renews = s.renews(harness, k)
	_, own := s.Own[harness]
	switch {
	case k.Refused:
		st.Expired, st.Error = true, "the vendor refused to renew the sign-in: connect again"
	case own && !s.now().Before(k.ExpiresAt):
		st.Expired, st.Error = true, "the sign-in's time is up and "+harness+" has not renewed it: connect again"
	case !st.Renews && !s.now().Before(k.ExpiresAt):
		st.Expired, st.Error = true, "the token's time is up: connect again"
	}
	return st, nil
}

// Remove forgets harness's token: the person disconnected it.
func (s *Subscriptions) Remove(harness string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.file(harness)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("credentials: the subscription could not be removed")
	}
	if o, ok := s.Own[harness]; ok {
		return s.removeOwnHome(o)
	}
	return nil
}

// Credential is n.Harness's token, while it works. An OAuth sign-in that is
// due is renewed first.
func (s *Subscriptions) Credential(n Need) (Cred, error) {
	k, err := s.read(n.Harness)
	switch {
	case errors.Is(err, ErrNotConnected):
		return Cred{}, &Missing{Why: NotConnected(n.Harness)}
	case err != nil:
		return Cred{}, &Missing{Why: "the " + n.Harness + " subscription kept on control cannot be read; connect it again in Settings"}
	}
	if s.renews(n.Harness, k) && (k.Refused || s.due(n.Harness, k)) {
		var rerr error
		k, _, rerr = s.renew(n.Harness)
		switch {
		case errors.Is(rerr, ErrNotConnected):
			return Cred{}, &Missing{Why: NotConnected(n.Harness)}
		case k.Refused:
			return Cred{}, &Missing{Why: "the vendor refused to renew the " + n.Harness + " sign-in kept on control; connect it again in Settings"}
		case rerr != nil && !s.now().Before(k.ExpiresAt):
			return Cred{}, &Missing{Why: "the " + n.Harness + " sign-in on control has expired and could not be renewed just now; try again soon, or connect it again in Settings"}
		}
		// A renewal that failed for now leaves a token with time left: it
		// is used, and the next call tries again.
	}
	if !s.now().Before(k.ExpiresAt) {
		return Cred{}, &Missing{Why: "the " + n.Harness + " subscription's token has expired; connect it again in Settings"}
	}
	c := Cred{Token: k.Token}
	if h := s.OAuth[n.Harness].AccountHeader; h != "" && k.Account != "" {
		c.Headers = map[string]string{h: k.Account}
	}
	return c, nil
}

// NotConnected is what a turn whose harness's subscription is not connected
// on control is told: it names the harness and where to connect it, never
// a vendor's error.
func NotConnected(harness string) string {
	return "connect your " + harness + " subscription in Settings"
}

// Ready says why n's subscription cannot be used now, or "": none is
// connected, the vendor refused to renew it, or its time is up and nothing
// renews it. One whose time is up but that control renews (codex's, agy's)
// is ready: the first call renews it (Credential). It renews nothing here.
func (s *Subscriptions) Ready(n Need) string {
	k, err := s.read(n.Harness)
	switch {
	case errors.Is(err, ErrNotConnected):
		return NotConnected(n.Harness)
	case err != nil:
		return "the " + n.Harness + " subscription kept on control cannot be read; connect it again in Settings"
	case k.Refused:
		return "the vendor refused to renew the " + n.Harness + " sign-in kept on control; connect it again in Settings"
	case !s.renews(n.Harness, k) && !s.now().Before(k.ExpiresAt):
		return "the " + n.Harness + " subscription's token has expired; connect it again in Settings"
	}
	return ""
}

// RenewDue renews every OAuth sign-in that is due, as control does on its
// own between turns, so a sign-in no turn used still works. It returns each
// renewal it tried, with its error.
func (s *Subscriptions) RenewDue() map[string]error {
	tried := map[string]error{}
	var names []string
	for h := range s.OAuth {
		names = append(names, h)
	}
	for h := range s.Own {
		names = append(names, h)
	}
	for _, h := range names {
		k, err := s.read(h)
		if err != nil || !s.renews(h, k) || k.Refused || !s.due(h, k) {
			continue
		}
		if _, did, err := s.renew(h); did {
			tried[h] = err
		}
	}
	return tried
}

// errRefused is a renewal the vendor refused.
var errRefused = errors.New("credentials: the vendor refused to renew the sign-in")

// renew renews harness's sign-in when it is still due once no other renewal
// runs: a refresh token works once, so two must never send the same one. It
// returns the sign-in as kept after, and whether it asked the vendor.
//
// What it learned is kept only over the sign-in it renewed (settle): a
// disconnect or a new connect made while it waited on the vendor stands.
func (s *Subscriptions) renew(harness string) (kept, bool, error) {
	s.renewing.Lock()
	defer s.renewing.Unlock()
	k, err := s.read(harness)
	if err != nil {
		return k, false, err
	}
	if k.Refused {
		return k, false, errRefused
	}
	if own, ok := s.Own[harness]; ok {
		if !s.due(harness, k) {
			return k, false, nil // another renewal came first
		}
		return s.renewOwn(harness, own, k)
	}
	o, ok := s.OAuth[harness]
	if !ok || k.Refresh == "" || !s.due(harness, k) {
		return k, false, nil // another renewal came first
	}
	si, err := refresh(s.client(), o, k.Refresh)
	switch {
	case errors.Is(err, errRefused):
		next := k
		next.Refused = true
		return s.settle(harness, k, next, err)
	case err != nil:
		return k, true, err
	}
	next := k
	next.Token, next.ExpiresAt, next.RenewedAt = si.Token, si.Expires, s.now().UTC()
	if si.Refresh != "" {
		next.Refresh = si.Refresh
	}
	return s.settle(harness, k, next, nil)
}

// settle keeps next, a renewal's result, in place of was, the sign-in it
// renewed, only while was is still the one kept. A sign-in connected or
// removed since, in this process or another, is left as it is, and returned
// as it is now.
func (s *Subscriptions) settle(harness string, was, next kept, why error) (kept, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.read(harness)
	if err != nil {
		return cur, true, err
	}
	if cur.Refresh != was.Refresh || !cur.ConnectedAt.Equal(was.ConnectedAt) {
		return cur, true, nil
	}
	if err := s.write(harness, next); err != nil {
		return cur, true, errors.New("credentials: the subscription could not be kept")
	}
	return next, true, why
}

func (s *Subscriptions) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// refresh asks o's token endpoint for a new access token. The request is the
// vendor's own (codex-cli 0.164.0, codex-rs/login/src/oauth/client.rs, read
// 2026-11-10): a JSON body of grant_type, client_id and refresh_token; the
// reply's access_token, and a new refresh_token when it rotates it. The
// vendor answering 400, 401 or 403 is a refusal; anything else may pass.
// Neither the reply's body nor Go's error text is passed on: either can quote
// a token.
func refresh(c *http.Client, o OAuth, refreshToken string) (Signin, error) {
	b, _ := json.Marshal(map[string]string{"grant_type": "refresh_token", "client_id": o.ClientID, "refresh_token": refreshToken})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL, bytes.NewReader(b))
	if err != nil {
		return Signin{}, errors.New("credentials: the renewal could not be asked for")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return Signin{}, errors.New("credentials: the vendor's token endpoint could not be reached")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Signin{}, errRefused
	case resp.StatusCode/100 != 2:
		return Signin{}, errors.New("credentials: the vendor's token endpoint did not renew the sign-in just now")
	}
	var r struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil || r.Access == "" {
		return Signin{}, errors.New("credentials: the vendor's renewal reply cannot be used")
	}
	exp, ok := expiry(r.Access)
	if !ok {
		return Signin{}, errors.New("credentials: the renewed token says no expiry")
	}
	return Signin{Token: r.Access, Refresh: r.Refresh, Expires: exp}, nil
}

// expiry is the exp claim of a JWT, such as an OAuth access token. Its
// signature is not checked: the token is only passed on, to the vendor that
// signed it.
func expiry(jwt string) (time.Time, bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(c.Exp, 0).UTC(), true
}
