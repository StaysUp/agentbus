package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// The pack routes take and give many blobs a request, on a runner's token
// only, each checked against its hash.
func TestBlobPacks(t *testing.T) {
	ctx := context.Background()
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fr, _ := testFolders(t)
	st, _ := blobs.Open(t.TempDir())
	s, err := New(Config{Queue: q, Token: mainTok, EnrollToken: enrollTok, MaxWait: 200 * time.Millisecond, Folders: fr, Blobs: st})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); q.Close() })
	e := enroll(t, srv, 1)
	c := blobs.Client{URL: srv.URL, Token: e.Token}

	from, _ := blobs.Open(t.TempDir())
	var hs []string
	for i := range 1500 {
		h, _ := from.PutBytes([]byte(fmt.Sprintf("file %d", i)))
		hs = append(hs, h)
	}
	if err := blobs.Copy(ctx, from, c, hs); err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if !st.Has(h) {
			t.Fatalf("%s not kept", h)
		}
	}
	// A runner reads only what its jobs name (blobscope.go): this one's
	// turn changed every file, and one more that is not held.
	gone := strings.Repeat("ab", 32)
	own, _ := blobs.Open(t.TempDir())
	if err := (blobs.Local{Store: own, From: c}).Fetch(ctx, hs[:1]); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a fetch with no job: %v", err)
	}
	cs := workspace.Changeset{Space: "s_1", Checkpoint: "t"}
	for i, h := range append(slices.Clone(hs), gone) {
		cs.Files = append(cs.Files, workspace.FileChange{Path: fmt.Sprintf("f%d", i), Now: &workspace.File{Hash: h, Size: 1, Mode: 0o644}})
	}
	csb, _ := json.Marshal(cs)
	csh, _ := st.PutBytes(csb)
	makeJob(t, q, queue.Spec{Harness: "claude", Prompt: "p", Session: "s_1"})
	var taken TakenWork
	if code := send(t, srv, e.Token, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 || taken.Job == nil {
		t.Fatalf("take: %d", code)
	}
	base := "/api/v0/runner/attempts/" + taken.Attempt.ID
	send(t, srv, e.Token, "POST", base+"/start", AttemptCall{Number: 1}, nil)
	res, _ := json.Marshal(queue.Result{Changeset: csh})
	if code := send(t, srv, e.Token, "POST", base+"/finish", FinishCall{Number: 1, Status: queue.Finished, Result: res}, nil); code != 200 {
		t.Fatalf("finish: %d", code)
	}
	if err := (blobs.Local{Store: own, From: c}).Fetch(ctx, hs); err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if !own.Has(h) {
			t.Fatalf("%s not fetched", h)
		}
	}
	// Named but not held: 404, the missing named.
	if err := c.GetPack(ctx, []string{hs[0], gone}, func(string, io.Reader) error { return nil }); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("a pack with a blob not held: %v", err)
	}
	// A blob that is not its hash: 400, and it is not kept.
	bad := hs[0][:63] + "0"
	if hs[0][63] == '0' {
		bad = hs[0][:63] + "1"
	}
	req, _ := http.NewRequest("POST", srv.URL+PackPath, strings.NewReader(bad+" 6\nfile 0"))
	req.Header.Set("Authorization", "Bearer "+e.Token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 400 || st.Has(bad) {
		t.Fatalf("a blob under another's hash: %v %v", resp, err)
	}
	// The main token is not a runner's.
	if code := send(t, srv, mainTok, "POST", "/api/v0/runner/packs/fetch", blobs.FetchRequest{Hashes: hs[:1]}, nil); code != 401 && code != 403 {
		t.Fatalf("the main token fetched blobs: %d", code)
	}
}
