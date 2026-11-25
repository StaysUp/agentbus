package harness

// A provider is a model API that speaks Chat Completions, reached by a
// harness that can be pointed at any such endpoint (OpenCode). It is data:
// its name (the access kind an agent names), its base URL, the key set in
// Settings that control adds to its calls, and the models offered first. Its calls go through the service at ProviderPrefix(name) and on to
// URL + /v1/chat/completions, recorded by adapter/chatcompletions, as an
// OpenRouter agent's are. The child never holds the key: it gets a
// placeholder, and control's relay puts the key on the call.
//
// OpenCode Zen's free models (opencode/big-pickle) are not a provider: Zen
// answers every client but OpenCode's own "Free usage limit reached. Free
// models work in OpenCode", and through the gateway the unmodified opencode
// 2.1.1 got that 429 too, with its own headers passed on (checked
// 2026-11-24: same headers by curl, HTTP/1.1 and 2, refused; the same opencode
// straight to Zen answered). Reaching them would mean posing as OpenCode.
type Provider struct {
	// Name is the access kind an agent names, and the relay's name for it.
	Name string
	// Label is its name for the person.
	Label string
	// URL is its API base, above /v1.
	URL string
	// Key names the key set in Settings that its calls carry.
	Key string
	// KeyLabel is the key's name for the person, such as "xAI API key".
	KeyLabel string
	// Models are offered first; any model id is accepted.
	Models []string
}

// Providers are every provider an agent can name.
//
// xai: xAI's API, https://api.x.ai/v1/chat/completions with an xAI API key
// (OpenCode's own xai provider, packages/core/src/plugin/provider/xai.ts,
// opencode 54b4698, 2026-11-17). Its usage reports its own charge as
// cost_in_usd_ticks (adapter/chatcompletions). grok-4.7 is litellm's newest
// xai/ chat model (model_prices_and_context_window.json, 2026-11-24).
var Providers = []Provider{
	{Name: "xai", Label: "xAI key", URL: "https://api.x.ai", Key: "xai", KeyLabel: "xAI API key",
		Models: []string{"grok-4.7", "grok-4.6", "grok-4.5"}},
}

// ProviderOf is the provider an access kind names.
func ProviderOf(kind string) (Provider, bool) {
	for _, p := range Providers {
		if p.Name == kind {
			return p, true
		}
	}
	return Provider{}, false
}

// ProviderPrefix is the path under a turn's base URL that the service
// forwards to the provider named name.
func ProviderPrefix(name string) string { return "/p/" + name }

// providerAccess is an Access for each provider.
func providerAccess() []Access {
	out := make([]Access, len(Providers))
	for i, p := range Providers {
		out[i] = Access{Kind: p.Name, Label: p.Label, Models: p.Models, AnyModel: true}
	}
	return out
}
