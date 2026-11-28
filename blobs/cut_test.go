package blobs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// CutTransport answers every call 200 with its headers, then cuts the body
// part way, as a connection that drops mid-reply does.
type CutTransport struct{}

func (CutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		r.Body.Close()
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"missing":["`), errReader{})), Request: r}, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// A reply cut after its headers is no answer: the caller sends again, as for
// a control that did not answer (review t_f1724c4b98986fcb P1: it was another
// error, and a turn ended finished with no changeset).
func TestCutReplyIsUnreachable(t *testing.T) {
	c := Client{URL: "http://control", Token: "t", HTTP: &http.Client{Transport: CutTransport{}}}
	_, err := c.Missing(context.Background(), []string{strings.Repeat("a", 64)})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a cut reply: %v, want ErrUnreachable", err)
	}
}
