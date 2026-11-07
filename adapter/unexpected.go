package adapter

import (
	"bytes"
	"encoding/json"
	"regexp"

	"github.com/staysup/agentbus/event"
)

// MaxUnexpected bounds the schema.unexpected events of one exchange.
const MaxUnexpected = 16

// Unknowns collects the shapes an adapter did not recognise in one body. Each
// distinct shape is kept once, and at most MaxUnexpected are kept. The zero
// value is ready to use. It is not safe for concurrent use.
type Unknowns struct {
	seen map[event.Unexpected]struct{}
	list []event.Unexpected
}

// name is what a reported value or field may look like: the shape vendor
// type, event, role, status and usage field names have (lowercase, digits,
// "_" and "."). Anything else is replaced, so text or a credential that a
// vendor put where a name belongs never reaches an event: an upper-case word
// such as PRIVATE_PAYLOAD_123, or a key such as sk-ant-…, AKIA… or a JWT.
var name = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,47}$`)

// notAName replaces a value that is not shaped like a name.
const notAName = "(not a name)"

// clean keeps a name, and its own two markers, so cleaning twice (the adapter
// and then the gateway) gives the same result.
func clean(s string) string {
	if name.MatchString(s) || s == notAName || s == missing {
		return s
	}
	return notAName
}

// Value records an unknown value, such as a block type, at where.
func (u *Unknowns) Value(where, value string) {
	u.Admit(event.Unexpected{Where: where, Value: value})
}

// Field records an unknown field name in the object at where.
func (u *Unknowns) Field(where, field string) {
	u.Admit(event.Unexpected{Where: where, Field: field})
}

// Clean replaces a Value or Field that is not shaped like a name. They come
// from the vendor. Where is the adapter's own wording and is kept.
//
// A report with neither a value nor a field is something the vendor sent with
// no name at all (an untyped block or stream line); it gets Value "(missing)"
// so every report names a shape.
func Clean(x event.Unexpected) event.Unexpected {
	if x.Value != "" {
		x.Value = clean(x.Value)
	}
	if x.Field != "" {
		x.Field = clean(x.Field)
	}
	if x.Value == "" && x.Field == "" {
		x.Value = missing
	}
	return x
}

// missing names a shape the vendor sent without any name.
const missing = "(missing)"

// Admit keeps x, cleaned, if it is new and the bound is not reached, and says
// whether it did.
func (u *Unknowns) Admit(x event.Unexpected) bool {
	x = Clean(x)
	if _, ok := u.seen[x]; ok || len(u.list) >= MaxUnexpected {
		return false
	}
	if u.seen == nil {
		u.seen = map[event.Unexpected]struct{}{}
	}
	u.seen[x] = struct{}{}
	u.list = append(u.list, x)
	return true
}

// Events returns one schema.unexpected event per kept shape, in the order seen.
func (u *Unknowns) Events() []event.Event {
	evs := make([]event.Event, 0, len(u.list))
	for _, x := range u.list {
		x := x
		evs = append(evs, event.Event{Kind: event.SchemaUnexpected, Unexpected: &x})
	}
	return evs
}

// String reads raw as a JSON string. ok is false for anything else. A field
// read only to report it is kept raw and read with String, so reporting never
// makes parsing stricter.
func String(raw json.RawMessage) (s string, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	return s, json.Unmarshal(raw, &s) == nil
}

// Set is a list of known names.
type Set map[string]struct{}

// Names makes a Set.
func Names(names ...string) Set {
	s := make(Set, len(names))
	for _, n := range names {
		s[n] = struct{}{}
	}
	return s
}

// Has reports whether n is known.
func (s Set) Has(n string) bool {
	_, ok := s[n]
	return ok
}

// maxKeys bounds how many keys UnknownFields reads from one object, and
// maxTokens how many tokens it walks in all, so an object with a huge number
// of fields or a deep value costs a bounded amount of work.
const (
	maxKeys   = 256
	maxTokens = 4096
)

// UnknownFields appends to out each field name of the object b that known
// does not hold, in the order they appear, up to MaxUnexpected in all. It
// reads keys only and steps over values without keeping them, and stops after
// maxKeys keys or maxTokens tokens. Anything that is not an object adds
// nothing.
func UnknownFields(b []byte, where string, known Set, out []event.Unexpected) []event.Unexpected {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return out
	}
	tokens := 1
	for keys := 0; keys < maxKeys && dec.More() && len(out) < MaxUnexpected; keys++ {
		t, err := dec.Token()
		if err != nil {
			return out
		}
		k, ok := t.(string)
		if !ok {
			return out
		}
		if !known.Has(k) {
			out = append(out, event.Unexpected{Where: where, Field: k})
		}
		// Step over the value, counting nesting.
		depth := 0
		for {
			t, err := dec.Token()
			tokens++
			if err != nil || tokens > maxTokens {
				return out
			}
			switch t {
			case json.Delim('{'), json.Delim('['):
				depth++
			case json.Delim('}'), json.Delim(']'):
				depth--
			}
			if depth == 0 {
				break
			}
		}
	}
	return out
}
