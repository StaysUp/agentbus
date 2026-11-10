// Package blobs keeps file contents by their SHA-256: a content-addressed
// store on disk, and a client for the control plane's copy of one. A blob is
// named by the hex SHA-256 of its bytes, so the same content has the same
// name on every machine, and a blob already held is never sent again.
//
// Three roles use it. The control plane keeps the store every file travels
// through (GET, PUT and missing, in package jobs). A connector puts the
// files of a folder's snapshot there, and reads the files a turn changed
// from it. A runner reads a snapshot's files from it to build a copy, and
// puts the files its turns changed. File data never goes from a connector
// to a runner directly. On one machine (agentbus up) the three share one
// Store on disk and nothing is sent over HTTP.
//
// It imports nothing of the module.
package blobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"
)

// MaxBlob bounds one blob: a file bigger than this is not sent.
const MaxBlob = 4 << 30

// hashShape is a blob's name.
var hashShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Valid says h is a blob's name: 64 lowercase hex digits.
func Valid(h string) bool { return hashShape.MatchString(h) }

// Errors of this package, in fixed wording.
var (
	ErrNotFound = errors.New("blobs: no blob of that hash")
	ErrBadHash  = errors.New("blobs: the content does not have the hash it was sent with")
	ErrTooBig   = errors.New("blobs: the blob is bigger than a blob may be")
)

// Source reads blobs.
type Source interface {
	// Open reads the blob h. ErrNotFound when it is not held.
	Open(ctx context.Context, h string) (io.ReadCloser, error)
}

// Sink keeps blobs.
type Sink interface {
	// Missing are the hashes of hs not held, in hs's order.
	Missing(ctx context.Context, hs []string) ([]string, error)
	// Put keeps the content r reads as the blob h, checked against h.
	Put(ctx context.Context, h string, r io.Reader) error
}

// Store is blobs on disk, one file each at <dir>/<first two hex>/<hash>,
// written once by rename and never changed. It is safe for concurrent use,
// by this process and by others on the same folder.
type Store struct{ dir string }

// Open is the store in dir, made when it does not exist.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("blobs: the store's folder could not be made")
	}
	return &Store{dir: dir}, nil
}

// Dir is the store's folder.
func (s *Store) Dir() string { return s.dir }

// Path is where the blob h is, held or not. h must be Valid.
func (s *Store) Path(h string) string { return filepath.Join(s.dir, h[:2], h) }

// Has says the blob h is held.
func (s *Store) Has(h string) bool {
	if !Valid(h) {
		return false
	}
	fi, err := os.Lstat(s.Path(h))
	return err == nil && fi.Mode().IsRegular()
}

// Missing are the hashes of hs this store does not hold.
func (s *Store) Missing(_ context.Context, hs []string) ([]string, error) {
	var out []string
	for _, h := range hs {
		if !Valid(h) {
			return nil, ErrBadHash
		}
		if !s.Has(h) {
			out = append(out, h)
		}
	}
	return out, nil
}

// Open reads the blob h.
func (s *Store) Open(_ context.Context, h string) (io.ReadCloser, error) {
	if !Valid(h) {
		return nil, ErrNotFound
	}
	f, err := os.Open(s.Path(h))
	if err != nil {
		return nil, ErrNotFound
	}
	return f, nil
}

// Put writes what r reads as the blob h: to a new file, hashed as it is
// written, and named h only when the hash matches. A blob already held is
// read to its end and kept as it was. An empty h takes whatever hash the
// content has.
func (s *Store) Put(_ context.Context, h string, r io.Reader) error {
	_, err := s.put(h, r)
	return err
}

// PutReader is Put that answers the hash.
func (s *Store) PutReader(r io.Reader) (string, error) { return s.put("", r) }

// PutBytes keeps b and answers its hash.
func (s *Store) PutBytes(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	if s.Has(h) {
		return h, nil
	}
	_, err := s.put(h, bytes.NewReader(b))
	return h, err
}

func (s *Store) put(want string, r io.Reader) (string, error) {
	if want != "" && !Valid(want) {
		return "", ErrBadHash
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "tmp"), 0o700); err != nil {
		return "", errors.New("blobs: the store's folder could not be written")
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "put-")
	if err != nil {
		return "", errors.New("blobs: the store's folder could not be written")
	}
	defer os.Remove(tmp.Name())
	hw := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hw), io.LimitReader(r, MaxBlob+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("blobs: the blob could not be written: %w", err)
	}
	if n > MaxBlob {
		return "", ErrTooBig
	}
	got := hex.EncodeToString(hw.Sum(nil))
	if want != "" && got != want {
		return "", ErrBadHash
	}
	return got, s.place(tmp.Name(), got)
}

// PutFile keeps the file at p as a blob, by link where link makes one (a
// clone: no bytes copied), and answers its hash. hash, when not empty, is
// the file's hash as the caller took it: a blob already held under it is
// taken as the file, and a new one is checked against it. link may be nil.
func (s *Store) PutFile(p, hash string, link func(src, dst string) error) (string, error) {
	if hash != "" && s.Has(hash) {
		return hash, nil
	}
	if link != nil {
		if err := os.MkdirAll(filepath.Join(s.dir, "tmp"), 0o700); err == nil {
			tmp := filepath.Join(s.dir, "tmp", "link-"+strconv.FormatInt(time.Now().UnixNano(), 36)+"-"+strconv.FormatInt(seq.Add(1), 36))
			if link(p, tmp) == nil {
				defer os.Remove(tmp)
				got, err := hashFile(tmp)
				if err == nil && (hash == "" || got == hash) {
					return got, s.place(tmp, got)
				}
				if err == nil {
					return "", ErrBadHash
				}
			}
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return s.put(hash, f)
}

var seq atomic.Int64

// place renames the written file tmp to the blob h, unless h is held.
func (s *Store) place(tmp, h string) error {
	dst := s.Path(h)
	if s.Has(h) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return errors.New("blobs: the store's folder could not be written")
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, dst); err != nil {
		if s.Has(h) {
			return nil
		}
		return errors.New("blobs: the blob could not be kept")
	}
	return nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Copy puts every blob of hs that dst lacks there, read from src.
func Copy(ctx context.Context, src Source, dst Sink, hs []string) error {
	missing, err := missingBatched(ctx, dst, hs)
	if err != nil {
		return err
	}
	for _, h := range missing {
		if err := ctx.Err(); err != nil {
			return err
		}
		rc, err := src.Open(ctx, h)
		if err != nil {
			return fmt.Errorf("blobs: %s could not be read: %w", h, err)
		}
		err = dst.Put(ctx, h, rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// MaxAsk bounds the hashes one missing call asks about.
const MaxAsk = 10000

// MissingAll asks dst which of hs it lacks, MaxAsk at a time, each once.
func MissingAll(ctx context.Context, dst Sink, hs []string) ([]string, error) {
	return missingBatched(ctx, dst, hs)
}

// missingBatched asks dst about hs MaxAsk at a time, once each.
func missingBatched(ctx context.Context, dst Sink, hs []string) ([]string, error) {
	seen := map[string]bool{}
	var uniq []string
	for _, h := range hs {
		if !seen[h] {
			seen[h] = true
			uniq = append(uniq, h)
		}
	}
	var out []string
	for i := 0; i < len(uniq); i += MaxAsk {
		m, err := dst.Missing(ctx, uniq[i:min(len(uniq), i+MaxAsk)])
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	return out, nil
}

// ReadAll reads the blob h from src, at most max bytes.
func ReadAll(ctx context.Context, src Source, h string, max int64) ([]byte, error) {
	rc, err := src.Open(ctx, h)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrTooBig
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != h {
		return nil, ErrBadHash
	}
	return b, nil
}

// Clean removes what an interrupted Put left in the store, older than age.
func (s *Store) Clean(age time.Duration) {
	dir := filepath.Join(s.dir, "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > age {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Local is a Store with a Source behind it: a blob it lacks is fetched
// from there and kept. A runner's own store is one, with the control
// plane's store behind it.
type Local struct {
	*Store
	From Source
}

// Open reads the blob h, fetching it first when it is not held.
func (l Local) Open(ctx context.Context, h string) (io.ReadCloser, error) {
	if rc, err := l.Store.Open(ctx, h); err == nil {
		return rc, nil
	}
	if l.From == nil {
		return nil, ErrNotFound
	}
	if err := l.Fetch(ctx, []string{h}); err != nil {
		return nil, err
	}
	return l.Store.Open(ctx, h)
}

// Fetch makes every blob of hs held, from From.
func (l Local) Fetch(ctx context.Context, hs []string) error {
	if l.From == nil {
		for _, h := range hs {
			if !l.Has(h) {
				return ErrNotFound
			}
		}
		return nil
	}
	return Copy(ctx, l.From, l.Store, hs)
}
