package run

import (
	"slices"
	"strings"
)

// A turn in a home of its own gets an environment of its own. The home keeps
// the runner user's files away from the agent; the service's environment
// would bring their secrets back in (GH_TOKEN, AWS_SECRET_ACCESS_KEY, another
// vendor's API key), since a child inherits every variable its parent does not
// remove. So such a turn starts from an allow list, never from a list of
// secrets to remove: a secret has no shape a list could match, and a new one
// must not need a change here to stay out. (2026-11-24: a runner's own
// GH_TOKEN and AWS keys reached every agent it ran.)

// baseEnv are the variables of the service's own environment that a turn in a
// home of its own keeps: where programs are, who and where it runs, the
// language and terminal, the temp folder, and how this machine reaches the
// network (proxies, extra CA certificates), which its tools need as the
// person's own shell does. A proxy URL may carry a proxy's own sign-in; it is
// the machine's, and the agent's tools need it to reach anything. Every LC_
// variable is kept too (keptPrefix).
var baseEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"LANG", "LANGUAGE", "TZ", "TERM", "COLORTERM", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
}

// keptPrefix is the one family of variables kept by prefix: the locale's.
const keptPrefix = "LC_"

// turnVars are the variables turnEnv sets for every turn: the service's
// address, the turn's own token and who asked for the turn.
var turnVars = []string{"AGENTBUS_URL", "AGENTBUS_TOKEN", "AGENTBUS_ASKED_BY_SESSION", "AGENTBUS_ASKED_BY_TURN"}

// cleanEnv is env with only the variables a turn in a home of its own keeps:
// baseEnv, the locale's, turnVars, keep (those the harness's wiring set,
// Launch.Keep) and pass (the ones the person named, Config.PassEnv). Order is
// kept.
func cleanEnv(env, keep, pass []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, keptPrefix) || slices.Contains(baseEnv, k) || slices.Contains(turnVars, k) ||
			slices.Contains(keep, k) || slices.Contains(pass, k) {
			out = append(out, kv)
		}
	}
	return out
}
