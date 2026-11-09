// Package harness says what differs between the programs that run an
// agent's loop, such as Claude Code (claude) and the Codex CLI (codex). Each
// harness is one file of this package and one entry of the registry
// (registry). The rest of the code asks the registry and never names a
// harness itself.
//
// What a harness says: its names, how to probe it, the access it offers, how
// its child reaches the service, the command line of a turn, how to read its
// resume id from its output, and the history format own replay uses for it.
//
// It imports nothing of the module, so every other package may use it
// (cmd/agentbus/layers_test.go).
//
// To add a harness: write <name>.go with a type that implements Harness, add
// it to registry, and add its cases to cmd/agentbus/launch_integration_test.go
// (then -update-launch, and read the golden diff line by line).
package harness

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Access kinds: how a harness reaches its model.
const (
	Subscription = "subscription" // the harness's own sign-in
	OpenRouter   = "openrouter"   // an OpenRouter key, from an environment variable
	API          = "api"          // the vendor's API key: not available yet
)

// History formats own replay knows (package replay).
const (
	FormatAnthropic = "anthropic" // Anthropic Messages, as Claude Code sends them
	FormatOpenAI    = "openai"    // the Responses API, as codex sends it
)

// Info is what is fixed about a harness.
type Info struct {
	// Name is the harness as an agent names it, such as claude. It is also
	// the program looked up on PATH.
	Name string
	// Label is its name for a screen and for a transcript, such as Claude
	// Code.
	Label string
	// Product is what to install, for the reply when the program is
	// missing, such as "the Codex CLI".
	Product string
	// StatusArgs is its own offline sign-in status command: exit 0 signed
	// in, 1 not. Its output is never read, since it can name the account.
	StatusArgs []string
	// OwnEndpointEnv names the variable that says the person already sends
	// this harness to another endpoint. A turn then refuses rather than send
	// that endpoint's credentials to the vendor.
	OwnEndpointEnv string
	// Access is each way it reaches a model, in the order a form shows them.
	Access []Access
	// Replay is the history format own replay continues its sessions in,
	// or "" when its sessions continue by the vendor's resume only.
	Replay string
	// ReplayOnOpenRouter says own replay knows its requests on OpenRouter
	// too. When false, a turn of it on OpenRouter neither gives nor takes a
	// replayed history: the turn after gets the earlier turns as text.
	ReplayOnOpenRouter bool
}

// Access is one way a harness reaches a model.
type Access struct {
	Kind     string
	Label    string
	Disabled bool     // listed, but no agent can use it yet
	Models   []string // to offer, first the one to start from
	AnyModel bool     // any model name is accepted
}

// OpenRouterModels are offered first for an OpenRouter agent; any OpenRouter
// model id is accepted.
var OpenRouterModels = []string{"deepseek/deepseek-v4.1-flash", "openai/gpt-oss-120b"}

// standardAccess is a subscription, an OpenRouter key, and an API key, which
// is not available yet.
func standardAccess(subLabel string, subModels []string) []Access {
	return []Access{
		{Kind: Subscription, Label: subLabel, Models: subModels},
		{Kind: OpenRouter, Label: "OpenRouter key", Models: OpenRouterModels, AnyModel: true},
		{Kind: API, Label: "API key", Disabled: true, Models: []string{}},
	}
}

// OpenRouterWire is what an OpenRouter turn's child needs.
type OpenRouterWire struct {
	// Base is the service's OpenRouter path under the turn's base URL.
	Base string
	// KeyEnv names the variable that holds the key; Key is its value.
	KeyEnv, Key string
	// Model is the turn's model, or "".
	Model string
}

// Writes is what one turn may change. Nothing when ReadOnly. Otherwise the
// files under each folder of Edit, never the files of Protect.
type Writes struct {
	ReadOnly bool
	// Edit is the turn's folder, and its real path when that differs, when
	// edit = "session". Empty: the turn edits nothing without asking.
	Edit []string
	// Protect are the data folder's own files: the policy, the
	// instructions, the saved agents and the tokens. No turn edits them.
	Protect []string
}

// Turn is what one turn's command line is built from.
type Turn struct {
	// Hint, when not empty, goes in as an instruction of the harness's own,
	// never inside the prompt.
	Hint string
	// ResumeID, when not empty, continues that vendor conversation.
	ResumeID string
	Model    string
	Writes   Writes
	// The lists of permissions.toml.
	ReadFolders, Allow, Forbid []string
	// Prompt is always last, after "--", so it is never read as a flag.
	Prompt string
}

// Harness is one program that runs an agent's loop.
type Harness interface {
	Info() Info
	// Wire is how a subscription turn's child reaches the service at
	// baseURL: what goes into its environment, and the arguments that go
	// before the turn's own.
	Wire(baseURL string) (env, args []string)
	// WireOpenRouter is the same for a turn on an OpenRouter key.
	WireOpenRouter(w OpenRouterWire) (env, args []string)
	// Edit is the edit folders a turn may get, from the turn's folder (and
	// its real path) and the files it must never edit. Empty, with why for
	// the service log, when edits must be left out.
	Edit(folders, protect []string) (edit []string, why string)
	// OwnInstructions says, with a reason for the service log, that the
	// person set this harness's instructions themselves, so a turn's hint is
	// left out. env is the child's environment.
	OwnInstructions(env []string, folder string) (why string)
	// Args is the turn's command line, after the wire's arguments.
	Args(t Turn) []string
	// ResumeID reads one line of the child's stdout and returns the resume
	// id so far: current, or the id that line names.
	ResumeID(line []byte, current string) string
}

// registry is every harness turns can start, in the order they are listed.
var registry = []Harness{claude{}, codex{}}

// planned are harnesses listed as coming, which no agent can use yet.
var planned = []Info{{Name: "opencode", Label: "opencode"}}

// Get returns the harness turns can start under name.
func Get(name string) (Harness, bool) {
	for _, h := range registry {
		if h.Info().Name == name {
			return h, true
		}
	}
	return nil, false
}

// All is every harness turns can start, in the order they are listed.
func All() []Harness { return registry }

// Names is the name of each harness of All.
func Names() []string {
	out := make([]string, len(registry))
	for i, h := range registry {
		out[i] = h.Info().Name
	}
	return out
}

// NameList is Names for a sentence: "claude or codex".
func NameList() string {
	n := Names()
	if len(n) < 2 {
		return strings.Join(n, "")
	}
	return strings.Join(n[:len(n)-1], ", ") + " or " + n[len(n)-1]
}

// Planned are the harnesses listed as coming, which no agent can use yet.
func Planned() []Info { return planned }

// resumeIDShape is the shape of a vendor's resume id: it is only ever an
// argument after "--" or of --resume, and never starts with a dash.
var resumeIDShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// stdoutLine is the fields of a JSON line of a child's stdout that name a
// resume id. Each line is decoded whole, so text inside a reply cannot pose
// as the id.
type stdoutLine struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id"`
}

func decodeLine(line []byte) (stdoutLine, bool) {
	var v stdoutLine
	return v, json.Unmarshal(line, &v) == nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
