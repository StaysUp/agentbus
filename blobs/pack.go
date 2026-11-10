package blobs

// Many blobs in one request: a pack. A snapshot of 50,000 files is 50,000
// blobs, and one request each costs a round trip each. A pack is a stream of
// frames, each a header line and then the blob's bytes:
//
//	<64 hex> <size in bytes>\n<size bytes>
//
// The control plane's pack routes (package jobs serves them):
//
//	POST /api/v0/packs    a pack → {"kept": n}
//	POST /api/v0/packs/fetch   {"hashes": [...]} → a pack of them, in that
//	                           order; 404 with {"missing": [...]} when one is
//	                           not held
//
// Every blob is checked against its hash as it arrives, as a single PUT is.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	// PackBytes bounds the blob bytes one sent pack holds. A blob of
	// PackAlone or more goes alone, by PUT.
	PackBytes = 8 << 20
	PackAlone = 1 << 20
	// PackCount bounds the blobs one pack holds, sent or fetched.
	PackCount = 1000
	// MaxPackBody bounds a sent pack's body: its blobs and their headers.
	MaxPackBody = PackBytes + PackCount*frameMax
	// PackWorkers is how many packs go at once.
	PackWorkers = 4

	frameMax = 64 + 1 + 20 + 1 // a header line: hash, space, size, newline
)

// ErrBadPack says a pack is not frames of blobs as asked.
var ErrBadPack = errors.New("blobs: the pack is not well formed")

// FetchRequest is the body of POST /api/v0/packs/fetch.
type FetchRequest struct {
	Hashes []string `json:"hashes"`
}

// PackReply is the answer to POST /api/v0/packs.
type PackReply struct {
	Kept int `json:"kept"`
}

// Item is one blob of a pack to send.
type Item struct {
	Hash string
	Size int64
	// Open reads the blob; ctx ends when the pack it is in ends.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

func header(h string, size int64) string { return h + " " + strconv.FormatInt(size, 10) + "\n" }

// WritePack writes the frames of items to w, each blob read from its Open
// with ctx and checked to be Size bytes.
func WritePack(ctx context.Context, w io.Writer, items []Item) error {
	for _, it := range items {
		if _, err := io.WriteString(w, header(it.Hash, it.Size)); err != nil {
			return err
		}
		rc, err := it.Open(ctx)
		if err != nil {
			return fmt.Errorf("blobs: %s could not be read: %w", it.Hash, err)
		}
		n, err := io.Copy(w, io.LimitReader(rc, it.Size))
		rc.Close()
		if err != nil {
			return err
		}
		if n != it.Size {
			return ErrBadPack
		}
	}
	return nil
}

// ReadPack reads the frames r holds and gives each blob to keep, which must
// read it to its end; a frame cut short fails that read, so keep keeps
// nothing of it. want, when not nil, are the hashes in the order they must
// come, all of them. At most max frames are read. It answers how many
// blobs it read.
func ReadPack(r io.Reader, want []string, max int, keep func(h string, r io.Reader) error) (int, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	n := 0
	for {
		line, err := br.ReadSlice('\n')
		if err == io.EOF && len(line) == 0 {
			if want != nil && n != len(want) {
				return n, ErrBadPack
			}
			return n, nil
		}
		if err != nil || len(line) > frameMax {
			return n, ErrBadPack
		}
		h, s, ok := strings.Cut(strings.TrimSuffix(string(line), "\n"), " ")
		size, perr := strconv.ParseInt(s, 10, 64)
		if !ok || !Valid(h) || perr != nil || size < 0 || size > MaxBlob {
			return n, ErrBadPack
		}
		if n >= max || (want != nil && (n >= len(want) || want[n] != h)) {
			return n, ErrBadPack
		}
		body := &counted{r: io.LimitReader(br, size), size: size}
		if err := keep(h, body); err != nil {
			return n, err
		}
		if body.n != size {
			return n, ErrBadPack
		}
		n++
	}
}

// counted reads one frame's body, and fails at an end before its size.
type counted struct {
	r       io.Reader
	n, size int64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err == io.EOF && c.n < c.size {
		return n, ErrBadPack
	}
	return n, err
}

// Size is the size of the blob h, held.
func (s *Store) Size(h string) (int64, error) {
	if !Valid(h) {
		return 0, ErrNotFound
	}
	fi, err := os.Lstat(s.Path(h))
	if err != nil || !fi.Mode().IsRegular() {
		return 0, ErrNotFound
	}
	return fi.Size(), nil
}

// PutPack sends items, one pack, to the control plane. The pack is read
// into memory first (at most PackBytes of blobs), so the request's body
// never waits on a source: net/http waits for a body to end before it
// answers, whatever the server did. A source being read when ctx ends is
// closed.
func (c Client) PutPack(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	var mu sync.Mutex
	var reading io.Closer
	ended := false
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		ended = true
		if reading != nil {
			reading.Close()
		}
	})
	wrapped := make([]Item, len(items))
	for i, it := range items {
		wrapped[i] = it
		wrapped[i].Open = func(ctx context.Context) (io.ReadCloser, error) {
			rc, err := it.Open(ctx)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			if ended {
				rc.Close()
				return nil, ctx.Err()
			}
			reading = rc
			return rc, nil
		}
	}
	var body bytes.Buffer
	err := WritePack(ctx, &body, wrapped)
	stop()
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/v0/packs", bytes.NewReader(body.Bytes()), int64(body.Len()), "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("blobs: pack answered %d", resp.StatusCode)
	}
	return nil
}

// GetPack fetches the blobs hs, one pack, from the control plane, and gives
// each to keep in hs's order.
func (c Client) GetPack(ctx context.Context, hs []string, keep func(h string, r io.Reader) error) error {
	if len(hs) == 0 {
		return nil
	}
	b, _ := json.Marshal(FetchRequest{Hashes: hs})
	resp, err := c.do(ctx, http.MethodPost, "/api/v0/packs/fetch", bytes.NewReader(b), int64(len(b)), "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("blobs: fetch answered %d", resp.StatusCode)
	}
	if _, err := ReadPack(resp.Body, hs, len(hs), keep); err != nil {
		if errors.Is(err, ErrBadPack) || errors.Is(err, ErrBadHash) || errors.Is(err, ErrTooBig) {
			return err
		}
		return errors.New("blobs: the fetched pack could not be read")
	}
	return nil
}

// packSink takes many blobs a request (Client).
type packSink interface {
	PutPack(ctx context.Context, items []Item) error
}

// packSource sends many blobs a request (Client).
type packSource interface {
	GetPack(ctx context.Context, hs []string, keep func(h string, r io.Reader) error) error
}

// sizer knows each blob's size before it is read (Store).
type sizer interface {
	Size(h string) (int64, error)
}

// putPacks sends the blobs hs from src to dst: small ones PackBytes or
// PackCount to a pack, big ones alone, PackWorkers at a time.
func putPacks(ctx context.Context, src Source, sz sizer, dst interface {
	packSink
	Sink
}, hs []string) error {
	type sized struct {
		h    string
		size int64
	}
	var tasks []func(context.Context) error
	var pack []sized
	var bytes int64
	flush := func() {
		if len(pack) > 0 {
			p := pack
			// Each source opens with the task's own context, which ends
			// when another pack fails.
			tasks = append(tasks, func(ctx context.Context) error {
				items := make([]Item, len(p))
				for i, b := range p {
					items[i] = Item{Hash: b.h, Size: b.size, Open: func(ctx context.Context) (io.ReadCloser, error) { return src.Open(ctx, b.h) }}
				}
				return dst.PutPack(ctx, items)
			})
		}
		pack, bytes = nil, 0
	}
	for _, h := range hs {
		size, err := sz.Size(h)
		if err != nil {
			return fmt.Errorf("blobs: %s could not be read: %w", h, err)
		}
		if size >= PackAlone {
			tasks = append(tasks, func(ctx context.Context) error {
				rc, err := src.Open(ctx, h)
				if err != nil {
					return fmt.Errorf("blobs: %s could not be read: %w", h, err)
				}
				defer rc.Close()
				return dst.Put(ctx, h, rc)
			})
			continue
		}
		if len(pack) == PackCount || bytes+size > PackBytes {
			flush()
		}
		pack = append(pack, sized{h, size})
		bytes += size
	}
	flush()
	return parallel(ctx, PackWorkers, tasks)
}

// getPacks fetches the blobs hs from src into dst, PackCount to a pack,
// PackWorkers at a time.
func getPacks(ctx context.Context, src packSource, dst Sink, hs []string) error {
	var tasks []func(context.Context) error
	for i := 0; i < len(hs); i += PackCount {
		part := hs[i:min(len(hs), i+PackCount)]
		tasks = append(tasks, func(ctx context.Context) error {
			return src.GetPack(ctx, part, func(h string, r io.Reader) error { return dst.Put(ctx, h, r) })
		})
	}
	return parallel(ctx, PackWorkers, tasks)
}

// parallel runs tasks, n at a time, and answers the first error; the rest
// stop by their context once one fails.
func parallel(ctx context.Context, n int, tasks []func(context.Context) error) error {
	if len(tasks) == 1 {
		return tasks[0](ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	next := make(chan func(context.Context) error)
	for range min(n, len(tasks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range next {
				if err := t(ctx); err != nil {
					once.Do(func() { first = err; cancel() })
				}
			}
		}()
	}
	for _, t := range tasks {
		if ctx.Err() != nil {
			break
		}
		next <- t
	}
	close(next)
	wg.Wait()
	if first == nil {
		first = ctx.Err() // the caller's context ended
	}
	return first
}
