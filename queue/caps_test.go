package queue

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/harness"
)

func capsRunner(t *testing.T, q *Queue, labels []string, caps *Capabilities) Runner {
	t.Helper()
	r, err := q.RegisterRunner(DefaultTeam, "r", labels, 4, HashToken(NewID("tok_")), caps)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func makeJob(t *testing.T, q *Queue, sp Spec) Job {
	t.Helper()
	sp.Prompt = "hi"
	j, _, err := q.Create(DefaultTeam, KindTurn, "", sp, Links{})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func takes(t *testing.T, q *Queue, r Runner) string {
	t.Helper()
	j, _, ok, _, err := q.Take(r.ID, lease)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return ""
	}
	return j.ID
}

const (
	conA = "con_aaaaaaaaaaaaaaaa"
	conB = "con_bbbbbbbbbbbbbbbb"
	refX = "fld_0123456789abcdef"
)

// A job that names a folder goes only to the runner of that folder's
// connector; a runner of none, or of another, never takes it.
func TestTakeRoutesByConnector(t *testing.T) {
	q, _ := open(t)
	none := capsRunner(t, q, nil, nil)
	other := capsRunner(t, q, nil, &Capabilities{Connector: conB})
	owner := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	j := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Connector: conA})
	if got := takes(t, q, none); got != "" {
		t.Fatalf("a runner with no connector took %s", got)
	}
	if got := takes(t, q, other); got != "" {
		t.Fatalf("another connector's runner took %s", got)
	}
	if got := takes(t, q, owner); got != j.ID {
		t.Fatalf("the owner took %q, want %s", got, j.ID)
	}
}

// A job's labels must all be the runner's.
func TestTakeRoutesByLabels(t *testing.T) {
	q, _ := open(t)
	plain := capsRunner(t, q, []string{"agentbus"}, nil)
	gpu := capsRunner(t, q, []string{"agentbus", "gpu"}, nil)
	j := makeJob(t, q, Spec{Harness: "claude", Folder: "/x", Labels: []string{"gpu"}})
	if got := takes(t, q, plain); got != "" {
		t.Fatalf("a runner without the label took %s", got)
	}
	if got := takes(t, q, gpu); got != j.ID {
		t.Fatalf("the gpu runner took %q", got)
	}
}

func found(names ...string) []harness.Report {
	var out []harness.Report
	for _, n := range []string{"claude", "codex"} {
		r := harness.Report{Name: n}
		for _, f := range names {
			if f == n {
				r.Found = true
			}
		}
		out = append(out, r)
	}
	return out
}

// A first job on a harness goes to a runner that has it. When no runner
// seen lately has it, any runner takes it, so its engine says how to install
// the program; a runner that has not probed yet takes any.
func TestTakeRoutesByHarness(t *testing.T) {
	q, c := open(t)
	noCodex := capsRunner(t, q, nil, &Capabilities{Harnesses: found("claude")})
	both := capsRunner(t, q, nil, &Capabilities{Harnesses: found("claude", "codex")})
	j := makeJob(t, q, Spec{Harness: "codex", Folder: "/x"})
	if got := takes(t, q, noCodex); got != "" {
		t.Fatalf("a runner without codex took %s while another has it", got)
	}
	if got := takes(t, q, both); got != j.ID {
		t.Fatalf("the runner with codex took %q", got)
	}

	// Now no runner has codex: the one that had it reports it gone.
	if _, err := q.RunnerBeat(both.ID, 4, nil, &Capabilities{Harnesses: found("claude")}); err != nil {
		t.Fatal(err)
	}
	j2 := makeJob(t, q, Spec{Harness: "codex", Folder: "/x"})
	if got := takes(t, q, noCodex); got != j2.ID {
		t.Fatalf("with no runner that has codex, %q was taken, want %s", got, j2.ID)
	}

	// A runner that reported codex but was not seen within RunnerFresh does
	// not hold jobs back from the others.
	stale := capsRunner(t, q, nil, &Capabilities{Harnesses: found("codex")})
	if _, err := q.db.Exec(`UPDATE runners SET seen = ? WHERE id = ?`, nanos(c.now().Add(-2*RunnerFresh)), stale.ID); err != nil {
		t.Fatal(err)
	}
	j3 := makeJob(t, q, Spec{Harness: "codex", Folder: "/x"})
	if got := takes(t, q, noCodex); got != j3.ID {
		t.Fatalf("a stale runner's report held the job back: %q", got)
	}

	// A runner that has not reported its harnesses takes any.
	fresh := capsRunner(t, q, nil, &Capabilities{OS: "linux"})
	j4 := makeJob(t, q, Spec{Harness: "claude", Folder: "/x"})
	if got := takes(t, q, fresh); got != j4.ID {
		t.Fatalf("an unprobed runner took %q", got)
	}
}

// A runner's report is checked: ids shaped as issued, and bounded lists.
func TestCapabilitiesCheck(t *testing.T) {
	for _, c := range []Capabilities{
		{Connector: "con_x"},
		{Folders: []FolderInfo{{Ref: refX, Name: "a"}}},
		{Connector: conA, Folders: []FolderInfo{{Ref: "/abs/path", Name: "a"}}},
		{Connector: conA, Folders: []FolderInfo{{Ref: refX, Name: strings.Repeat("a", 256)}}},
	} {
		if c.Check() == "" {
			t.Errorf("accepted %+v", c)
		}
	}
	if msg := (Capabilities{Connector: conA, Folders: []FolderInfo{{Ref: refX, Name: "a"}}}).Check(); msg != "" {
		t.Fatal(msg)
	}
}

// Folders lists what runners report, once per reference.
func TestFoldersListed(t *testing.T) {
	q, _ := open(t)
	capsRunner(t, q, nil, &Capabilities{Connector: conA, Folders: []FolderInfo{{Ref: refX, Name: "proj"}}})
	capsRunner(t, q, nil, nil)
	list, err := q.Folders(DefaultTeam)
	if err != nil || len(list) != 1 || list[0].Ref != refX || list[0].Connector != conA || list[0].Name != "proj" {
		t.Fatalf("%+v %v", list, err)
	}
}

type fakeIssuer map[string]string

func (f fakeIssuer) Connector() string            { return conA }
func (f fakeIssuer) Add(p string) (string, error) { return f.Register(p) }
func (f fakeIssuer) Register(p string) (string, error) {
	ref := "fld_" + strings.Repeat("0", 15) + string(rune('0'+len(f)))
	for k, v := range f {
		if v == p {
			return k, nil
		}
	}
	f[ref] = p
	return ref, nil
}
func (f fakeIssuer) Path(ref string) (string, bool) { p, ok := f[ref]; return p, ok }
func (f fakeIssuer) Within(p, ref string) bool {
	d, ok := f[ref]
	return ok && (p == d || strings.HasPrefix(p, d+"/"))
}
func (f fakeIssuer) WithinRef(child, ref string) bool {
	p, ok := f[child]
	return ok && f.Within(p, ref)
}

// A path becomes a reference and goes; a server with no connector refuses a
// path; a reference without a connector takes the issuer's.
func TestResolveFolders(t *testing.T) {
	f := fakeIssuer{}
	sp := Spec{Folder: "/a/b", Repo: ""}
	if msg := ResolveFolders(&sp, f); msg != "" || sp.Folder != "" || sp.FolderRef == "" || sp.Connector != conA || f[sp.FolderRef] != "/a/b" {
		t.Fatalf("%q %+v", msg, sp)
	}
	if msg := ResolveFolders(&Spec{Folder: "/a"}, nil); msg == "" {
		t.Fatal("a server took a path")
	}
	sp = Spec{FolderRef: refX}
	if msg := ResolveFolders(&sp, nil); msg == "" {
		t.Fatal("a reference with no connector was taken by a server")
	}
	if msg := ResolveFolders(&Spec{Folder: "/a", FolderRef: refX}, f); msg == "" {
		t.Fatal("both a path and a reference")
	}
	if msg := ResolveFolders(&Spec{FolderRef: "/a", Connector: conA}, nil); msg == "" {
		t.Fatal("a path as a reference")
	}
}

// A queue from before references: its jobs' paths become references, and
// its tables get the new columns.
func TestMigrateFoldersAndColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.db")
	q, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := q.Create(DefaultTeam, KindTurn, "", Spec{Harness: "claude", Folder: "/p/q", Prompt: "hi"}, Links{})
	if err != nil {
		t.Fatal(err)
	}
	// The columns as an earlier build made them: none.
	for _, s := range []string{`ALTER TABLE runners DROP COLUMN caps`, `ALTER TABLE sessions DROP COLUMN snapshot`} {
		if _, err := q.db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	q.Close()
	if q, err = Open(path, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	for _, c := range []struct{ table, col string }{{"runners", "caps"}, {"sessions", "snapshot"}} {
		var n int
		if err := q.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.col).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s.%s: %d %v", c.table, c.col, n, err)
		}
	}
	f := fakeIssuer{}
	if n, err := q.MigrateFolders(f); err != nil || n != 1 {
		t.Fatalf("migrated %d %v", n, err)
	}
	got := state(t, q, j.ID)
	if got.Spec.Folder != "" || got.Spec.FolderRef == "" || got.Spec.Connector != conA || f[got.Spec.FolderRef] != "/p/q" {
		t.Fatalf("%+v", got.Spec)
	}
	var raw sql.NullString
	if err := q.db.QueryRow(`SELECT spec FROM jobs WHERE id = ?`, j.ID).Scan(&raw); err != nil || strings.Contains(raw.String, "/p/q") {
		t.Fatalf("the path is still stored: %s %v", raw.String, err)
	}
	if n, _ := q.MigrateFolders(f); n != 0 {
		t.Fatalf("a second run rewrote %d", n)
	}
}

// A queue made before jobs.turn gets it filled: the id of the attempt that
// started the turn, even after its lease ran out and expiry cleared
// jobs.attempt, so Lost still finds that turn.
func TestFillTurnFromStartedAttempt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.db")
	q, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := runner(t, q, 5)
	j := job(t, q, "")
	_, a := take(t, q, r)
	if _, _, err := q.Start(r.ID, a.ID, a.Number, lease); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.AddEvents(r.ID, a.ID, a.Number, 1, []AttemptEvent{{Seq: 1, Time: time.Now(), Kind: "turn.created",
		Data: json.RawMessage(`{"session":"` + j.Session + `","turn":"` + a.ID + `"}`)}}); err != nil {
		t.Fatal(err)
	}
	queued := job(t, q, "")
	// As an earlier build left it: the lease ran out, expiry cleared
	// jobs.attempt, and there is no turn column.
	for _, s := range []string{`UPDATE attempts SET state = 'lost'`, `UPDATE jobs SET attempt = NULL, state = 'failed', ended = 1 WHERE id = '` + j.ID + `'`,
		`DROP INDEX jobs_turn`, `ALTER TABLE jobs DROP COLUMN turn`} {
		if _, err := q.db.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	q.Close()
	if q, err = Open(path, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	if got := state(t, q, j.ID); got.Turn != a.ID {
		t.Fatalf("turn %q, want the started attempt %s", got.Turn, a.ID)
	}
	if got := state(t, q, queued.ID); got.Turn == "" || got.Turn == a.ID {
		t.Fatalf("the queued job's turn %q", got.Turn)
	}
	if lost, err := q.Lost([]string{a.ID}); err != nil || !lost[a.ID] {
		t.Fatalf("Lost after the fill: %v %v", lost, err)
	}
}

// The harness rule counts only runners that could take the job: a runner of
// another connector that has the harness does not hold back the folder's
// own runner, whose engine then says how to install it.
func TestHarnessRuleCountsOnlyEligibleRunners(t *testing.T) {
	q, _ := open(t)
	owner := capsRunner(t, q, nil, &Capabilities{Connector: conA, Harnesses: found("claude")})
	capsRunner(t, q, []string{"gpu"}, &Capabilities{Connector: conB, Harnesses: found("claude", "codex")})
	j := makeJob(t, q, Spec{Harness: "codex", FolderRef: refX, Connector: conA})
	if got := takes(t, q, owner); got != j.ID {
		t.Fatalf("the folder's runner took %q, want %s", got, j.ID)
	}
	// Labels count the same way: a runner without the job's label does not
	// count as having the harness.
	plain := capsRunner(t, q, []string{"cuda"}, &Capabilities{Harnesses: found("claude")})
	capsRunner(t, q, nil, &Capabilities{Harnesses: found("codex")})
	j2 := makeJob(t, q, Spec{Harness: "codex", Folder: "/x", Labels: []string{"cuda"}})
	if got := takes(t, q, plain); got != j2.ID {
		t.Fatalf("the only runner with the label took %q, want %s", got, j2.ID)
	}
}

// Equal paths on two connectors are two folders.
func TestFoldersOfTwoConnectors(t *testing.T) {
	q, _ := open(t)
	capsRunner(t, q, nil, &Capabilities{Connector: conA, Folders: []FolderInfo{{Ref: refX, Name: "p"}}})
	capsRunner(t, q, nil, &Capabilities{Connector: conB, Folders: []FolderInfo{{Ref: refX, Name: "p"}}})
	if list, err := q.Folders(DefaultTeam); err != nil || len(list) != 2 || list[0].Connector == list[1].Connector {
		t.Fatalf("%+v %v", list, err)
	}
}

// A heartbeat's labels replace the runner's, as after a restart with others.
func TestHeartbeatReplacesLabels(t *testing.T) {
	q, _ := open(t)
	r := capsRunner(t, q, []string{"agentbus"}, nil)
	got, err := q.RunnerBeat(r.ID, 4, []string{"agentbus", "gpu"}, nil)
	if err != nil || len(got.Labels) != 2 || got.Labels[1] != "gpu" {
		t.Fatalf("%+v %v", got.Labels, err)
	}
	if got, _ := q.RunnerBeat(r.ID, 4, nil, nil); len(got.Labels) != 2 {
		t.Fatalf("a heartbeat without labels changed them: %+v", got.Labels)
	}
}

func (f fakeIssuer) WithinDir(p, d string) bool { return p == d || strings.HasPrefix(p, d+"/") }
