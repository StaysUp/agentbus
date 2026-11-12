package jobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/workspace"
)

// A runner reads the blobs of its own jobs, single and in packs, for a while
// after its attempt ends; another runner's job's blob, and a blob no job
// names, get 403 whether held or not.
func TestRunnerReadsOnlyItsOwnBlobs(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	q, err := queue.Open(filepath.Join(t.TempDir(), "jobs.db"), clock)
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
	put := func(b []byte) string {
		h, err := st.PutBytes(b)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	// Each runner's turn changes one file, under a tree of one more.
	type side struct {
		tok              string
		file, tree, cs   string
		inTree, fileName string
	}
	sides := map[string]*side{"a": {}, "b": {}}
	for name, sd := range sides {
		sd.tok = enroll(t, srv, 1).Token
		sd.file = put([]byte("changed by " + name))
		sd.inTree = put([]byte("in the tree of " + name))
		sd.tree = put(workspace.EncodeTree([]workspace.TreeEntry{{Name: "x.txt", Hash: sd.inTree, Mode: 0o644, Size: 1}}))
		b, _ := json.Marshal(workspace.Changeset{Space: "s_" + name, Checkpoint: "t", Snapshot: sd.tree, Files: []workspace.FileChange{
			{Path: "c.txt", Now: &workspace.File{Hash: sd.file, Size: 1, Mode: 0o644}}}})
		sd.cs = put(b)
		makeJob(t, q, queue.Spec{Harness: "claude", Prompt: "p", Session: "s_" + name})
		var taken TakenWork
		if code := send(t, srv, sd.tok, "POST", "/api/v0/runner/take", TakeRequest{Protocol: ProtocolVersion}, &taken); code != 200 || taken.Job == nil {
			t.Fatalf("take %s: %d", name, code)
		}
		base := "/api/v0/runner/attempts/" + taken.Attempt.ID
		if code := send(t, srv, sd.tok, "POST", base+"/start", AttemptCall{Number: 1}, nil); code != 200 {
			t.Fatalf("start %s: %d", name, code)
		}
		res, _ := json.Marshal(queue.Result{Changeset: sd.cs})
		if code := send(t, srv, sd.tok, "POST", base+"/finish", FinishCall{Number: 1, Status: queue.Finished, Result: res}, nil); code != 200 {
			t.Fatalf("finish %s: %d", name, code)
		}
	}
	nobody := put([]byte("no job names this"))
	a, b := sides["a"], sides["b"]
	get := func(tok, h string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+BlobPath+h, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	pack := func(tok string, hs ...string) int {
		t.Helper()
		return send(t, srv, tok, "POST", "/api/v0/runner/packs/fetch", blobs.FetchRequest{Hashes: hs}, nil)
	}
	for _, h := range []string{a.file, a.cs, a.tree, a.inTree} {
		if code := get(a.tok, h); code != 200 {
			t.Errorf("its own blob %s: %d", h, code)
		}
	}
	if code := pack(a.tok, a.file, a.cs, a.tree, a.inTree); code != 200 {
		t.Errorf("its own pack: %d", code)
	}
	ctx := context.Background()
	if err := (blobs.Local{Store: mustStore(t), From: blobs.Client{URL: srv.URL, Token: a.tok}}).Fetch(ctx, []string{a.file}); err != nil {
		t.Errorf("its own fetch: %v", err)
	}
	for _, h := range []string{b.file, b.cs, b.tree, b.inTree, nobody, "ab" + nobody[2:]} {
		if code := get(a.tok, h); code != 403 {
			t.Errorf("another's blob %s: %d, want 403", h, code)
		}
	}
	if code := pack(a.tok, a.file, b.file); code != 403 {
		t.Errorf("a pack with another's blob: %d, want 403", code)
	}
	if code := pack(b.tok, b.file, b.inTree); code != 200 {
		t.Errorf("b's own pack: %d", code)
	}
	// Long after the attempts ended, neither reads its old job's blobs.
	mu.Lock()
	now = now.Add(RecentRead + time.Minute)
	mu.Unlock()
	if code := get(a.tok, a.file); code != 403 {
		t.Errorf("an old job's blob: %d, want 403", code)
	}
	if code := pack(b.tok, b.file); code != 403 {
		t.Errorf("an old job's pack: %d, want 403", code)
	}
}

func mustStore(t *testing.T) *blobs.Store {
	t.Helper()
	st, err := blobs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}
