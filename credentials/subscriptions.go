package credentials

// The person's own subscriptions, for runners they own: each harness's
// sign-in token, connected once with the vendor's own login (Manager) and
// kept in control's data folder, one file per harness, readable by its owner
// only. Never the Mac Keychain, never a runner. Only used when the policy
// says subscriptions_on_runners = "own".

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Subscriptions is the folder that holds them.
type Subscriptions struct {
	Dir string
	Now func() time.Time // nil: time.Now
}

// kept is one harness's file.
type kept struct {
	Token       string    `json:"token"`
	ConnectedAt time.Time `json:"connected_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Status is what may be shown of one harness's subscription: never the token.
type Status struct {
	Harness     string     `json:"harness" doc:"The harness, such as claude."`
	Connected   bool       `json:"connected" doc:"A token is kept for it."`
	ConnectedAt *time.Time `json:"connected_at,omitempty" doc:"When it was connected."`
	ExpiresAt   *time.Time `json:"expires_at,omitempty" doc:"When the token stops working, as the vendor's login says; connect again before then."`
	Expired     bool       `json:"expired" doc:"The token's time is up: connect again."`
}

// harnessShape is a harness name as a file name.
var harnessShape = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

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
	p, err := s.file(harness)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	b, err := json.Marshal(kept{Token: token, ConnectedAt: now, ExpiresAt: now.Add(valid)})
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
	if json.Unmarshal(b, &k) != nil || k.Token == "" {
		return kept{}, errors.New("credentials: the subscription's file cannot be used; connect again")
	}
	return k, nil
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
	st.Expired = !s.now().Before(k.ExpiresAt)
	return st, nil
}

// Remove forgets harness's token: the person disconnected it.
func (s *Subscriptions) Remove(harness string) error {
	p, err := s.file(harness)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("credentials: the subscription could not be removed")
	}
	return nil
}

// Credential is n.Harness's token, while its time is not up.
func (s *Subscriptions) Credential(n Need) (string, error) {
	k, err := s.read(n.Harness)
	switch {
	case errors.Is(err, ErrNotConnected):
		return "", &Missing{Why: "no " + n.Harness + " subscription is connected on control; connect it once (agentbus connect " + n.Harness + ", or Connect in the cockpit)"}
	case err != nil:
		return "", &Missing{Why: "the " + n.Harness + " subscription kept on control cannot be read; connect it again"}
	case !s.now().Before(k.ExpiresAt):
		return "", &Missing{Why: "the " + n.Harness + " subscription's token has expired; connect it again"}
	}
	return k.Token, nil
}
