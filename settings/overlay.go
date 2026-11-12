package settings

// A policy in the settings database is an overlay: only the keys the person
// changed from the defaults, by dotted name (limits.max_depth). The policy
// a turn runs under is the defaults of the database's version with the
// overlay on top.

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// flat is a policy as dotted keys, each with its JSON value.
type flat map[string]json.RawMessage

// flatten is p, filled, as dotted keys.
func flatten(p Policy) flat {
	p.fill()
	b, err := json.Marshal(p)
	if err != nil {
		panic(err) // a Policy always encodes
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		panic(err)
	}
	out := flat{}
	for k, v := range top {
		var sub map[string]json.RawMessage
		if bytes.HasPrefix(v, []byte("{")) && json.Unmarshal(v, &sub) == nil {
			for sk, sv := range sub {
				out[k+"."+sk] = sv
			}
			continue
		}
		out[k] = v
	}
	return out
}

// unflatten is f as a policy. A key the policy does not have is left out, so
// a key a later build retired does not stop an older overlay.
func (f flat) policy() (Policy, error) {
	top := map[string]any{}
	for k, v := range f {
		if i := strings.IndexByte(k, '.'); i >= 0 {
			sub, _ := top[k[:i]].(map[string]json.RawMessage)
			if sub == nil {
				sub = map[string]json.RawMessage{}
				top[k[:i]] = sub
			}
			sub[k[i+1:]] = v
			continue
		}
		top[k] = v
	}
	b, err := json.Marshal(top)
	if err != nil {
		return Policy{}, err
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return Policy{}, errors.New("a stored value has the wrong type")
	}
	if err := p.Check(); err != nil {
		return Policy{}, err
	}
	p.fill()
	return p, nil
}

// overlayOf is what p changes from base: the keys whose value differs.
func overlayOf(base, p Policy) flat {
	b, q := flatten(base), flatten(p)
	out := flat{}
	for k, v := range q {
		if !bytes.Equal(b[k], v) {
			out[k] = v
		}
	}
	return out
}

// applyOverlay is base with o on top. Keys base does not have are ignored.
func applyOverlay(base Policy, o flat) (Policy, error) {
	f := flatten(base)
	for k, v := range o {
		if _, ok := f[k]; ok {
			f[k] = v
		}
	}
	return f.policy()
}

// changedKeys are the keys whose value differs between a and b, sorted.
func changedKeys(a, b Policy) []string {
	fa, fb := flatten(a), flatten(b)
	var out []string
	for k, v := range fb {
		if !bytes.Equal(fa[k], v) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// policyVersion names the policy p, for an ETag.
func policyVersion(p Policy) Version {
	p.fill()
	b, _ := json.Marshal(p)
	return versionOf(b)
}
