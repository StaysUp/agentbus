package credentials

// A login that keeps its sign-in in a file rather than printing it, such as
// codex login --device-auth, which writes auth.json in CODEX_HOME. Control
// runs it with that folder set to a new one of its own, inside the
// subscriptions folder (0700), reads the sign-in from the file when the
// login ends, and removes the folder. Never the person's own folder, and
// never the Keychain. Of what such a login prints, the person sees the links
// and the one-time code they enter at the link.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// home makes l's own folder in dir and gives the login's environment with
// l.Home set to it. A login that prints its token gets l.Env and no folder.
func (l Login) home(dir string) (env []string, home string, err error) {
	if l.Home == "" {
		return l.Env, "", nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", errors.New("credentials: the subscriptions folder could not be made")
	}
	if home, err = os.MkdirTemp(dir, ".login-*"); err != nil {
		return nil, "", errors.New("credentials: the login's folder could not be made")
	}
	base := l.Env
	if base == nil {
		base = os.Environ()
	}
	env = make([]string, 0, len(base)+1)
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); k != l.Home {
			env = append(env, kv)
		}
	}
	return append(env, l.Home+"="+home), home, nil
}

// signin is what the login gave: the token it printed (printed is its
// output), or the sign-in it kept in its folder home. ok is false when it
// gave none.
func (l Login) signin(printed []byte, home string, now time.Time) (Signin, bool) {
	if l.Home == "" {
		tok := string(l.Token.Find(printed))
		return Signin{Token: tok, Expires: now.UTC().Add(l.Valid)}, tok != ""
	}
	b, err := os.ReadFile(filepath.Join(home, l.File))
	if err != nil {
		return Signin{}, false
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return Signin{}, false
	}
	si := Signin{Token: field(v, l.TokenKey), Refresh: field(v, l.RefreshKey), Account: field(v, l.AccountKey)}
	exp, ok := expiry(si.Token)
	si.Expires = exp
	return si, ok && si.Token != ""
}

// field is the string at a dotted path of a JSON value, or "".
func field(v any, path string) string {
	if path == "" {
		return ""
	}
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return s
}

// ended says why the login gave no sign-in, in fixed wording.
func (l Login) ended() string {
	if l.Home != "" {
		return "the login ended without a sign-in"
	}
	return "the login ended without printing a token"
}
