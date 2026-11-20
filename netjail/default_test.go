package netjail

import "testing"

// The wall's default allow-list lets each of its own hosts through and
// refuses a host that is not on it, without resolving either.
func TestDefaultAllow(t *testing.T) {
	for _, h := range DefaultAllow {
		if !Allowed(DefaultAllow, h) {
			t.Errorf("DefaultAllow does not allow its own host %q", h)
		}
	}
	// Near misses and common egress that must stay refused until a person
	// allows them (the Ask card of a later step).
	for _, h := range []string{
		"api.openai.com", "example.com", "raw.githubusercontent.com",
		"codeload.github.com", "npmjs.org", "evil.github.com.attacker.net",
	} {
		if Allowed(DefaultAllow, h) {
			t.Errorf("DefaultAllow allows %q, which is not on the list", h)
		}
	}
	// Through the proxy's own decision: an allowed host passes, a host not
	// on the list is refused. No DNS: Allow answers before any lookup.
	j := New(Spec{Allow: DefaultAllow})
	for _, h := range []string{"github.com:443", "registry.npmjs.org:443", "crates.io:443", "proxy.golang.org:443"} {
		if why := j.check(h); why != "" {
			t.Errorf("proxy refused allowed %q: %s", h, why)
		}
	}
	if j.check("api.openai.com:443") == "" {
		t.Error("proxy let through a host not on the list")
	}
}
