package harness

import (
	"strings"
	"testing"
)

// Every registered harness says everything the rest of the code asks, under
// a name no other harness, and no planned one, has.
func TestRegistry(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Planned() {
		seen[p.Name] = true
	}
	for _, h := range All() {
		i := h.Info()
		// StatusArgs may be empty: agy has no offline status command.
		if i.Name == "" || i.Label == "" || i.Product == "" || i.OwnEndpointEnv == "" {
			t.Errorf("%q: incomplete info %+v", i.Name, i)
		}
		if seen[i.Name] {
			t.Errorf("%q registered twice", i.Name)
		}
		seen[i.Name] = true
		if len(i.Access) == 0 || i.Access[0].Kind != Subscription {
			t.Errorf("%q: access %+v does not start with its subscription", i.Name, i.Access)
		}
		if i.Replay != "" && i.Replay != FormatAnthropic && i.Replay != FormatOpenAI {
			t.Errorf("%q: replay format %q is not one own replay knows", i.Name, i.Replay)
		}
		if g, ok := Get(i.Name); !ok || g.Info().Name != i.Name {
			t.Errorf("Get(%q) = %v %t", i.Name, g, ok)
		}
		// The prompt is always last, after "--" or joined to its flag by
		// "=", so it is never read as a flag.
		a := h.Args(Turn{Prompt: "-p"})
		last := a[len(a)-1]
		if !(len(a) >= 2 && last == "-p" && a[len(a)-2] == "--") && !(strings.HasPrefix(last, "--") && strings.HasSuffix(last, "=-p")) {
			t.Errorf("%q: args %q do not end in -- and the prompt", i.Name, a)
		}
	}
	if _, ok := Get("opencode"); ok {
		t.Error("a planned harness can start turns")
	}
	if got := NameList(); got != "claude, codex or agy" {
		t.Errorf("NameList = %q", got)
	}
}
