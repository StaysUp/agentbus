package blobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hashOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// A blob is kept only under its own hash; the same content twice is one
// blob; missing lists only what is not held.
func TestStore(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := []byte("hello")
	h := hashOf(b)
	if err := s.Put(ctx, h, bytes.NewReader([]byte("other"))); !errors.Is(err, ErrBadHash) || s.Has(h) {
		t.Fatalf("a wrong body was kept: %v", err)
	}
	if err := s.Put(ctx, h, bytes.NewReader(b)); err != nil || !s.Has(h) {
		t.Fatal(err)
	}
	if got, err := s.PutBytes(b); err != nil || got != h {
		t.Fatal(got, err)
	}
	other := hashOf([]byte("x"))
	if m, err := s.Missing(ctx, []string{h, other}); err != nil || len(m) != 1 || m[0] != other {
		t.Fatalf("missing %v %v", m, err)
	}
	if _, err := s.Missing(ctx, []string{"../../etc"}); !errors.Is(err, ErrBadHash) {
		t.Fatal("a path was taken as a hash")
	}
	got, err := ReadAll(ctx, s, h, 100)
	if err != nil || string(got) != "hello" {
		t.Fatal(string(got), err)
	}
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("file"), 0o644)
	if fh, err := s.PutFile(p, "", nil); err != nil || fh != hashOf([]byte("file")) {
		t.Fatal(fh, err)
	}
	if _, err := s.PutFile(p, hashOf([]byte("not held")), nil); !errors.Is(err, ErrBadHash) {
		t.Fatalf("a file under another's hash: %v", err)
	}
}

// The client and a server of the same routes agree; a body that does not
// have its hash fails as it is read.
func TestClient(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v0/blobs", func(w http.ResponseWriter, r *http.Request) {
		var req MissingRequest
		_ = jsonDecode(r.Body, &req)
		m, _ := s.Missing(ctx, req.Hashes)
		jsonWrite(w, MissingReply{Missing: m})
	})
	mux.HandleFunc("PUT /api/v0/blobs/{h}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Put(ctx, r.PathValue("h"), r.Body); err != nil {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/v0/blobs/{h}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("h") == strings.Repeat("0", 64) {
			io.WriteString(w, "not its content")
			return
		}
		rc, err := s.Open(ctx, r.PathValue("h"))
		if err != nil {
			w.WriteHeader(404)
			return
		}
		defer rc.Close()
		io.Copy(w, rc)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := Client{URL: srv.URL, Token: "t"}
	b := bytes.Repeat([]byte("big "), 1<<20) // 4 MiB: past the protocol's 2 MiB
	h := hashOf(b)
	if m, err := c.Missing(ctx, []string{h}); err != nil || len(m) != 1 {
		t.Fatal(m, err)
	}
	if err := c.Put(ctx, h, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadAll(ctx, c, h, 8<<20); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("read back: %d bytes, %v", len(got), err)
	}
	rc, err := c.Open(ctx, strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrBadHash) {
		t.Fatalf("a body without its hash read as %v", err)
	}
	// Local fetches what it lacks from behind it, once.
	own, _ := Open(t.TempDir())
	l := Local{Store: own, From: c}
	if got, err := ReadAll(ctx, l, h, 8<<20); err != nil || !bytes.Equal(got, b) || !own.Has(h) {
		t.Fatalf("local: %v", err)
	}
}
