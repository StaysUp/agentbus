package queue

import (
	"encoding/json"
	"strings"
	"testing"
)

func walls(levels ...string) *Capabilities {
	return &Capabilities{Wall: &WallReport{Engine: "srt", Version: "0.0.83", Levels: levels}}
}

// A walled job goes to a runner whose wall probe lists its level. When no
// fresh runner lists it, a runner that reported a probe takes it, to refuse
// it with the probe's reason. A runner that reported no probe (an older
// build, or one not probed yet) never takes it, and a job with no wall goes
// to any runner as before.
func TestTakeRoutesByWall(t *testing.T) {
	q, c := open(t)
	old := capsRunner(t, q, nil, &Capabilities{OS: "darwin"})
	none := capsRunner(t, q, nil, &Capabilities{Wall: &WallReport{Engine: "srt", Problem: "srt is not installed"}})
	built := capsRunner(t, q, nil, walls("copy"))

	j := makeJob(t, q, Spec{Harness: "claude", Folder: "/x", Wall: "copy"})
	if got := takes(t, q, old); got != "" {
		t.Fatalf("a runner that reported no wall took walled job %s", got)
	}
	if got := takes(t, q, none); got != "" {
		t.Fatalf("a runner with no copy wall took %s while another lists it", got)
	}
	if got := takes(t, q, built); got != j.ID {
		t.Fatalf("the runner with the copy wall took %q, want %s", got, j.ID)
	}

	// A level no runner lists: the runner that reported a probe takes it, to
	// refuse it; the older runner still does not.
	j2 := makeJob(t, q, Spec{Harness: "claude", Folder: "/x", Wall: "sandbox"})
	if got := takes(t, q, old); got != "" {
		t.Fatalf("a runner that reported no wall took %s", got)
	}
	if got := takes(t, q, none); got != j2.ID {
		t.Fatalf("with no runner listing the level, %q was taken, want %s", got, j2.ID)
	}

	// A runner that listed the level but was not seen lately holds nothing back.
	stale := capsRunner(t, q, nil, walls("copy", "sandbox"))
	if _, err := q.db.Exec(`UPDATE runners SET seen = ? WHERE id = ?`, nanos(c.now().Add(-2*RunnerFresh)), stale.ID); err != nil {
		t.Fatal(err)
	}
	j3 := makeJob(t, q, Spec{Harness: "claude", Folder: "/x", Wall: "sandbox"})
	if got := takes(t, q, none); got != j3.ID {
		t.Fatalf("a stale runner's wall held the job back: %q", got)
	}

	// A job with no wall goes to the runner that reported none.
	j4 := makeJob(t, q, Spec{Harness: "claude", Folder: "/x"})
	if got := takes(t, q, old); got != j4.ID {
		t.Fatalf("a job with no wall: %q, want %s", got, j4.ID)
	}
}

// A runner listing the level on another machine, or without the job's
// labels, does not hold the job back from the job's own runner, which then
// takes it to refuse it.
func TestWallRuleCountsOnlyEligibleRunners(t *testing.T) {
	q, _ := open(t)
	owner := capsRunner(t, q, nil, &Capabilities{Connector: conA, Wall: &WallReport{Engine: "srt", Problem: "x"}})
	capsRunner(t, q, nil, &Capabilities{Connector: conB, Wall: &WallReport{Engine: "srt", Levels: []string{"copy"}}})
	j := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA, Wall: "copy"})
	if got := takes(t, q, owner); got != j.ID {
		t.Fatalf("the folder's runner took %q, want %s", got, j.ID)
	}
	plain := capsRunner(t, q, []string{"cuda"}, &Capabilities{Wall: &WallReport{Engine: "srt", Problem: "x"}})
	capsRunner(t, q, nil, walls("copy"))
	j2 := makeJob(t, q, Spec{Harness: "claude", Folder: "/x", Labels: []string{"cuda"}, Wall: "copy"})
	if got := takes(t, q, plain); got != j2.ID {
		t.Fatalf("the only runner with the label took %q, want %s", got, j2.ID)
	}
}

// The wall report is bounded, and a spec's wall survives the wire.
func TestWallReportWire(t *testing.T) {
	if msg := (Capabilities{Wall: &WallReport{Engine: "srt", Problem: strings.Repeat("x", 513)}}).Check(); msg == "" {
		t.Fatal("a 513-byte problem passed the check")
	}
	if msg := (Capabilities{Wall: &WallReport{Engine: "srt", Levels: []string{strings.Repeat("x", 33)}}}).Check(); msg == "" {
		t.Fatal("a 33-byte level passed the check")
	}
	if msg := walls("copy").Check(); msg != "" {
		t.Fatalf("a plain report: %s", msg)
	}
	b, err := json.Marshal(Spec{Prompt: "p", Wall: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	var back Spec
	if err := json.Unmarshal(b, &back); err != nil || back.Wall != "copy" {
		t.Fatalf("spec wire: %s -> %+v %v", b, back, err)
	}
}
