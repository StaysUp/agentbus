# Third-party code

## Shipped in the binary

### Redoc 2.5.4

- What: the interactive API reference served at `/docs`.
- Where: `api/viewer/redoc.standalone.js`, the file `bundles/redoc.standalone.js` from the npm
  package `redoc@2.5.4` (published 2026-09-10), unchanged. npm tarball sha1
  `c2b77b368800f56f69d43697d0fc45738fd8f466`; bundle sha256
  `dcaf76612bc4a3fbcc923a8966dee2f6146a5f32e5ce1b6f02dd60cbbf89500b`.
- Source: https://github.com/Redocly/redoc
- License: MIT, Copyright (c) 2015-present, Rebilly, Inc. Full text: `api/viewer/REDOC_LICENSE`.
- The bundle includes other open-source packages. Their notices are in
  `api/viewer/redoc.standalone.js.LICENSE.txt`, shipped with the bundle by Redoc. They are under the
  MIT license (React, classnames, Stickyfill, perfect-scrollbar, mark.js and others), except
  DOMPurify 3.4.13, which is under the Apache License 2.0 and the Mozilla Public License 2.0.

To update it, replace the three files in `api/viewer/` from a newer `redoc` release, update this
entry, and run the tests: `api/docs_test.go` checks that `/docs` serves the file in the repository.

### kin-openapi v0.152.0

- What: builds the OpenAPI description served at `/api/v0/openapi.json` when the service starts
  (`api/spec.go`, with its `openapi3gen` package). Tests also load and validate that description and
  check real responses against it.
- Source: https://github.com/getkin/kin-openapi
- License: MIT.
- It brings, also in the binary: `github.com/santhosh-tekuri/jsonschema/v6` (Apache 2.0),
  `github.com/go-openapi/jsonpointer` and `github.com/go-openapi/swag/jsonname` (Apache 2.0),
  `github.com/oasdiff/yaml` and `github.com/oasdiff/yaml3` (MIT and Apache 2.0). Tests alone use
  `github.com/gorilla/mux` (BSD 3-Clause).

### BurntSushi/toml v1.5.0

- What: reads `permissions.toml` in the data folder at the start of every turn
  (`run/permissions.go`), with the line of a parse error for the reply.
- Source: https://github.com/BurntSushi/toml
- License: MIT, Copyright (c) 2013 TOML authors.

### mvdan.cc/sh v3.14.1

- What: parses each shell command a model asks to run, so the gateway's guard can match every
  simple command in it against `forbid` (`gateway/command.go`, its `syntax` package only).
- Source: https://github.com/mvdan/sh
- License: BSD 3-Clause, Copyright (c) 2016, Daniel Martí.

### golang.org/x/sys v0.48.0

- What: system calls the standard library does not offer: overlay mounts and file clones
  (`isolation/`, `connector/`), user namespaces (`cmd/agentbus`), the network jail (`netjail/`),
  the process table on macOS (`run/`), and the terminal a subscription's login runs in
  (`credentials/pty*.go`).
- Source: https://go.googlesource.com/sys
- License: BSD 3-Clause, Copyright 2009 The Go Authors.
