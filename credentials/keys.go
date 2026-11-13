package credentials

// API keys the person sets in Settings, such as their OpenRouter key and
// their Anthropic and OpenAI API keys: pasted once, kept in the
// subscriptions' folder beside the subscriptions' tokens, in a file of its
// own readable by its owner only, and written the same way (write). An
// OpenRouter key set there wins over the variable an agent names (key_env),
// which still works for agents saved before keys could be set in Settings.
// A vendor's API key is only ever the one set there (APIKeys).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// The keys a vendor's API takes, by their names in Settings: each is a
// harness's Info.APIKey.Name.
const (
	KeyAnthropic = "anthropic"
	KeyOpenAI    = "openai"
	KeyGemini    = "gemini"
)

// KeyNames are every key that can be set in Settings. No other name has a
// file.
var KeyNames = []string{AccessOpenRouter, KeyAnthropic, KeyOpenAI, KeyGemini}

// MaxKeyLen bounds a key's length. OpenRouter's are about 73 characters,
// Anthropic's about 108, OpenAI's up to about 170, Gemini's 39.
const MaxKeyLen = 512

// ErrBadKey is a key that cannot be one: empty, too long, or holding a space
// or a control character. Its text never quotes the key.
var ErrBadKey = errors.New("the key must be one line of at most 512 characters with no spaces")

// KeyFile is the file of the key named name (KeyNames). Its name holds a
// dot, which no harness's name does (harnessShape), so it is never a
// subscription's file.
func (s *Subscriptions) KeyFile(name string) (string, error) {
	if !slices.Contains(KeyNames, name) {
		return "", errors.New("credentials: no key is kept by this name")
	}
	return filepath.Join(s.Dir, name+".key.json"), nil
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

// Key is the value of the key named name set in Settings, for control's own
// call with it (the vendor's list of models), or ErrNotConnected. It is
// never logged, shown or sent anywhere but that vendor.
func (s *Subscriptions) Key(name string) (string, error) {
	k, err := s.readKey(name)
	if err != nil {
		return "", err
	}
	return k.Key, nil
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

// VendorKey is the key a harness's access "api" takes: its name in Settings
// (KeyNames), its name for the person, and the header it goes in (Cred).
type VendorKey struct {
	Name, Label, Header string
}

// APIKeys gives an agent on access "api" its vendor's key, set in Settings.
// Of is each harness's key, by the harness's name. There is no variable to
// fall back on: the key is only ever the one set in Settings.
type APIKeys struct {
	Kept *Subscriptions
	Of   map[string]VendorKey
}

// Credential is the key of n.Harness's vendor set in Settings.
func (k APIKeys) Credential(n Need) (Cred, error) {
	v, ok := k.Of[n.Harness]
	if !ok || k.Kept == nil {
		return Cred{}, &Missing{Why: "control adds no API key to " + n.Harness + "'s calls"}
	}
	kk, err := k.Kept.readKey(v.Name)
	switch {
	case errors.Is(err, ErrNotConnected):
		return Cred{}, &Missing{Why: "set your " + v.Label + " in Settings"}
	case err != nil:
		return Cred{}, &Missing{Why: "the " + v.Label + " kept on control cannot be read; set it again in Settings"}
	}
	return Cred{Token: kk.Key, Header: v.Header}, nil
}

// Ready: as Credential, which reads only files.
func (k APIKeys) Ready(n Need) string {
	if _, err := k.Credential(n); err != nil {
		return err.Error()
	}
	return ""
}
