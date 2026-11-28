.PHONY: test gate

# Unit tests: seconds. Tests that start child processes or wait on real time
# carry the integration build tag and run in the gate.
test:
	go test ./...

# The gate: run before every commit. CI runs the same steps.
gate:
	@git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "make gate: run it inside a git checkout (it lists the Go files with git ls-files)"; exit 1; }
	@out=$$(gofmt -l $$(git ls-files '*.go') </dev/null); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go vet -tags integration ./...
	go test -race -tags integration -timeout 20m ./...
	go build ./cmd/agentbus
