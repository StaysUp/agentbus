package adapter_test

import (
	"testing"

	"github.com/staysup/agentbus/adapter"
	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/adapter/gemini"
	"github.com/staysup/agentbus/adapter/openai"
)

// What each vendor's gateway records, and so adds a credential to, is its
// adapter's Calls: method, path and query. Every other call is none of them.
func TestCallsOfEachAdapter(t *testing.T) {
	for _, c := range []struct {
		a             adapter.Adapter
		method, path  string
		query         string
		found, admits bool
		service       bool
	}{
		{anthropic.Adapter{}, "POST", "/v1/messages", "", true, true, false},
		{anthropic.Adapter{}, "POST", "/v1/messages", "beta=true", true, true, false},
		{anthropic.Adapter{}, "POST", "/v1/messages", "beta=false", true, false, false},
		{anthropic.Adapter{}, "POST", "/v1/messages", "beta=true&x=1", true, false, false},
		{anthropic.Adapter{}, "DELETE", "/v1/messages", "", false, false, false},
		{anthropic.Adapter{}, "POST", "/v1/messages/count_tokens", "", false, false, false},
		{anthropic.Adapter{}, "GET", "/v1/models", "", false, false, false},
		{anthropic.Adapter{}, "POST", "/v1/messages/", "", false, false, false},
		{anthropic.Adapter{}, "POST", "//v1/messages", "", false, false, false},
		{anthropic.Adapter{}, "HEAD", "/api/hello", "", true, true, true},
		{anthropic.Adapter{}, "GET", "/api/hello", "", false, false, false},
		{anthropic.Adapter{}, "HEAD", "/api/hello", "x=1", true, false, true},

		{openai.Adapter{}, "POST", "/backend-api/codex/responses", "", true, true, false},
		{openai.Adapter{}, "POST", "/v1/responses", "", true, true, false},
		{openai.Adapter{}, "POST", "/backend-api/codex/responses", "stream=true", true, false, false},
		{openai.Adapter{}, "POST", "/backend-api/codex/responses/compact", "", false, false, false},
		{openai.Adapter{}, "POST", "/v1/responses/compact", "", false, false, false},
		{openai.Adapter{}, "POST", "/backend-api/other", "", false, false, false},
		{openai.Adapter{}, "GET", "/backend-api/codex/models", "", false, false, false},
		{openai.Adapter{}, "POST", "/v1/chat/completions", "", false, false, false},
		{openai.Adapter{}, "GET", "/backend-api/codex/responses", "", false, false, false},

		{gemini.Adapter{}, "POST", "/v1internal:streamGenerateContent", "alt=sse", true, true, false},
		{gemini.Adapter{}, "POST", "/v1internal:streamGenerateContent", "", true, true, false},
		{gemini.Adapter{}, "POST", "/v1internal:streamGenerateContent", "alt=proto", true, false, false},
		{gemini.Adapter{}, "POST", "/v1internal:streamGenerateContent", "alt=sse&alt=proto", true, false, false},
		{gemini.Adapter{}, "POST", "/v1internal:streamGenerateContent", "alt=sse;x", true, false, false},
		{gemini.Adapter{}, "POST", "/v1internal:generateContent", "alt=sse", true, false, false},
		{gemini.Adapter{}, "POST", "/v1internal:generateContent", "", true, true, false},
		{gemini.Adapter{}, "POST", "/v1beta/models/gemini-3.8-flash:streamGenerateContent", "alt=sse", true, true, false},
		{gemini.Adapter{}, "POST", "/v1beta/models/a/b:streamGenerateContent", "", false, false, false},
		{gemini.Adapter{}, "POST", "/v1beta/models/:streamGenerateContent", "", false, false, false},
		{gemini.Adapter{}, "POST", "/x/v1internal:streamGenerateContent", "", false, false, false},
		{gemini.Adapter{}, "POST", "/v1internal:loadCodeAssist", "", true, true, true},
		{gemini.Adapter{}, "POST", "/v1internal:loadCodeAssist", "alt=proto", true, false, true},
		{gemini.Adapter{}, "GET", "/v1internal:loadCodeAssist", "", false, false, false},
		{gemini.Adapter{}, "POST", "/v1internal:onboardUser", "", false, false, false},
		{gemini.Adapter{}, "POST", "/v1internal:anything", "", false, false, false},
	} {
		call, found := adapter.Find(c.a, c.method, c.path)
		if found != c.found || (found && (call.Admits(c.query) != c.admits || call.Service != c.service)) {
			t.Errorf("%s %s %s?%s: found %v admits %v service %v", c.a.Vendor(), c.method, c.path, c.query,
				found, found && call.Admits(c.query), call.Service)
		}
		if adapter.Watches(c.a, c.method, c.path) != (c.found && !c.service) {
			t.Errorf("%s %s %s: Watches disagrees with Calls", c.a.Vendor(), c.method, c.path)
		}
	}
}

// A reply passes on only in a format the adapter reads.
func TestCallReads(t *testing.T) {
	model, _ := adapter.Find(gemini.Adapter{}, "POST", "/v1internal:streamGenerateContent")
	service, _ := adapter.Find(gemini.Adapter{}, "POST", "/v1internal:loadCodeAssist")
	anth, _ := adapter.Find(anthropic.Adapter{}, "POST", "/v1/messages")
	for _, c := range []struct {
		call adapter.Call
		ct   string
		want bool
	}{
		{model, "text/event-stream", true},
		{model, "application/json; charset=UTF-8", true},
		{model, "", true},
		{model, "application/x-protobuf", false},
		{model, "application/json+protobuf", false},
		{model, "text/html", false},
		{model, "not a type;;", false},
		{service, "application/json; charset=UTF-8", true},
		{service, "", false},
		{service, "text/event-stream", false},
		{anth, "text/event-stream; charset=utf-8", true},
		{anth, "", false},
	} {
		if got := c.call.Reads(c.ct); got != c.want {
			t.Errorf("%s reads %q: %v", c.call.Path, c.ct, got)
		}
	}
}

// A call's path names its model only where its Calls entry has a "*"
// segment: the name that segment matched.
func TestCallModelFromPath(t *testing.T) {
	for _, c := range []struct {
		a      adapter.Adapter
		method string
		path   string
		want   string
	}{
		{gemini.Adapter{}, "POST", "/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent", "gemini-3.1-pro-preview"},
		{gemini.Adapter{}, "POST", "/v1beta/models/gemini-4-argon:generateContent", "gemini-4-argon"},
		{gemini.Adapter{}, "POST", "/v1internal:streamGenerateContent", ""},
		{anthropic.Adapter{}, "POST", "/v1/messages", ""},
		{openai.Adapter{}, "POST", "/v1/responses", ""},
	} {
		call, ok := adapter.Find(c.a, c.method, c.path)
		if !ok || call.Model(c.path) != c.want {
			t.Errorf("%s %s: found %v model %q, want %q", c.method, c.path, ok, call.Model(c.path), c.want)
		}
	}
	// Not the call's path: a different length, or the same length with a
	// fixed segment, or the rest of the "*" segment, that differs.
	c := adapter.Call{Path: "/v1beta/models/*:generateContent"}
	for _, p := range []string{"/v1/messages", "/v1alpha/models/gemini-4-argon:generateContent",
		"/v1beta/files/gemini-4-argon:generateContent", "/v1beta/models/gemini-4-argon:countTokens"} {
		if m := c.Model(p); m != "" {
			t.Errorf("%s is not the call's path and named model %q", p, m)
		}
	}
}
