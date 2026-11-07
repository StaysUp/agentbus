// Package docs holds the documents the service serves from its binary.
package docs

import _ "embed"

// LLMs is llms.txt: the client API in one plain file for coding agents,
// served at /docs/llms.txt.
//
//go:embed llms.txt
var LLMs []byte
