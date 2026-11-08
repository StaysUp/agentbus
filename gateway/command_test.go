package gateway

import (
	"slices"
	"testing"
)

func TestForbiddenMatchesEachSimpleCommand(t *testing.T) {
	rules := []string{"git push", "rm -rf"}
	for _, c := range []struct {
		line string
		want string // the rule, "" for no match
	}{
		// The research misses (2026-11-08). x1: codex's own style.
		{"git -C repo push origin main && git --git-dir=remote.git rev-parse refs/heads/main && git -C repo rev-parse HEAD", "git push"},
		// c1: Claude chains with &&, and its message has a newline in quotes.
		{"git add notes.txt && git commit -m \"add notes\n\nCo-Authored-By: someone\" && git push origin main", "git push"},
		{"git push", "git push"},
		{"git push origin main", "git push"},
		{"cd repo; git push", "git push"},
		{"git status || git push", "git push"},
		{"echo hi | git push", "git push"},
		{"git status\ngit push", "git push"},
		{"git status & git push", "git push"},
		{"(cd repo && git push)", "git push"},
		{"echo $(git push)", "git push"},
		{"echo `git push`", "git push"},
		{"/usr/bin/git push", "git push"},
		{"git -c user.name=x push", "git push"},
		{"git --git-dir=.git --work-tree=. push", "git push"},
		{"git --git-dir .git push", "git push"},
		{"git --no-pager -C a -C b push", "git push"},
		{"GIT_TRACE=1 git push", "git push"},
		{"sudo -E git push", "git push"},
		{"env FOO=bar git push", "git push"},
		{"bash -lc 'git push origin main'", "git push"},
		{"sh -c \"cd repo && git push\"", "git push"},
		{"if true; then git push; fi", "git push"},
		{"{ git push; }", "git push"},
		{"git -C ${DIR} push", "git push"},
		{"git \\\n  push", "git push"},
		{"rm -rf build", "rm -rf"},
		// From review, 2026-11-08.
		{`echo "$(git push)"`, "git push"},
		{"echo \"`git push`\"", "git push"},
		{`x="$(cd repo && git push origin main)"`, "git push"},
		{"sudo -u root git push", "git push"},
		{"env -C repo git push", "git push"},
		{"xargs -I {} git push", "git push"},
		{"bash <<'EOF'\ngit push\nEOF", "git push"},
		{"git -C repo push --dry-run # done", "git push"},
		// From the second review, 2026-11-08.
		{"cat <<EOF\n$(git push)\nEOF", "git push"},
		{"cat <<EOF\n`git push`\nEOF", "git push"},
		{`echo "$(printf '%s' ')' && git push)"`, "git push"},
		{"env -S 'git push origin main'", "git push"},
		{"env --split-string='git push'", "git push"},
		// From the third review, 2026-11-08.
		{">/tmp/out git push", "git push"},
		{"git >/tmp/out push", "git push"},
		{"git 2>&1 push", "git push"},
		{"git push origin main 2>&1 | tail -3", "git push"},
		{"git > out push", "git push"},
		{"&>/dev/null git push", "git push"},
		// From the fourth review, 2026-11-08.
		{"git push>/tmp/log", "git push"},
		{"bash <<< 'git push'", "git push"},
		{"bash --rcfile /dev/null -c 'git push'", "git push"},
		{"bash -e -o pipefail -c 'git push'", "git push"},
		{"git pu\\sh", "git push"},
		{"git $'\\x70ush'", "git push"},
		{"eval 'git push'", "git push"},
		{"cat <(git push)", "git push"},
		{"f() { git push; }", "git push"},
		{"time git push", "git push"},
		{"! git push", "git push"},
		{"for r in a b; do git -C $r push; done", "git push"},
		{"bash <<EOF\ngit push\nEOF", "git push"},
		// A line the parser cannot read errs toward refusing.
		{"git push 'unclosed", "git push"},
		{"echo $( ; git push", "git push"},
		{"env -S'git push'", "git push"},
		{`env -S '"git" push'`, "git push"},

		// Not a push.
		{"git log --grep push", ""},
		{"git commit -m 'git push later; ok'", ""},
		{"echo \"git push\"", ""},
		{"git status && git log --oneline", ""},
		{"git pushx", ""},
		{"gitpush", ""},
		{"git -C push status", ""},
		{"rm -r build", ""},
		{"cat <<'EOF'\ngit push\nEOF", ""},
		{"cat <<'EOF'\n$(git push)\nEOF", ""},
		{"cat <<-EOF > notes.md\n\tgit push\n\tEOF\necho ok", ""},
		{"echo ok # example: ; git push", ""},
		{"echo ok\n# git push", ""},
		{"echo a#b; git status", ""},
		{"sh script.sh -c 'git push'", ""},
		{"bash <<< 'echo git push'", ""},
		{"git $(echo push)", ""}, // built at run time: the documented limit
		{"echo 'unclosed git status", ""},
		{"git status 2>&1", ""},
		{"", ""},
	} {
		got, ok := forbidden(rules, c.line)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("forbidden(%q) = %q, %v; want %q", c.line, got, ok, c.want)
		}
	}
}

// A rule may name a wrapper, and a git rule may carry git's own options.
func TestForbiddenRuleShapes(t *testing.T) {
	for _, c := range []struct{ rule, line string }{
		{"sudo", "sudo echo hello"},
		{"env", "env FOO=1 ls"},
		{"git -C repo push", "git -C repo push origin main"},
		{"git -C repo push", "git push"},
		{"/usr/bin/git push", "git push"},
		{"git push", "/usr/bin/git push origin main"},
	} {
		if _, ok := forbidden([]string{c.rule}, c.line); !ok {
			t.Errorf("rule %q did not match %q", c.rule, c.line)
		}
	}
	if _, ok := forbidden([]string{"sudo"}, "echo sudo"); ok {
		t.Error("rule sudo matched echo sudo")
	}
}

func TestForbiddenNoRules(t *testing.T) {
	if _, ok := forbidden(nil, "git push"); ok {
		t.Fatal("no rules matched")
	}
}

func TestExecCommands(t *testing.T) {
	for _, c := range []struct {
		script string
		want   []string
	}{
		// codex-cli 0.164.0, 2026-11-08, run x1: the push that ran.
		{"const r = await tools.exec_command({cmd:\"git -C repo push origin main && git -C repo status --short --branch\",\"max_output_tokens\":2000});\ntext(r.output);\n",
			[]string{"git -C repo push origin main && git -C repo status --short --branch"}},
		// Run x2: the key quoted, a workdir after.
		{`const r = await tools.exec_command({"cmd":"git push origin main","workdir":"/tmp/x"}); text(JSON.stringify(r))`,
			[]string{"git push origin main"}},
		{`tools.exec_command({ cmd : 'git push' }); tools.exec_command({cmd: ` + "`ls -la`" + `})`,
			[]string{"git push", "ls -la"}},
		{`tools.exec_command({cmd:"printf 'a\\n' \"x\""})`, []string{"printf 'a\\n' \"x\""}},
		{`tools.exec_command({cmd:"\` + `u0067it push"})`, []string{"git push"}},
		{`tools.exec_command({cmd:"\x67it push"})`, []string{"git push"}},
		{`tools.exec_command({cmd: /* c */ "git push"})`, []string{"git push"}},
		{"// {cmd: 'git push'}\ntools.exec_command({cmd:\"git status\"})", []string{"git status"}},
		{`/* {cmd: "git push"} */ text("example cmd: 'git push'")`, nil},
		// From the fourth review: a regular expression is not a comment.
		{`const re = /https?:\/\//; await tools.exec_command({cmd: "git push"});`, []string{"git push"}},
		{`const re = /cmd: "git push"/; text(re)`, nil},
		{`const x = a / 2; tools.exec_command({cmd: "ls"}) // b / c`, []string{"ls"}},
		// Not a cmd key, or not a literal.
		{`const cmd2 = "git push"; tools.exec_command({mycmd:"git push"})`, nil},
		{`tools.exec_command({cmd: "git " + "push"})`, []string{"git "}},
		{`const c = "git push"; tools.exec_command({cmd: c})`, nil},
	} {
		if got := execCommands(c.script); !slices.Equal(got, c.want) {
			t.Errorf("execCommands(%q) = %q, want %q", c.script, got, c.want)
		}
	}
}

// The fallback for a line the parser cannot read is taken, and refuses.
func TestUnparsedLineErrsTowardRefusing(t *testing.T) {
	for _, line := range []string{"git push 'unclosed", "echo $( ; git push"} {
		if _, err := parse(line); err == nil {
			t.Fatalf("%q parsed; the test needs a line that does not", line)
		}
		if _, ok := forbidden([]string{"git push"}, line); !ok {
			t.Errorf("%q was not refused", line)
		}
	}
}
