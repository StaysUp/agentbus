package api

// The harnesses and the access kinds the contract names are the code's own: a
// harness added to the registry, an access kind added to harness.Accesses, or a
// harness no agent can run yet appears in openapi.json without an edit here, and
// a list typed by hand that misses one fails.

import (
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/staysup/agentbus/agents"
	"github.com/staysup/agentbus/harness"
)

// words is what a description says, split at everything that is not part of a
// name, so "claude-cli" holds "claude" but "claudette" does not.
func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
}

// TestOpenAPIDescriptionsNameEveryHarnessAndAccess: every description that says
// which harnesses an agent can be saved on names all of harness.Names(), the one
// that says which are listed but not runnable names all of harness.Planned(),
// every one that says which access kinds exist names all of harness.Accesses(),
// and no token is left unfilled. The enum on the request's harness is that list
// of harnesses and no other.
func TestOpenAPIDescriptionsNameEveryHarnessAndAccess(t *testing.T) {
	doc := loadSpec(t)
	for _, c := range []struct {
		schema, field string
		names         []string
	}{
		{"AgentRequest", "harness", harness.Names()},
		{"SavedAgent", "harness", harness.Names()},
		{"Harness", "name", append(harness.Names(), plannedNames()...)},
		{"AgentRequest", "access", harness.Accesses()},
		{"SavedAgent", "access", harness.Accesses()},
		{"ModelList", "access", harness.Accesses()},
		{"AccessOption", "kind", harness.Accesses()},
	} {
		got := doc.Components.Schemas[c.schema].Value.Properties[c.field].Value.Description
		for _, tok := range []string{harnessesToken, accessesToken, plannedToken} {
			if strings.Contains(got, tok) {
				t.Errorf("%s.%s: %s is left unfilled in the description", c.schema, c.field, tok)
			}
		}
		have := words(got)
		for _, name := range c.names {
			if !slices.Contains(have, name) {
				t.Errorf("%s.%s: the description does not name %s: %q", c.schema, c.field, name, got)
			}
		}
	}
	got := doc.Components.Schemas["AgentRequest"].Value.Properties["harness"].Value.Enum
	for _, name := range harness.Names() {
		if !slices.Contains(got, any(name)) {
			t.Errorf("the enum on harness has no %s, which an agent can be saved on", name)
		}
	}
	if len(got) != len(harness.Names()) {
		t.Errorf("the enum on harness has %d values, harness.Names() has %d", len(got), len(harness.Names()))
	}
}

// plannedNames is the name of each harness an agent cannot run yet.
func plannedNames() []string {
	var out []string
	for _, p := range harness.Planned() {
		out = append(out, p.Name)
	}
	return out
}

// TestOpenAPIAccessEnumMatchesCheck: the enum on access holds each kind the code
// has and no other, and agents.Check accepts each of them on a harness that
// offers it.
func TestOpenAPIAccessEnumMatchesCheck(t *testing.T) {
	doc := loadSpec(t)
	got := doc.Components.Schemas["AgentRequest"].Value.Properties["access"].Value.Enum
	for _, v := range got {
		kind, ok := v.(string)
		if !ok {
			t.Errorf("the enum on access holds %v, which is not a string", v)
			continue
		}
		if !slices.Contains(harness.Accesses(), kind) {
			t.Errorf("the enum on access has %q, which is not one of the code's kinds: %s", kind, harness.AccessList())
		}
	}
	if len(got) != len(harness.Accesses()) {
		t.Errorf("the enum on access has %d values, the code has %d", len(got), len(harness.Accesses()))
	}
	for _, kind := range harness.Accesses() {
		if !slices.Contains(got, any(kind)) {
			t.Errorf("the enum on access has no %s, which the code has", kind)
		}
		offered := false
		for _, h := range harness.All() {
			if !harness.Offers(h.Info(), kind) {
				continue
			}
			offered = true
			f := agents.Fields{Name: "a", Harness: h.Info().Name, Access: kind, Model: "deepseek/deepseek-v4.1-flash"}
			if why := agents.Check(&f); why != "" {
				t.Errorf("agents.Check refuses %q on %s, which offers it: %s", kind, h.Info().Name, why)
			}
			break
		}
		if !offered {
			t.Errorf("no harness offers %s, which the enum on access lists", kind)
		}
	}
}
