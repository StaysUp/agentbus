.PHONY: test gate

# Unit tests: seconds. Tests that start child processes or wait on real time
# carry the integration build tag and run in the gate.
test:
	go test ./...

# The gate: run before every commit. CI runs the same steps.
gate:
	@out=$$(gofmt -l $$(git ls-files '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go vet -tags integration ./...
	go test -race -tags integration ./...
	go build ./cmd/agentbus
