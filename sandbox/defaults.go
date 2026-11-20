package sandbox

// The wall's defaults, applied whenever it is built: the folders and files
// it hides from the agent even inside a Read grant, and the git environment
// that turns an ssh remote into HTTPS so a fetch goes through the proxy and
// ssh is ruled out. The hosts the wall lets through by default live beside
// the proxy, in netjail.DefaultAllow, and are added a layer up in
// run.wallNet, where an offline turn can be told apart.
//
// Hiding, not merely not granting: a session already reaches nothing of the
// person's home (it runs in a copy, and the home is not a readable path).
// These make the secret paths stay hidden even when a Read grant, or a
// later read-out tool, reaches a folder that holds them -- "deny beats
// allow", the shape Anthropic's sandbox-runtime calls mandatory deny paths.
// What is taken from srt: the idea of a fixed set that wins over any allow;
// ".git/config" and the rest of srt's longer list are left to a later step.
//
// macOS does the hiding through the Seatbelt profile (sandbox_darwin.go). On
// Linux the agent sees only what bwrap binds in, so an ungranted secret is
// already invisible; hiding a secret that a Read grant DOES bind in is a
// known gap, left to a later step (see the package doc and TODO.md).
// Measured in research/2026-11-20-srt-spike.md.

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// homeDir is the person's home, where the secret paths live. A package var
// so a test can point the set at a folder of its own and never name a real
// secret.
var homeDir = os.UserHomeDir

// secretRel are the secret paths under the person's home, every platform:
// tool sign-ins, keys and registry tokens.
var secretRel = []string{
	".ssh", ".aws", ".gnupg",
	filepath.Join(".config", "gh"),
	filepath.Join(".docker", "config.json"),
	".npmrc", ".netrc",
}

// secretPaths are the absolute paths the wall hides, under the person's
// home, plus the machine's keychains on macOS. Each goes in as it is AND,
// when it is a symlink, as its resolved target: on macOS Seatbelt matches a
// read against the resolved path, so a deny on the link alone would let a
// symlinked dotfile (~/.config -> ~/dotfiles/config, as Stow or chezmoi
// make) slip past. The unresolved path stays too, so a secret that is not
// there now -- and may be recreated before the next turn -- is still denied.
func secretPaths() []string {
	var out []string
	add := func(p string) {
		out = append(out, p)
		if rp, err := filepath.EvalSymlinks(p); err == nil && rp != p {
			out = append(out, rp)
		}
	}
	if home, err := homeDir(); err == nil && home != "" {
		if real, err := filepath.EvalSymlinks(home); err == nil {
			home = real
		}
		for _, rel := range secretRel {
			add(filepath.Join(home, rel))
		}
		if runtime.GOOS == "darwin" {
			add(filepath.Join(home, "Library", "Keychains"))
		}
	}
	if runtime.GOOS == "darwin" {
		add("/Library/Keychains")
	}
	return out
}

// gitDefaultsEnv rewrites an ssh git remote to HTTPS so a fetch goes
// through the proxy (which carries HTTPS, not ssh), and rules ssh itself
// out so a remote it misses fails at once rather than hanging on a refused
// dial. A later step holds the push itself; here the remote is only made
// reachable. insteadOf is multi-valued, so each host takes two entries.
func gitDefaultsEnv() []string {
	pairs := [][2]string{
		{"url.https://github.com/.insteadOf", "git@github.com:"},
		{"url.https://github.com/.insteadOf", "ssh://git@github.com/"},
		{"url.https://gitlab.com/.insteadOf", "git@gitlab.com:"},
		{"url.https://gitlab.com/.insteadOf", "ssh://git@gitlab.com/"},
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, p := range pairs {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+p[0], "GIT_CONFIG_VALUE_"+n+"="+p[1])
	}
	// ssh is ruled out: a remote the rewrites miss fails fast, not on a
	// dial the wall would refuse after a timeout.
	return append(env, "GIT_SSH_COMMAND=false")
}
