package blobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// packRoutes serves the pack routes over s, as package jobs does, counting
// each request in n when it is not nil.
func packRoutes(mux *http.ServeMux, s *Store, n *atomic.Int64) {
	count := func() {
		if n != nil {
			n.Add(1)
		}
	}
	mux.HandleFunc("POST /api/v0/runner/packs", func(w http.ResponseWriter, r *http.Request) {
		count()
		kept, err := ReadPack(r.Body, nil, PackCount, func(h string, b io.Reader) error { return s.Put(r.Context(), h, b) })
		if err != nil {
			w.WriteHeader(400)
			return
		}
		jsonWrite(w, PackReply{Kept: kept})
	})
	mux.HandleFunc("POST /api/v0/runner/packs/fetch", func(w http.ResponseWriter, r *http.Request) {
		count()
		var req FetchRequest
		_ = jsonDecode(r.Body, &req)
		var items []Item
		for _, h := range req.Hashes {
			size, err := s.Size(h)
			if err != nil {
				w.WriteHeader(404)
				return
			}
			items = append(items, Item{Hash: h, Size: size, Open: func(ctx context.Context) (io.ReadCloser, error) { return s.Open(ctx, h) }})
		}
		_ = WritePack(r.Context(), w, items)
	})
}

// Many blobs go in few requests both ways, a big one alone; every blob
// arrives whole and under its own hash.
func TestCopyByPacks(t *testing.T) {
	ctx := context.Background()
	server, _ := Open(t.TempDir())
	var packs, puts atomic.Int64
	mux := http.NewServeMux()
	packRoutes(mux, server, &packs)
	mux.HandleFunc("POST /api/v0/runner/blobs", func(w http.ResponseWriter, r *http.Request) {
		var req MissingRequest
		_ = jsonDecode(r.Body, &req)
		m, _ := server.Missing(ctx, req.Hashes)
		jsonWrite(w, MissingReply{Missing: m})
	})
	mux.HandleFunc("PUT /api/v0/runner/blobs/{h}", func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		if err := server.Put(ctx, r.PathValue("h"), r.Body); err != nil {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(204)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := Client{URL: srv.URL, Token: "t"}

	from, _ := Open(t.TempDir())
	var hs []string
	for i := range 2500 {
		h, err := from.PutBytes([]byte(fmt.Sprintf("file %d\n", i)))
		if err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
	}
	big, _ := from.PutBytes(bytes.Repeat([]byte("b"), PackAlone+1))
	empty, _ := from.PutBytes(nil)
	hs = append(hs, big, empty, hs[0]) // and one asked for twice
	if err := Copy(ctx, from, c, hs); err != nil {
		t.Fatal(err)
	}
	if packs.Load() != 3 || puts.Load() != 1 {
		t.Fatalf("sent by %d packs and %d puts, want 3 and 1", packs.Load(), puts.Load())
	}
	for _, h := range hs {
		if !server.Has(h) {
			t.Fatalf("%s not sent", h)
		}
	}
	// Sent again: nothing is missing, nothing goes.
	packs.Store(0)
	if err := Copy(ctx, from, c, hs); err != nil || packs.Load() != 0 {
		t.Fatalf("sent again: %d packs, %v", packs.Load(), err)
	}

	// Back the other way, into a store that fetches what it lacks.
	own, _ := Open(t.TempDir())
	l := Local{Store: own, From: c}
	if err := l.Fetch(ctx, hs); err != nil {
		t.Fatal(err)
	}
	if packs.Load() != 3 {
		t.Fatalf("fetched by %d packs, want 3", packs.Load())
	}
	for _, h := range hs {
		a, _ := os.ReadFile(own.Path(h))
		b, _ := os.ReadFile(from.Path(h))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s fetched as %d bytes, want %d", h, len(a), len(b))
		}
	}
	// One the server does not hold fails the fetch.
	gone := hashOf([]byte("never sent"))
	if err := (Local{Store: own, From: c}).Fetch(ctx, []string{gone}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a blob not held fetched as %v", err)
	}
}

// A pack is read only as it was written: the hashes asked for, in order,
// each with its size and content.
func TestReadPack(t *testing.T) {
	a, b := []byte("alpha"), []byte("beta")
	ha, hb := hashOf(a), hashOf(b)
	frame := func(h string, body []byte) string { return h + " " + fmt.Sprint(len(body)) + "\n" + string(body) }
	keep := func(s *Store) func(string, io.Reader) error {
		return func(h string, r io.Reader) error { return s.Put(context.Background(), h, r) }
	}
	for name, c := range map[string]struct {
		body string
		want []string
		err  error
	}{
		"whole":        {frame(ha, a) + frame(hb, b), []string{ha, hb}, nil},
		"out of order": {frame(hb, b) + frame(ha, a), []string{ha, hb}, ErrBadPack},
		"one short":    {frame(ha, a), []string{ha, hb}, ErrBadPack},
		"one more":     {frame(ha, a) + frame(hb, b), []string{ha}, ErrBadPack},
		"cut":          {frame(ha, a)[:60], nil, ErrBadPack},
		"wrong bytes":  {ha + " 4\nbeta", nil, ErrBadHash},
		"bad size":     {ha + " -1\n", nil, ErrBadPack},
		"bad hash":     {strings.ToUpper(ha) + " 5\nalpha", nil, ErrBadPack},
		"no newline":   {ha + " 5", nil, ErrBadPack},
		"too many":     {frame(ha, a) + frame(hb, b) + frame(ha, a), nil, ErrBadPack},
		// The bytes have the hash, but the frame said more: nothing kept.
		"short frame": {ha + " 6\nalpha", nil, ErrBadPack},
	} {
		s, _ := Open(t.TempDir())
		_, err := ReadPack(strings.NewReader(c.body), c.want, 2, keep(s))
		if !errors.Is(err, c.err) && !(c.err == nil && err == nil) {
			t.Errorf("%s: %v, want %v", name, err, c.err)
		}
		if name == "short frame" && s.Has(ha) {
			t.Error("a frame cut short was kept")
		}
	}
}

// A sweep removes an old blob nothing names; it keeps one named, one young,
// and one just said to be held.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir())
	put := func(b string, old bool) string {
		h, err := s.PutBytes([]byte(b))
		if err != nil {
			t.Fatal(err)
		}
		if old {
			then := time.Now().Add(-2 * time.Hour)
			os.Chtimes(s.Path(h), then, then)
		}
		return h
	}
	named, young, asked, held, gone := put("named", true), put("young", false), put("asked", true), put("held", true), put("gone", true)
	if m, _ := s.Missing(ctx, []string{asked}); len(m) != 0 {
		t.Fatal("asked is held")
	}
	if !s.Held(held) {
		t.Fatal("held is held")
	}
	kept, removed, err := s.Sweep(ctx, func(h string) bool { return h == named }, time.Hour)
	if err != nil || kept != 4 || removed != 1 {
		t.Fatalf("kept %d removed %d: %v", kept, removed, err)
	}
	for _, h := range []string{named, young, asked, held} {
		if !s.Has(h) {
			t.Fatalf("%s removed", h)
		}
	}
	if s.Has(gone) {
		t.Fatal("an old blob nothing names was kept")
	}
}

// A blob kept is new to a sweep, whatever the time of the file it came
// from (a clone keeps its source's), and so is one put again by PutBytes.
func TestKeptBlobsAreNew(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir())
	then := time.Now().Add(-2 * time.Hour)
	src := filepath.Join(t.TempDir(), "old.txt")
	os.WriteFile(src, []byte("old file"), 0o644)
	os.Chtimes(src, then, then)
	keepTime := func(from, to string) error { // a clone: the bytes and the time
		b, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, b, 0o600); err != nil {
			return err
		}
		return os.Chtimes(to, then, then)
	}
	cloned, err := s.PutFile(src, "", keepTime)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.PutBytes([]byte("put again"))
	os.Chtimes(s.Path(again), then, then)
	if _, err := s.PutBytes([]byte("put again")); err != nil {
		t.Fatal(err)
	}
	if _, removed, _ := s.Sweep(ctx, func(string) bool { return false }, time.Hour); removed != 0 || !s.Has(cloned) || !s.Has(again) {
		t.Fatalf("removed %d of blobs just kept", removed)
	}
}

// A pack whose source stalls ends with its context, and the source is
// closed.
func TestPutPackEndsWithItsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body) }))
	defer srv.Close()
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Client{URL: srv.URL, Token: "t"}.PutPack(ctx, []Item{{Hash: strings.Repeat("a", 64), Size: 10,
			Open: func(context.Context) (io.ReadCloser, error) { return pr, nil }}})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled pack was sent")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PutPack waited on a stalled source")
	}
	if _, err := pw.Write([]byte("x")); err == nil {
		t.Fatal("the stalled source was not closed")
	}
}

// blocking is a source whose Open waits for its context: a pack still
// opening ends when another pack fails.
type blocking struct{ n int64 }

func (b blocking) Size(string) (int64, error) { return b.n, nil }
func (blocking) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type failFirst struct{ calls atomic.Int64 }

func (f *failFirst) PutPack(ctx context.Context, items []Item) error {
	if f.calls.Add(1) == 1 {
		return errors.New("refused")
	}
	_, err := items[0].Open(ctx)
	return err
}
func (*failFirst) Put(context.Context, string, io.Reader) error             { return nil }
func (*failFirst) Missing(_ context.Context, hs []string) ([]string, error) { return hs, nil }

func TestOnePackFailingEndsTheRest(t *testing.T) {
	var hs []string
	for i := range 3 * PackCount {
		hs = append(hs, hashOf([]byte(fmt.Sprint(i))))
	}
	done := make(chan error, 1)
	go func() { done <- putPacks(context.Background(), blocking{1}, blocking{1}, &failFirst{}, hs) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a failed pack was not an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the other packs waited on their sources")
	}
}

// Clean leaves a file this handle is still writing, however old its time
// (a clone keeps its source's), and removes it once it is not.
func TestCleanLeavesFilesInFlight(t *testing.T) {
	s, _ := Open(t.TempDir())
	os.MkdirAll(filepath.Join(s.Dir(), "tmp"), 0o700)
	p := filepath.Join(s.Dir(), "tmp", "link-x")
	os.WriteFile(p, []byte("clone"), 0o600)
	then := time.Now().Add(-48 * time.Hour)
	os.Chtimes(p, then, then)
	s.writing.Store("link-x", true)
	s.Clean(time.Hour)
	if _, err := os.Stat(p); err != nil {
		t.Fatal("a file in flight was cleaned away")
	}
	s.writing.Delete("link-x")
	s.Clean(time.Hour)
	if _, err := os.Stat(p); err == nil {
		t.Fatal("a file left over was kept")
	}
}

// A server that drops the connection ends the upload with an error; it
// does not wait.
func TestPutPackServerGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	from, _ := Open(t.TempDir())
	h, _ := from.PutBytes([]byte("a blob"))
	done := make(chan error, 1)
	go func() {
		done <- Client{URL: srv.URL, Token: "t"}.PutPack(context.Background(), []Item{{Hash: h, Size: 6,
			Open: func(ctx context.Context) (io.ReadCloser, error) { return from.Open(ctx, h) }}})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an upload to a server gone was not an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PutPack waited after the server went")
	}
}
