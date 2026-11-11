package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A folder job goes only to the connector it names. Take hands it the path;
// the stored job never holds it.
func TestFolderJob(t *testing.T) {
	q, clk := open(t)
	plain := capsRunner(t, q, nil, nil)
	connB := capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conB})
	connA := capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conA})
	if got, err := q.Connectors(DefaultTeam); err != nil || strings.Join(got, ",") != conA+","+conB {
		t.Fatalf("connectors %v %v", got, err)
	}
	j, err := q.RegisterFolder(DefaultTeam, conA, "/home/me/code")
	if err != nil || j.Kind != KindFolder || j.Spec.Folder != "" || j.Spec.Connector != conA || j.Turn != "" || j.Session != "" {
		t.Fatalf("folder job %+v %v", j, err)
	}
	for _, r := range []Runner{plain, connB} {
		if got := takes(t, q, r); got != "" {
			t.Fatalf("%s took the folder job", got)
		}
	}
	taken, _, ok, _, err := q.Take(connA.ID, lease)
	if err != nil || !ok || taken.ID != j.ID || taken.Spec.Folder != "/home/me/code" {
		t.Fatalf("the named connector took %+v %v %v", taken.Spec, ok, err)
	}
	a := mustAttempt(t, q, j.ID)
	if _, _, err := q.Start(connA.ID, a, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(connA.ID, a, 1, Finish{Status: Finished, Result: []byte(`{"folder_ref":"` + refX + `"}`)}); err != nil {
		t.Fatal(err)
	}
	got, err := q.Get(DefaultTeam, j.ID)
	if res, _ := ReadResult(got.Result); err != nil || got.Spec.Folder != "" || res.FolderRef != refX {
		t.Fatalf("after its end: %+v %s (%v)", got.Spec, got.Result, err)
	}
	if err := q.ForgetFolder(DefaultTeam, j.ID); err != nil {
		t.Fatal(err)
	}

	// One no connector takes: ForgetFolder ends it.
	w, _ := q.RegisterFolder(DefaultTeam, conB, "/home/me/other")
	if err := q.ForgetFolder(DefaultTeam, w.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Get(DefaultTeam, w.ID); got.Ended == nil || got.State != Cancelled {
		t.Fatalf("forgotten: %+v", got)
	}

	// A connector not seen within RunnerFresh is not listed.
	clk.add(RunnerFresh + time.Second)
	if _, _, _, _, err := q.Take(connB.ID, lease); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Connectors(DefaultTeam); strings.Join(got, ",") != conB {
		t.Fatalf("connectors after a minute: %v", got)
	}
}

// The path never reaches the queue's files, not even its journal, and a
// folder job past FolderWait ends.
func TestFolderPathNeverStored(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	q, err := Open(filepath.Join(dir, "jobs.db"), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	conn := capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: conA})
	const path = "/home/me/very-private-folder-name"
	lost, _ := q.RegisterFolder(DefaultTeam, conA, path)
	if j, _, ok, _, _ := q.Take(conn.ID, lease); !ok || j.Spec.Folder != path {
		t.Fatalf("take: %+v %v", j.Spec, ok)
	}
	waiting, _ := q.RegisterFolder(DefaultTeam, conB, path+"-2")
	files, _ := filepath.Glob(filepath.Join(dir, "jobs.db*"))
	for _, f := range files {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "very-private-folder-name") {
			t.Fatalf("%s holds the path", filepath.Base(f))
		}
	}
	if len(files) == 0 {
		t.Fatal("no queue file read")
	}

	// The lease runs out with no API left, as after a restart: both jobs
	// are past FolderWait, so the sweep ends them, and neither goes out again.
	clk.add(lease + time.Second)
	if _, err := q.Sweep(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{lost.ID, waiting.ID} {
		if got, _ := q.Get(DefaultTeam, id); got.Ended == nil {
			t.Errorf("job %s did not end: %+v", id, got)
		}
	}
	if _, _, ok, _, _ := q.Take(conn.ID, lease); ok {
		t.Fatal("an ended folder job was taken again")
	}
}
