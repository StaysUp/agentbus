package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A folder job goes only to the runner of the machine it names, in a folder
// place. Take hands it the path; the stored job never holds it.
func TestFolderJob(t *testing.T) {
	q, clk := open(t)
	plain := capsRunner(t, q, nil, nil)
	connB := capsRunner(t, q, nil, &Capabilities{Connector: conB})
	connA := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	capsRunner(t, q, nil, &Capabilities{Role: RoleConnector, Connector: "con_cccccccccccccccc"})
	if got, err := q.Machines(DefaultTeam); err != nil || strings.Join(got, ",") != conA+","+conB {
		t.Fatalf("machines %v %v", got, err)
	}
	j, err := q.RegisterFolder(DefaultTeam, conA, "/home/me/code")
	if err != nil || j.Kind != KindFolder || j.Spec.Folder != "" || j.Spec.Machine != conA || j.Turn != "" || j.Session != "" {
		t.Fatalf("folder job %+v %v", j, err)
	}
	for _, r := range []Runner{plain, connB} {
		if got := takesFolder(t, q, r); got != "" {
			t.Fatalf("%s took the folder job", got)
		}
	}
	if got := takes(t, q, connA); got != "" {
		t.Fatalf("a turn place took the folder job %s", got)
	}
	taken, _, ok, _, err := q.Take(connA.ID, lease, LaneFolders)
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
	if _, _, _, _, err := q.Take(connB.ID, lease, LaneFolders); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Machines(DefaultTeam); strings.Join(got, ",") != conB {
		t.Fatalf("machines after a minute: %v", got)
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
	conn := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	const path = "/home/me/very-private-folder-name"
	lost, _ := q.RegisterFolder(DefaultTeam, conA, path)
	if j, _, ok, _, _ := q.Take(conn.ID, lease, LaneFolders); !ok || j.Spec.Folder != path {
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
	if _, _, ok, _, _ := q.Take(conn.ID, lease, LaneFolders); ok {
		t.Fatal("an ended folder job was taken again")
	}
}

// A home job goes only to the runner of the machine it names, in a folder
// place, never a turn place. DropHome removes it once ended, and the next
// AskHome removes any an earlier caller left.
func TestHomeJob(t *testing.T) {
	q, _ := open(t)
	connB := capsRunner(t, q, nil, &Capabilities{Connector: conB})
	connA := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	j, err := q.AskHome(DefaultTeam, conA)
	if err != nil || j.Kind != KindHome || j.Spec.Machine != conA || j.Session != "" {
		t.Fatalf("home job %+v %v", j, err)
	}
	if got := takesFolder(t, q, connB); got != "" {
		t.Fatalf("another machine took the home job: %s", got)
	}
	if got := takes(t, q, connA); got != "" {
		t.Fatalf("a turn place took the home job: %s", got)
	}
	if got := takesFolder(t, q, connA); got != j.ID {
		t.Fatalf("the machine took %q, want %s", got, j.ID)
	}
	a := mustAttempt(t, q, j.ID)
	if _, _, err := q.Start(connA.ID, a, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(connA.ID, a, 1, Finish{Status: Finished, Result: []byte(`{"home":[{"path":"~/.gitconfig","kind":"file","size":7}]}`)}); err != nil {
		t.Fatal(err)
	}
	got, err := q.Get(DefaultTeam, j.ID)
	if res, _ := ReadResult(got.Result); err != nil || len(res.Home) != 1 || res.Home[0].Size != 7 {
		t.Fatalf("after its end: %s %v", got.Result, err)
	}
	if err := q.DropHome(DefaultTeam, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Get(DefaultTeam, j.ID); err == nil {
		t.Fatal("a read home job is kept")
	}
	// One its caller left: cancelled, then gone at the next ask.
	left, _ := q.AskHome(DefaultTeam, conB)
	if _, err := q.Cancel(DefaultTeam, left.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.AskHome(DefaultTeam, conB); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Get(DefaultTeam, left.ID); err == nil {
		t.Fatal("a home job left ended is kept")
	}
}

// Two callers ask at once: the second ask never drops the first's answer,
// ended and not yet read (review of 2026-11-14, P1). Only a job its caller
// gave up on goes.
func TestHomeJobConcurrentAsks(t *testing.T) {
	q, _ := open(t)
	connA := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	first, err := q.AskHome(DefaultTeam, conA)
	if err != nil {
		t.Fatal(err)
	}
	if got := takesFolder(t, q, connA); got != first.ID {
		t.Fatalf("took %q, want %s", got, first.ID)
	}
	a := mustAttempt(t, q, first.ID)
	if _, _, err := q.Start(connA.ID, a, 1, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := q.End(connA.ID, a, 1, Finish{Status: Finished, Result: []byte(`{"home":[{"path":"~/.gitconfig","kind":"file","size":7}]}`)}); err != nil {
		t.Fatal(err)
	}
	// The second caller asks before the first reads.
	second, err := q.AskHome(DefaultTeam, conA)
	if err != nil {
		t.Fatal(err)
	}
	got, err := q.Get(DefaultTeam, first.ID)
	if res, _ := ReadResult(got.Result); err != nil || len(res.Home) != 1 {
		t.Fatalf("the first caller's answer: %s %v", got.Result, err)
	}
	if err := q.DropHome(DefaultTeam, first.ID); err != nil {
		t.Fatal(err)
	}
	// The second caller gives up; the next ask drops its job.
	if _, err := q.Cancel(DefaultTeam, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.AskHome(DefaultTeam, conA); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Get(DefaultTeam, second.ID); err == nil {
		t.Fatal("a home job its caller gave up on is kept")
	}
}
