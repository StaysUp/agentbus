package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/openai"
	"github.com/staysup/agentbus/event"
)

// decider builds a guardBody for one reply with the given rules and allow-once
// list, so decide can be driven directly.
func decider(t *testing.T, a adapter.Adapter, rules Rules, allowOnce []string) *guardBody {
	t.Helper()
	g := New(&url.URL{}, a, &event.Memory{})
	return g.guardReply(context.Background(), &exchange{id: "x1", session: "s1", turn: "t_1"}, rules, allowOnce, nil,
		io.NopCloser(strings.NewReader("")), func() {}).(*guardBody)
}

func bashItem(cmd string) map[string]any {
	return map[string]any{"name": "Bash", "input": map[string]any{"command": cmd}}
}

func execItem(cmd string) map[string]any {
	return map[string]any{"type": "custom_tool_call", "name": "exec",
		"input": `await tools.exec_command({cmd:"` + cmd + `"});`}
}

// A command on none of the lists is held for a person: the one gate's ask
// default. For claude (Bash) and codex (exec) alike — codex parity.
func TestUnlistedRaisesCard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		a      adapter.Adapter
		item   map[string]any
		cmdKey string
	}{
		{"claude", anthropic.Adapter{}, bashItem("make deploy"), "0"},
		{"codex", openai.Adapter{}, execItem("make deploy"), "c1"},
	} {
		b := decider(t, tc.a, Rules{Allow: []string{"ls"}, Unlisted: Ask}, nil)
		b.decide(tc.cmdKey, tc.cmdKey, "exec", json.RawMessage(`"x"`), tc.item)
		if len(b.asked) != 1 || b.asked[0].command != "make deploy" {
			t.Fatalf("%s: unlisted command was not held: %+v", tc.name, b.asked)
		}
		if _, ok := b.refused[tc.cmdKey]; !ok {
			t.Errorf("%s: the call was not refused to the agent", tc.name)
		}
	}
}

// An allowed command runs: it is not held.
func TestAllowedRuns(t *testing.T) {
	b := decider(t, anthropic.Adapter{}, Rules{Allow: []string{"ls"}, Unlisted: Ask}, nil)
	b.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem("ls -la"))
	if len(b.asked) != 0 || len(b.refused) != 0 {
		t.Fatalf("an allowed command was held: asked=%+v refused=%v", b.asked, b.refused)
	}
}

// Allow once lets exactly the approved command line through for this turn, and
// nothing else: a different command still asks. It is per-turn, so a turn
// without it (the next one) asks again — nothing is written to any list.
func TestAllowOnce(t *testing.T) {
	rules := Rules{Allow: []string{"ls"}, Unlisted: Ask}
	// This turn: the exact command line is allowed once.
	b := decider(t, anthropic.Adapter{}, rules, []string{"make deploy"})
	b.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem("make deploy"))
	if len(b.asked) != 0 || len(b.refused) != 0 {
		t.Fatalf("the allowed-once command was held: asked=%+v", b.asked)
	}
	// A different command is not covered.
	b2 := decider(t, anthropic.Adapter{}, rules, []string{"make deploy"})
	b2.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem("make deploy --prod"))
	if len(b2.asked) != 1 {
		t.Fatalf("a command not allowed once was not held: %+v", b2.asked)
	}
	// The next turn has no allow-once (none persisted): the same command asks.
	b3 := decider(t, anthropic.Adapter{}, rules, nil)
	b3.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem("make deploy"))
	if len(b3.asked) != 1 {
		t.Fatalf("allow once leaked to a turn without it: %+v", b3.asked)
	}
	// Allow once never overrides a forbid: the hard boundary wins.
	fb := decider(t, anthropic.Adapter{}, Rules{Forbid: []string{"rm -rf"}, Unlisted: Ask}, []string{"rm -rf /"})
	fb.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem("rm -rf /"))
	if _, ok := fb.refused["0"]; !ok || len(fb.asked) != 0 {
		t.Fatalf("allow once overrode a forbid: refused=%v asked=%+v", fb.refused, fb.asked)
	}
}

// An unlisted command's approval suggests a program+subcommand rule, so an
// "always allow" has a clean entry to add to the allow list.
func TestUnlistedSuggestsRule(t *testing.T) {
	b := decider(t, anthropic.Adapter{}, Rules{Unlisted: Ask}, nil)
	b.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem("npm run build"))
	if len(b.asked) != 1 || b.asked[0].rule != "npm run" {
		t.Fatalf("suggested rule = %+v; want npm run", b.asked)
	}
}

// rulesHeld is the distinct non-empty rules collected for a held call.
func rulesHeld(b *guardBody) []string {
	var out []string
	for _, h := range b.asked {
		if h.rule != "" && !contains(out, h.rule) {
			out = append(out, h.rule)
		}
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// The suggested rule is the HELD part of a chain, never an earlier allowed part
// or a wrapper: "always allow" must add the command that was held, or it loops.
func TestSuggestsHeldPartNotWrapperOrAllowed(t *testing.T) {
	for _, tc := range []struct {
		line string
		rule string
	}{
		{"git status && make test", "make test"}, // not "git status"
		{"env FOO=1 make deploy", "make deploy"}, // not "env"
		{"sudo make deploy", "make deploy"},      // not "sudo make"
	} {
		b := decider(t, anthropic.Adapter{}, Rules{Allow: []string{"git status"}, Unlisted: Ask}, nil)
		b.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem(tc.line))
		if got := rulesHeld(b); len(got) != 1 || got[0] != tc.rule {
			t.Errorf("%q suggested %v; want [%q]", tc.line, got, tc.rule)
		}
	}
}

// A held command with no clean rule (built at run time) is shown but records no
// rule, so "always allow" adds nothing instead of a 500 on an empty command.
func TestHeldRunTimeCommandHasNoRule(t *testing.T) {
	b := decider(t, anthropic.Adapter{}, Rules{Unlisted: Ask}, nil)
	b.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem(`$CMD --flag`))
	if len(b.asked) != 1 {
		t.Fatalf("the run-time command was not held: %+v", b.asked)
	}
	if r := rulesHeld(b); len(r) != 0 {
		t.Errorf("a run-time command suggested a rule: %v", r)
	}
}

// A forbidden command behind wrappers is still refused, and a deep stack of
// wrappers is never silently allowed (it is held, not let through).
func TestForbidBehindWrappersAndDeepStack(t *testing.T) {
	r := Rules{Forbid: []string{"rm -rf"}, Unlisted: Ask}
	if dec, _ := classify(r, "env A=1 sudo env B=2 rm -rf /"); dec != Refuse {
		t.Errorf("forbid behind wrappers was not refused")
	}
	// 300 env prefixes before a forbidden command: past the strip bound it is
	// held, never Allow.
	deep := strings.Repeat("env ", 300) + "rm -rf /"
	if dec, _ := classify(r, deep); dec == Allow {
		t.Errorf("a deeply wrapped command was silently allowed")
	}
}

// A line the shell parser cannot read still holds, but suggests no rule: the
// fallback cuts it into runs of words to decide it, and those runs are not
// commands a person chose. Seen live (codex-cli 0.171.0, 2026-11-23): the model
// left a stray quote at the end of this line, and one "always allow" wrote 21
// entries such as "import", "if isinstance" and "in" to the allow list.
func TestUnparsedLineSuggestsNoRule(t *testing.T) {
	line := `curl -sS 'https://api.github.com/repos/StaysUp/agentbus/issues?state=open&per_page=50' | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d if isinstance(d,dict) else [(i["number"],i["title"],i["html_url"],len(i["assignees"])) for i in d if "pull_request" not in i])'; head -100 ROADMAP.md'`
	if _, err := parse(line); err == nil {
		t.Fatal("the line parsed; the test needs one the parser rejects")
	}
	rules := Rules{Allow: []string{"ls"}, Unlisted: Ask}
	if dec, _ := classify(rules, line); dec != Ask {
		t.Fatalf("the unparsed line decided %v; want it held (Ask)", dec)
	}
	b := decider(t, anthropic.Adapter{}, rules, nil)
	b.decide("0", "t1", "Bash", json.RawMessage(`{}`), bashItem(line))
	if r := rulesHeld(b); len(r) != 0 {
		t.Fatalf("an unparsed line suggested %d rules: %q", len(r), r)
	}
	if len(b.asked) != 1 || b.asked[0].command != line {
		t.Fatalf("the unparsed line was not held once: %d held", len(b.asked))
	}
	if _, ok := b.refused["0"]; !ok {
		t.Error("the call was not refused to the agent")
	}
}
