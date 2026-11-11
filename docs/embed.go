// Package docs holds the documents the service serves from its binary.
package docs

import _ "embed"

// LLMs is llms.txt: a short page a coding agent reads in one go, served at
// /docs/llms.txt: the three nouns and the routes an agent calls.
//
//go:embed llms.txt
var LLMs []byte

// Reference is the long reference in one plain file, served at
// /docs/reference.md: every mode, setting and route in words.
//
//go:embed reference.md
var Reference []byte
