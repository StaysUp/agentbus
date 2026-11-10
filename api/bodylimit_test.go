package api

import (
	"net/http/httptest"
	"testing"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/jobs"
)

// Each blob route takes the body it needs: a fetch of 1,000 hashes is more
// than the API's own 64 KiB.
func TestBlobBodyLimits(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         int64
	}{
		{"PUT", "/api/v0/blobs/ab", blobs.MaxBlob},
		{"POST", "/api/v0/packs", blobs.MaxPackBody},
		{"POST", "/api/v0/packs/fetch", jobs.MaxBody},
		{"POST", "/api/v0/blobs", jobs.MaxBody},
	} {
		if got := bodyLimit(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	if jobs.MaxBody < int64(blobs.PackCount)*70 {
		t.Fatal("a fetch of PackCount hashes does not fit the protocol's body")
	}
}
