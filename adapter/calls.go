package adapter

import (
	"mime"
	"net/url"
	"regexp"
	"strings"
)

// Call is one kind of call an adapter's gateway records: its method, its
// vendor path, the query it may carry and the reply formats the gateway
// reads. A gateway that adds a credential of control's (a subscription or a
// key) adds it to these calls only, and to no other: one list, the
// adapter's, says both what is recorded and what gets a credential.
type Call struct {
	Method string
	// Path is the vendor path, exact. A segment "*" stands for one path
	// segment of letters, digits and ._- (a model name); "*" with more
	// after it in the same segment, such as "*:generateContent", stands for
	// such a name followed by the rest.
	Path string
	// Query is each query key the call may carry, with the values it may
	// take. A key not listed, or a value not listed, and the call is not
	// this one. Nil: no query.
	Query map[string][]string
	// Replies are the media types of the replies the gateway reads for this
	// call, such as text/event-stream; "" is a reply with no Content-Type.
	// A reply of any other type is not passed on.
	Replies []string
	// Service: not a model call. A call the harness needs besides its model
	// calls, such as agy's loadCodeAssist: the gateway records it by method,
	// path and status (vendor.called) and reads none of its body.
	Service bool
	// NoCredential: a service call that goes to the vendor with no
	// credential at all, such as claude's HEAD /api/hello. Control adds
	// none, and the gateway takes off any the agent sent.
	NoCredential bool
}

// segment is what a "*" of Call.Path stands for.
var segment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// matches says whether a call of method on path is c, leaving the query out.
func (c Call) matches(method, path string) bool {
	if method != c.Method {
		return false
	}
	want, got := strings.Split(c.Path, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		rest, wild := strings.CutPrefix(w, "*")
		if !wild {
			if w != got[i] {
				return false
			}
			continue
		}
		name, ok := strings.CutSuffix(got[i], rest)
		if !ok || !segment.MatchString(name) {
			return false
		}
	}
	return true
}

// Model is the model a call of c on path names in its path: what the "*"
// segment of c.Path matched, such as gemini-4-argon in
// /v1beta/models/gemini-4-argon:streamGenerateContent. "" when c's path
// names no model, or path is not c's. The Gemini API names the model only
// there, never in the body.
func (c Call) Model(path string) string {
	if !c.matches(c.Method, path) {
		return ""
	}
	want, got := strings.Split(c.Path, "/"), strings.Split(path, "/")
	for i, w := range want {
		if rest, wild := strings.CutPrefix(w, "*"); wild {
			name, _ := strings.CutSuffix(got[i], rest)
			return name
		}
	}
	return ""
}

// Admits says whether rawQuery is a query c may carry: every key one c
// lists, every value one it lists for that key. A query that does not
// parse is not.
func (c Call) Admits(rawQuery string) bool {
	if rawQuery == "" {
		return true
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	for k, vs := range q {
		allowed, ok := c.Query[k]
		if !ok {
			return false
		}
		for _, v := range vs {
			if !contains(allowed, v) {
				return false
			}
		}
	}
	return true
}

// Reads says whether a reply of contentType is one the gateway reads for c.
func (c Call) Reads(contentType string) bool {
	mt := ""
	if strings.TrimSpace(contentType) != "" {
		var err error
		if mt, _, err = mime.ParseMediaType(contentType); err != nil {
			return false
		}
	}
	return contains(c.Replies, mt)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Find is the call of a's Calls that a call of method on path is, the query
// left out.
func Find(a Adapter, method, path string) (Call, bool) {
	for _, c := range a.Calls() {
		if c.matches(method, path) {
			return c, true
		}
	}
	return Call{}, false
}

// Watches says whether a call of method on path is a model call a reads.
func Watches(a Adapter, method, path string) bool {
	c, ok := Find(a, method, path)
	return ok && !c.Service
}

// Reply formats the adapters read.
var (
	// JSON or a stream of server-sent events.
	JSONOrSSE = []string{"application/json", "text/event-stream"}
	// Either, or a reply with no Content-Type, which the adapter reads by
	// its bytes (chatgpt.com streams codex's reply with none).
	JSONOrSSEOrNone = []string{"application/json", "text/event-stream", ""}
	// JSON only.
	JSONOnly = []string{"application/json"}
)
