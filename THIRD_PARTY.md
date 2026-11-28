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

### golang.org/x/text v0.14.0

- What: Unicode normalization (`unicode/norm`), so two spellings of one file name, composed and
  decomposed, compare as the disk takes them (`workspace/samename.go`). It was already in the
  binary through kin-openapi; 0.6.8 uses it directly.
- Source: https://go.googlesource.com/text
- License: BSD 3-Clause, Copyright 2009 The Go Authors.

## Installed beside the binary

Not in the binary. `install.sh` installs these with `npm ci` from `packaging/srt/package-lock.json`,
so each is the exact version and hash listed there.

### sandbox-runtime (srt) 0.0.83

- What: the wall around a turn's processes (`wall/`): Seatbelt on macOS, bwrap and seccomp on
  Linux. The runner probes it and reports it; from 0.6.9 a Copy turn runs inside it.
- Where: `<install dir>/../lib/agentbus-srt`, with `srt` linked next to `agentbus`. The npm
  package `@anthropic-ai/sandbox-runtime@0.0.83`, integrity
  `sha512-yEusORnVJrNseF/9fvjVYyk6tr79/aFbIB3T/X8p2puLZdaaiGNeYN50v0CE+1DT70v7XuzKOAnTivh1f5cksA==`.
- Source: https://github.com/anthropics/sandbox-runtime
- License: Apache 2.0.
- Needs Node.js 22.12 or later, and on Linux bwrap (bubblewrap), socat and ripgrep from the
  system's packages.
- It brings: `zod` 3.25.76 (MIT), `commander` 12.1.0 (MIT), `@pondwader/socks5-server` 1.0.10
  (MIT), and `node-forge` 1.4.4 (BSD 3-Clause or GPL 2.0; used under BSD 3-Clause).

To update it, change the version in `packaging/srt/package.json`, run
`npm install --package-lock-only` there, set `wall.Version` and `srt_version` in `install.sh` to
match (`TestWallPinAgrees` checks all three), update this entry, and run a live probe.
