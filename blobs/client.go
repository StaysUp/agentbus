package blobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
)

// The control plane's blob routes (package jobs serves them):
//
//	POST /api/v0/blobs  {"hashes": [...]} → {"missing": [...]}
//	PUT  /api/v0/blobs/{hash}   the content, streamed; 204 once kept
//	GET  /api/v0/blobs/{hash}   the content, streamed
//
// A body of a blob is not bound by the runner protocol's 2 MiB: it is
// streamed to disk and checked against its hash as it arrives.

// MissingRequest and MissingReply are the bodies of POST /api/v0/blobs.
type MissingRequest struct {
	Hashes []string `json:"hashes"`
}
type MissingReply struct {
	Missing []string `json:"missing"`
}

// Client is the control plane's store, reached over HTTP with a runner's
// token. It is a Source and a Sink.
type Client struct {
	URL   string // the server, such as http://127.0.0.1:8787
	Token string
	HTTP  *http.Client
}

func (c Client) do(ctx context.Context, method, path string, body io.Reader, size int64, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("blobs: the control plane could not be reached")
	}
	return resp, nil
}

// Missing asks which of hs the control plane lacks.
func (c Client) Missing(ctx context.Context, hs []string) ([]string, error) {
	if len(hs) == 0 {
		return nil, nil
	}
	b, _ := json.Marshal(MissingRequest{Hashes: hs})
	resp, err := c.do(ctx, http.MethodPost, "/api/v0/blobs", bytes.NewReader(b), int64(len(b)), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("blobs: missing answered %d", resp.StatusCode)
	}
	var r MissingReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return nil, errors.New("blobs: the missing reply could not be read")
	}
	return r.Missing, nil
}

// Put sends the blob h.
func (c Client) Put(ctx context.Context, h string, r io.Reader) error {
	if !Valid(h) {
		return ErrBadHash
	}
	size := int64(-1)
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err == nil {
			size = fi.Size()
		}
	}
	resp, err := c.do(ctx, http.MethodPut, "/api/v0/blobs/"+h, r, size, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("blobs: put answered %d", resp.StatusCode)
	}
	return nil
}

// Open reads the blob h, checked against h as it is read.
func (c Client) Open(ctx context.Context, h string) (io.ReadCloser, error) {
	if !Valid(h) {
		return nil, ErrNotFound
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/v0/blobs/"+h, nil, -1, "")
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return &checked{rc: resp.Body, want: h, h: newHash()}, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	}
	resp.Body.Close()
	return nil, fmt.Errorf("blobs: get answered %d", resp.StatusCode)
}

// checked reads a blob and fails at its end when the bytes do not have
// its hash.
type checked struct {
	rc   io.ReadCloser
	want string
	h    hash.Hash
}

func newHash() hash.Hash { return sha256.New() }

func (c *checked) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.h.Write(p[:n])
	if err == io.EOF && hex.EncodeToString(c.h.Sum(nil)) != c.want {
		return n, ErrBadHash
	}
	return n, err
}

func (c *checked) Close() error { return c.rc.Close() }
