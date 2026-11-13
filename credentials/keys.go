package credentials

// API keys the person sets in Settings, such as their OpenRouter key: pasted
// once, kept in the subscriptions' folder beside the subscriptions' tokens,
// in a file of its own readable by its owner only, and written the same way
// (write). A key set there wins over the variable an agent names (key_env),
// which still works for agents saved before keys could be set in Settings.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// KeyStatus is what may be shown of a key set in Settings: never the key.
type KeyStatus struct {
	Set   bool       `json:"set" doc:"A key is set in Settings and kept on control. Its value is never sent."`
	SetAt *time.Time `json:"set_at,omitempty" doc:"When it was set."`
}

// keptKey is one key's file.
type keptKey struct {
	Key   string    `json:"key"`
	SetAt time.Time `json:"set_at"`
}

// MaxKeyLen bounds a key's length. OpenRouter's are about 73 characters.
const MaxKeyLen = 512

// ErrBadKey is a key that cannot be one: empty, too long, or holding a space
// or a control character. Its text never quotes the key.
var ErrBadKey = errors.New("the key must be one line of at most 512 characters with no spaces")

// KeyFile is access's key file. Its name holds a dot, which no harness's
// name does (harnessShape), so it is never a subscription's file.
func (s *Subscriptions) KeyFile(access string) (string, error) {
	if access != AccessOpenRouter {
		return "", errors.New("credentials: no key is kept for this access")
	}
	return filepath.Join(s.Dir, access+".key.json"), nil
}

// CleanKey is key as pasted, with the spaces and line ends around it
// removed, or ErrBadKey.
func CleanKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > MaxKeyLen {
		return "", ErrBadKey
	}
	for _, c := range key {
		if c <= ' ' || c == 0x7f {
			return "", ErrBadKey
		}
	}
	return key, nil
}

// SetKey keeps key for access, in place of any kept before.
func (s *Subscriptions) SetKey(access, key string) error {
	key, err := CleanKey(key)
	if err != nil {
		return err
	}
	p, err := s.KeyFile(access)
	if err != nil {
		return err
	}
	b, err := json.Marshal(keptKey{Key: key, SetAt: s.now().UTC()})
	if err != nil {
		return errors.New("credentials: the key could not be kept")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeFile(p, b) != nil {
		return errors.New("credentials: the key could not be kept")
	}
	return nil
}

// RemoveKey forgets access's key. None kept is not an error.
func (s *Subscriptions) RemoveKey(access string) error {
	p, err := s.KeyFile(access)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("credentials: the key could not be removed")
	}
	return nil
}

// readKey is access's kept key, or ErrNotConnected when none is kept.
func (s *Subscriptions) readKey(access string) (keptKey, error) {
	p, err := s.KeyFile(access)
	if err != nil {
		return keptKey{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return keptKey{}, ErrNotConnected
	}
	if err != nil {
		return keptKey{}, errors.New("credentials: the key could not be read")
	}
	var k keptKey
	if json.Unmarshal(b, &k) != nil || k.Key == "" {
		return keptKey{}, errors.New("credentials: the key's file cannot be used; set the key again")
	}
	return k, nil
}

// KeyStatus says whether access's key is set. A file that cannot be read
// is an error, never shown as set.
func (s *Subscriptions) KeyStatus(access string) (KeyStatus, error) {
	k, err := s.readKey(access)
	if errors.Is(err, ErrNotConnected) {
		return KeyStatus{}, nil
	}
	if err != nil {
		return KeyStatus{}, err
	}
	return KeyStatus{Set: true, SetAt: &k.SetAt}, nil
}

// KeyFirst is the source of an API key: the one set in Settings (Kept),
// or, when none is, the variable the agent names, read by Env.
type KeyFirst struct {
	Kept *Subscriptions // nil: none is kept, Env only
	Env  Env
}

// Credential is the key set in Settings, or else n.KeyEnv's.
func (k KeyFirst) Credential(n Need) (Cred, error) {
	if k.Kept != nil {
		kk, err := k.Kept.readKey(n.Access)
		switch {
		case err == nil:
			return Cred{Token: kk.Key}, nil
		case !errors.Is(err, ErrNotConnected):
			return Cred{}, &Missing{Why: "the " + n.Access + " key kept on control cannot be read; set it again in Settings"}
		}
	}
	c, err := k.Env.Credential(n)
	if err == nil || k.Kept == nil {
		return c, err
	}
	if EnvNameOK(n.KeyEnv) {
		return Cred{}, &Missing{Why: "set your OpenRouter key in Settings, or set " + n.KeyEnv + " where control runs and restart agentbus"}
	}
	return Cred{}, &Missing{Why: "set your OpenRouter key in Settings"}
}

// Ready: as Credential, which reads only files and the environment.
func (k KeyFirst) Ready(n Need) string {
	if _, err := k.Credential(n); err != nil {
		return err.Error()
	}
	return ""
}

// KeySet says whether a key would be given for n now: set in Settings, or
// in n.KeyEnv. Its value is never returned.
func (k KeyFirst) KeySet(n Need) bool {
	_, err := k.Credential(n)
	return err == nil
}
