package gateway

// The guard's command matcher. A forbid entry such as "git push" is a command
// and its leading words. A model writes commands in many shapes: chained with
// && (Claude Code, 2026-11-08), or with git's global options before the
// subcommand, as in "git -C repo push" (codex-cli 0.164.0, 2026-11-08, which
// the first prefix rule missed, so the push ran). So the command line is
// parsed as shell, with mvdan.cc/sh, and every simple command in it is
// matched alone: inside pipelines, && and ||, subshells, $(…), backquotes,
// here-documents and here-strings fed to a shell, and sh -c scripts.
//
// It reads text only. A command built at run time, or run from a script
// written earlier, is not seen.

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// forbidden returns the first entry of rules that one of line's simple
// commands matches, and whether one did.
func forbidden(rules []string, line string) (string, bool) {
	if len(rules) == 0 {
		return "", false
	}
	return matchLine(rules, line, 0)
}

// maxNesting bounds sh -c inside sh -c.
const maxNesting = 4

// nonLiteral stands for a word part only known at run time, such as $DIR. It
// matches no rule word.
const nonLiteral = "\x00"

func parse(line string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(line), "")
}

func matchLine(rules []string, line string, depth int) (string, bool) {
	if depth > maxNesting {
		return "", false
	}
	f, err := parse(line)
	if err != nil {
		// A line the parser cannot read: the shell may still run part of
		// it, so err toward refusing. Any run of words that reads as a
		// rule matches.
		return matchTokens(rules, line)
	}
	rule, found := "", false
	syntax.Walk(f, func(n syntax.Node) bool {
		if found {
			return false
		}
		st, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		call, ok := st.Cmd.(*syntax.CallExpr)
		if !ok {
			return true
		}
		rule, found = matchCall(rules, st, call, depth)
		return !found
	})
	return rule, found
}

// matchCall matches one simple command, and the scripts it hands to a shell.
func matchCall(rules []string, st *syntax.Stmt, call *syntax.CallExpr, depth int) (string, bool) {
	words := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		words = append(words, wordText(w))
	}
	for len(words) > 0 {
		// Each stage is matched, so a rule may name a wrapper such as
		// sudo, and the command it runs is matched too.
		for _, r := range rules {
			if matchWords(strings.Fields(r), words) {
				return r, true
			}
		}
		next, ok := skipPrefix(words)
		if !ok {
			break
		}
		words = next
	}
	if len(words) == 0 {
		return "", false
	}
	var scripts []string
	switch {
	case isShell(words[0]):
		if script, ok := shellScript(words); ok {
			scripts = append(scripts, script)
		} else {
			// A shell reading its script from a here-document or a
			// here-string.
			for _, rd := range st.Redirs {
				switch {
				case (rd.Op == syntax.Hdoc || rd.Op == syntax.DashHdoc) && rd.Hdoc != nil:
					scripts = append(scripts, wordText(rd.Hdoc))
				case rd.Op == syntax.WordHdoc && rd.Word != nil:
					scripts = append(scripts, wordText(rd.Word))
				}
			}
		}
	case path.Base(words[0]) == "eval":
		scripts = append(scripts, strings.Join(words[1:], " "))
	}
	for _, s := range scripts {
		if r, ok := matchLine(rules, s, depth+1); ok {
			return r, true
		}
	}
	return "", false
}

// wordText is a word's value when it is literal. A part known only at run
// time ($VAR, $(…)) becomes nonLiteral. The commands inside $(…) are
// matched on their own, as the walk reaches them.
func wordText(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit:
			b.WriteString(unescape(x.Value, ""))
		case *syntax.SglQuoted:
			if x.Dollar {
				b.WriteString(decodeEscapes(x.Value))
			} else {
				b.WriteString(x.Value)
			}
		case *syntax.DblQuoted:
			for _, q := range x.Parts {
				if l, ok := q.(*syntax.Lit); ok {
					b.WriteString(unescape(l.Value, "$`\"\\\n"))
				} else {
					b.WriteString(nonLiteral)
				}
			}
		default:
			b.WriteString(nonLiteral)
		}
	}
	return b.String()
}

// unescape removes the shell's backslashes from a literal: before any
// character when only is empty (outside quotes), else before those in only
// (inside double quotes). A backslash-newline joins lines.
func unescape(s, only string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (only == "" || strings.IndexByte(only, s[i+1]) >= 0) {
			i++
			if s[i] != '\n' {
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// decodeEscapes reads the backslash escapes of a $'…' string.
func decodeEscapes(s string) string {
	v, _, ok := jsString("'"+strings.ReplaceAll(s, "'", "\\'")+"'", 0)
	if !ok {
		return s
	}
	return v
}

// matchTokens is the fallback for a line that does not parse: the line cut
// at white space, quotes and shell operators, and every run of words checked
// against every rule.
func matchTokens(rules []string, line string) (string, bool) {
	toks := strings.FieldsFunc(line, func(r rune) bool {
		return strings.ContainsRune(" \t\r\n;&|()<>`\"'$\\{}", r)
	})
	for i := range toks {
		for _, r := range rules {
			if matchWords(strings.Fields(r), toks[i:]) {
				return r, true
			}
		}
	}
	return "", false
}

// splitWords is a command line's first simple command as words.
func splitWords(line string) []string {
	f, err := parse(line)
	if err != nil {
		return strings.Fields(line)
	}
	var out []string
	syntax.Walk(f, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && out == nil {
			for _, w := range c.Args {
				out = append(out, wordText(w))
			}
			return false
		}
		return out == nil
	})
	return out
}

// prefixCommands run the command after them: "sudo git push" is a push. Each
// lists its options that take the next word as their value.
var prefixCommands = map[string]map[string]bool{
	"sudo": {"-u": true, "-g": true, "-C": true, "-D": true, "-h": true, "-p": true, "-r": true,
		"-t": true, "-U": true, "-T": true, "-R": true, "--user": true, "--group": true,
		"--chdir": true, "--prompt": true, "--role": true, "--type": true, "--other-user": true,
		"--host": true, "--close-from": true, "--command-timeout": true, "--chroot": true},
	"env":     {"-u": true, "-C": true, "-S": true, "--unset": true, "--chdir": true, "--split-string": true},
	"command": {},
	"builtin": {},
	"exec":    {"-a": true},
	"nohup":   {},
	"time":    {"-f": true, "-o": true},
	"nice":    {"-n": true, "--adjustment": true},
	"xargs": {"-I": true, "-n": true, "-L": true, "-P": true, "-d": true, "-E": true, "-s": true,
		"-a": true, "--max-args": true, "--max-procs": true, "--delimiter": true, "--arg-file": true},
}

// skipPrefix drops one leading VAR=value word, or one command that runs the
// next one, with its options and their values. ok is false when words start
// with neither.
func skipPrefix(words []string) (rest []string, ok bool) {
	w := words[0]
	if isAssignment(w) {
		return words[1:], true
	}
	opts, ok := prefixCommands[path.Base(w)]
	if !ok {
		return words, false
	}
	words = words[1:]
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		o := words[0]
		words = words[1:]
		if o == "--" {
			break
		}
		// env -S "<command line>" splits its value into the command.
		if path.Base(w) == "env" {
			v, ok := strings.CutPrefix(o, "--split-string=")
			if !ok && strings.HasPrefix(o, "-S") && !strings.HasPrefix(o, "--") && len(o) > 2 {
				v, ok = o[2:], true
			}
			if !ok && (o == "-S" || o == "--split-string") && len(words) > 0 {
				v, ok, words = words[0], true, words[1:]
			}
			if ok {
				return append(splitWords(v), words...), true
			}
		}
		if opts[o] && len(words) > 0 {
			words = words[1:]
		}
	}
	return words, true
}

func isShell(w string) bool {
	switch path.Base(w) {
	case "sh", "bash", "zsh", "dash", "ksh":
		return true
	}
	return false
}

func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// shellOptionsWithValue are a shell's options that take the next word.
var shellOptionsWithValue = map[string]bool{"-o": true, "+o": true, "-O": true, "+O": true,
	"--rcfile": true, "--init-file": true}

// shellScript is the script of sh -c, bash -lc, bash --norc -e -c and the
// like: the first word after the options, when one of them holds c. A first
// word without -c is a script file, and the words after it its arguments.
func shellScript(words []string) (string, bool) {
	seenC := false
	for i := 1; i < len(words); i++ {
		f := words[i]
		switch {
		case f == "--":
			if seenC && i+1 < len(words) {
				return words[i+1], true
			}
			return "", false
		case shellOptionsWithValue[f]:
			i++
		case strings.HasPrefix(f, "--"):
		case len(f) > 1 && (f[0] == '-' || f[0] == '+'):
			if f[0] == '-' && strings.Contains(f, "c") {
				seenC = true
			}
		default:
			return f, seenC
		}
	}
	return "", false
}

// gitOptionsWithValue are git's global options that take the next word as
// their value when written without =.
var gitOptionsWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true,
	"--work-tree": true, "--namespace": true, "--super-prefix": true, "--config-env": true}

// matchWords says whether the command words start with the rule's words. The
// first word of each matches by its base name, so /usr/bin/git is git, both
// in a command and in a rule. For git, the global options before the
// subcommand are skipped, so "git -C repo push" matches "git push" and
// "git log --grep push" does not. A rule's own git options are skipped the
// same way, so "git -C repo push" as a rule means any git push.
func matchWords(rule, words []string) bool {
	if len(rule) == 0 || len(words) == 0 || path.Base(words[0]) != path.Base(rule[0]) {
		return false
	}
	want, rest := rule[1:], words[1:]
	if path.Base(rule[0]) == "git" {
		want, rest = gitSubcommand(want), gitSubcommand(rest)
	}
	if len(rest) < len(want) {
		return false
	}
	for i, r := range want {
		if rest[i] != r {
			return false
		}
	}
	return true
}

// gitSubcommand is git's words after its global options.
func gitSubcommand(words []string) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		opt := words[0]
		words = words[1:]
		if gitOptionsWithValue[opt] && len(words) > 0 {
			words = words[1:]
		}
	}
	return words
}

// execCommands reads each cmd of a codex exec script, the JavaScript its exec
// tool runs, such as
//
//	const r = await tools.exec_command({cmd:"git push origin main",workdir:"…"});
//
// (codex-cli 0.164.0, 2026-11-08). It finds every cmd key followed by a
// string literal. A cmd built at run time, such as "git " + "push" or a
// variable, is not seen. A cmd key anywhere counts, not only inside an
// exec_command call: {cmd: "git push"} kept in a variable is refused even if
// the script never runs it. That errs on the side of refusing, since an
// object built first and passed later, exec_command(o), runs it.
func execCommands(script string) []string {
	var out []string
	key := false // the last token was a cmd key
	// prev is the last character that was not space or a comment, and
	// prevWord the last word, to tell a regular expression from a division.
	var prev byte
	prevWord := ""
	for i := 0; i < len(script); {
		c := script[i]
		switch {
		case strings.HasPrefix(script[i:], "//"):
			i = lineEnd(script, i)
		case strings.HasPrefix(script[i:], "/*"):
			if e := strings.Index(script[i+2:], "*/"); e >= 0 {
				i += e + 4
			} else {
				i = len(script)
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '/' && regexAllowed(prev, prevWord):
			// A regular expression literal, such as /https?:\/\//: its
			// slashes are not a comment, and its text is not code.
			i = regexEnd(script, i)
			prev, prevWord, key = 'r', "", false
		case c == '"' || c == '\'' || c == '`':
			v, end, _ := jsString(script, i)
			i = end
			prev, prevWord = c, ""
			if key {
				out = append(out, v)
				key = false
				continue
			}
			// A quoted key: "cmd":
			key = v == "cmd" && nextIs(script, i, ':')
			if key {
				i = skipJS(script, i) + 1
				prev = ':'
			}
		case isIdent(c):
			k := i
			for k < len(script) && isIdent(script[k]) {
				k++
			}
			prev, prevWord = c, script[i:k]
			key = script[i:k] == "cmd" && nextIs(script, k, ':')
			i = k
			if key {
				i = skipJS(script, i) + 1
				prev = ':'
			}
		default:
			prev, prevWord, key = c, "", false
			i++
		}
	}
	return out
}

// regexAllowed says whether a / after prev (and the word prevWord) starts a
// regular expression rather than a division.
func regexAllowed(prev byte, prevWord string) bool {
	switch prevWord {
	case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw", "instanceof", "yield", "await":
		return true
	case "":
	default:
		return false
	}
	return prev == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", prev) >= 0
}

// regexEnd is the index after the regular expression literal at s[i],
// its flags included.
func regexEnd(s string, i int) int {
	class := false
	for i++; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case c == '[':
			class = true
		case c == ']':
			class = false
		case c == '\n':
			return i
		case c == '/' && !class:
			i++
			for i < len(s) && isIdent(s[i]) {
				i++
			}
			return i
		}
	}
	return i
}

func lineEnd(s string, i int) int {
	if e := strings.IndexByte(s[i:], '\n'); e >= 0 {
		return i + e
	}
	return len(s)
}

// skipJS skips white space and comments from i.
func skipJS(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r':
			i++
		case strings.HasPrefix(s[i:], "//"):
			i = lineEnd(s, i)
		case strings.HasPrefix(s[i:], "/*"):
			if e := strings.Index(s[i+2:], "*/"); e >= 0 {
				i += e + 4
			} else {
				i = len(s)
			}
		default:
			return i
		}
	}
	return i
}

func nextIs(s string, i int, c byte) bool {
	i = skipJS(s, i)
	return i < len(s) && s[i] == c
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// jsString reads the JavaScript string literal at s[i], quoted with ', " or
// a backquote, and returns its value and the index after it. A template's
// ${…} parts are kept as written.
func jsString(s string, i int) (string, int, bool) {
	if i >= len(s) || strings.IndexByte("'\"`", s[i]) < 0 {
		return "", i, false
	}
	q := s[i]
	var b strings.Builder
	for k := i + 1; k < len(s); k++ {
		c := s[k]
		switch {
		case c == q:
			return b.String(), k + 1, true
		case c == '\\' && k+1 < len(s):
			k++
			switch s[k] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'x':
				if k+2 < len(s) {
					if r, ok := hexN(s[k+1 : k+3]); ok {
						b.WriteRune(r)
						k += 2
						continue
					}
				}
				b.WriteByte('x')
			case 'u':
				if k+4 < len(s) {
					if r, ok := hexN(s[k+1 : k+5]); ok {
						b.WriteRune(r)
						k += 4
						continue
					}
				}
				b.WriteByte('u')
			case '\n':
			default:
				b.WriteByte(s[k])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", len(s), false
}

func hexN(s string) (rune, bool) {
	var r rune
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			r = r*16 + rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r*16 + rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r*16 + rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}
