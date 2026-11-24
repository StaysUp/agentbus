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
		if i.Replay != "" && i.Replay != FormatAnthropic && i.Replay != FormatOpenAI && i.Replay != FormatChatCompletions {
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
	for _, p := range Planned() {
		if _, ok := Get(p.Name); ok {
			t.Errorf("planned harness %q can start turns", p.Name)
		}
	}
	// opencode, once planned, is registered; nothing is planned now.
	if p := Planned(); len(p) != 0 {
		t.Errorf("Planned = %+v", p)
	}
	if got := PlannedList(); got != "" {
		t.Errorf("PlannedList = %q", got)
	}
	if got := NameList(); got != "claude, codex, agy, opencode or gemini" {
		t.Errorf("NameList = %q", got)
	}
}

// NameList is the sentence the API and the CLI build from the registry, so its
// join is pinned for the shapes a registry of one, two or three names gives.
func TestNameListJoin(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{}, ""},
		{[]string{"agree"}, "agree"},
		{[]string{"a", "b"}, "a or b"},
		{[]string{"a", "b", "c"}, "a, b or c"},
		{[]string{"a", "b", "c", "d"}, "a, b, c or d"},
	}
	for _, c := range cases {
		if got := joinNames(c.names); got != c.want {
			t.Errorf("joinNames(%q) = %q, want %q", c.names, got, c.want)
		}
	}
	// The registry's own order is what a user reads in that sentence.
	if got := Names(); len(got) == 0 || got[0] != "claude" {
		t.Errorf("Names() starts with %v, not the first registered harness", got)
	}
}

// Offers is true only for a kind the harness lists and does not disable.
func TestOffers(t *testing.T) {
	info := Info{Access: []Access{
		{Kind: Subscription},
		{Kind: OpenRouter, Disabled: true},
	}}
	cases := []struct {
		kind string
		want bool
	}{
		{"", true}, // "" means the subscription
		{Subscription, true},
		{OpenRouter, false}, // listed, but disabled
		{API, false},        // absent
	}
	for _, c := range cases {
		if got := Offers(info, c.kind); got != c.want {
			t.Errorf("Offers(%q) = %t, want %t", c.kind, got, c.want)
		}
	}
}

func TestOpenRouterModels(t *testing.T) {
	// The first id is the example the agent form and API docs name.
	if len(OpenRouterModels) == 0 || OpenRouterModels[0] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("OpenRouterModels = %v, want deepseek/deepseek-v4.1-flash first", OpenRouterModels)
	}
	seen := map[string]bool{}
	for _, id := range OpenRouterModels {
		vendor, model, ok := strings.Cut(id, "/")
		if !ok || vendor == "" || model == "" || strings.ContainsAny(id, " :") {
			t.Errorf("%q is not an OpenRouter id of the form vendor/model with no variant", id)
		}
		if seen[id] {
			t.Errorf("%q is listed twice", id)
		}
		seen[id] = true
	}
}
