package api

import (
	"net/http/httptest"
	"testing"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/jobs"
)

// Each runner protocol route takes the body it needs: a fetch of 1,000 hashes
// or a finish with a long reply is more than the API's own 64 KiB. A client
// route keeps the API's own limit.
func TestBlobBodyLimits(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         int64
	}{
		{"PUT", "/api/v0/runner/blobs/ab", blobs.MaxBlob},
		{"POST", "/api/v0/runner/packs", blobs.MaxPackBody},
		{"POST", "/api/v0/runner/packs/fetch", jobs.MaxBody},
		{"POST", "/api/v0/runner/blobs", jobs.MaxBody},
		{"POST", "/api/v0/runner/register", jobs.MaxBody},
		{"POST", "/api/v0/runner/attempts/t_1/finish", jobs.MaxBody},
		{"POST", "/api/v0/runner/attempts/t_1/record", jobs.MaxBody},
		{"POST", "/api/v0/turns", MaxRunBody},
		{"POST", "/api/v0/turns/t_1/review", MaxBody},
		{"PATCH", "/api/v0/sessions/s1", MaxBody},
	} {
		if got := bodyLimit(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	if jobs.MaxBody <= MaxBody {
		t.Fatal("the runner protocol's body is no bigger than the API's")
	}
	if jobs.MaxBody < int64(blobs.PackCount)*70 {
		t.Fatal("a fetch of PackCount hashes does not fit the protocol's body")
	}
}
