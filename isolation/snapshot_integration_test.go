//go:build integration

package isolation

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/blobs"
	"github.com/staysup/agentbus/connector"
	"github.com/staysup/agentbus/folders"
)

// slowBlobs reads each blob after a pause, as a store on another machine does.
type slowBlobs struct{ BlobSource }

func (s slowBlobs) Open(ctx context.Context, h string) (io.ReadCloser, error) {
	time.Sleep(50 * time.Millisecond)
	return s.BlobSource.Open(ctx, h)
}

// The snapshot a copy is built from is taken when its last file is written,
// not when it began: a file's change time is after its start, and a
// checkpoint trusts a hash only for a file changed before the snapshot.
func TestMaterialiseIsTakenAfterItsFilesAreWritten(t *testing.T) {
	src := t.TempDir()
	files := map[string]string{}
	for i := range 8 {
		files[fmt.Sprintf("f%d", i)] = fmt.Sprint(i)
	}
	fixture(t, src, files)
	dir := t.TempDir()
	reg, _ := folders.Open(filepath.Join(dir, "folders.json"))
	ref, _ := reg.Register(src)
	st, _ := blobs.Open(filepath.Join(dir, "blobs"))
	conn := &connector.Connector{Folders: reg, Store: st, State: filepath.Join(dir, "state")}
	taken, err := conn.Snapshot(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := materialise(context.Background(), &Snapshot{ID: taken.ID, Blobs: slowBlobs{st}, Times: taken.Times},
		filepath.Join(t.TempDir(), "copy"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if late := time.Since(time.Unix(0, snap.Taken())); late > 200*time.Millisecond {
		t.Errorf("the snapshot was taken %v before the copy was done", late)
	}
}
