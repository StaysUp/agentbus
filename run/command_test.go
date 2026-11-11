package run

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

func TestCheckCommand(t *testing.T) {
	for _, bad := range []string{"", "   ", "a\x00b", strings.Repeat("x", MaxCommandLine+1)} {
		if CheckCommand(bad) == "" {
			t.Errorf("%q passed", bad)
		}
	}
	if msg := CheckCommand("ls -la"); msg != "" {
		t.Fatal(msg)
	}
}

func TestCommandEnv(t *testing.T) {
	got := commandEnv([]string{"PATH=/bin", "AGENTBUS_TOKEN=x", "AGENTBUS_URL=y", "KEEP=1"})
	if strings.Join(got, " ") != "PATH=/bin KEEP=1" {
		t.Fatalf("%q", got)
	}
}

func TestCapped(t *testing.T) {
	c := &capped{max: 4}
	c.Write([]byte("ab"))
	c.Write([]byte("cdef"))
	if c.text() != "abcd" || c.n != 6 || !c.cut() {
		t.Fatalf("%q %d %t", c.text(), c.n, c.cut())
	}
	// A NUL byte is replaced: the text may go in a prompt argument.
	c = &capped{max: 8}
	c.Write([]byte("a\x00b"))
	if c.text() != "a\uFFFDb" {
		t.Fatalf("%q", c.text())
	}
	// A character the cut split is replaced, never left half.
	c = &capped{max: 2}
	c.Write([]byte("aé"))
	if c.text() != "a�" {
		t.Fatalf("%q", c.text())
	}
}

// The next turn's text: each command with its status and output, newest
// kept within the budget, each stream cut.
func TestShellContext(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &Engine{cfg: Config{Store: st, Now: time.Now}}
	if text, err := e.shellContext("s1", MaxPrompt); err != nil || text != "" {
		t.Fatalf("no commands: %q %v", text, err)
	}
	add := func(c event.Command) {
		if _, _, err := st.Append(event.Event{ID: newEventID(), Kind: event.CommandRan, Time: time.Now(), Session: "s1", Command: &c}); err != nil {
			t.Fatal(err)
		}
	}
	add(event.Command{Line: "ls", ExitCode: 0, Stdout: "a\nb\n"})
	add(event.Command{Line: "make", ExitCode: 2, Stderr: "no rule\n", TimedOut: true})
	text, err := e.shellContext("s1", MaxPrompt)
	if err != nil {
		t.Fatal(err)
	}
	want := shellHead + "\n\n" +
		"<shell-command>ls</shell-command>\n<shell-status>exit code 0</shell-status>\n<shell-stdout>\na\nb\n</shell-stdout>\n\n" +
		"<shell-command>make</shell-command>\n<shell-status>exit code 2, ended at its time limit</shell-status>\n<shell-stderr>\nno rule\n</shell-stderr>\n\n"
	if text != want {
		t.Fatalf("got\n%q\nwant\n%q", text, want)
	}
	// A long stream is cut; the budget keeps the newest commands.
	add(event.Command{Line: "big", Stdout: strings.Repeat("x", ShellStreamMax+10)})
	for range 5 {
		add(event.Command{Line: "huge", Stdout: strings.Repeat("y", ShellStreamMax), Stderr: strings.Repeat("z", ShellStreamMax)})
	}
	text, _ = e.shellContext("s1", MaxPrompt)
	if len(text) > ShellContextMax || strings.Contains(text, "<shell-command>ls<") || !strings.Contains(text, "earlier commands are left out") ||
		strings.Count(text, "<shell-command>huge<") == 0 {
		t.Fatalf("over budget: %d bytes, %d huge", len(text), strings.Count(text, "huge"))
	}
	// Little room in the prompt: nothing fits, so nothing is sent.
	if text, _ = e.shellContext("s1", 64); text != "" {
		t.Fatalf("no room: %q", text)
	}
}
