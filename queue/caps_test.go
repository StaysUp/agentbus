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
	return takesKind(t, q, r, false)
}

// takesFolder is takes of a folder job.
func takesFolder(t *testing.T, q *Queue, r Runner) string {
	t.Helper()
	return takesKind(t, q, r, true)
}

func takesKind(t *testing.T, q *Queue, r Runner, folders bool) string {
	t.Helper()
	j, _, ok, _, err := q.Take(r.ID, lease, folders)
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
// machine; a runner of none, or of another, never takes it.
func TestTakeRoutesByConnector(t *testing.T) {
	q, _ := open(t)
	none := capsRunner(t, q, nil, nil)
	other := capsRunner(t, q, nil, &Capabilities{Connector: conB})
	owner := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	j := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA})
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
	if err != nil || len(list) != 1 || list[0].Ref != refX || list[0].Machine != conA || list[0].Name != "proj" {
		t.Fatalf("%+v %v", list, err)
	}
}

type fakeIssuer map[string]string

func (f fakeIssuer) Machine() string { return conA }
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
	if msg := ResolveFolders(&sp, f); msg != "" || sp.Folder != "" || sp.FolderRef == "" || sp.Machine != conA || f[sp.FolderRef] != "/a/b" {
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
	if msg := ResolveFolders(&Spec{FolderRef: "/a", Machine: conA}, nil); msg == "" {
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
	for _, s := range []string{`ALTER TABLE runners DROP COLUMN caps`, `ALTER TABLE copies DROP COLUMN snapshot`} {
		if _, err := q.db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	q.Close()
	if q, err = Open(path, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	for _, c := range []struct{ table, col string }{{"runners", "caps"}, {"copies", "snapshot"}} {
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
	if got.Spec.Folder != "" || got.Spec.FolderRef == "" || got.Spec.Machine != conA || f[got.Spec.FolderRef] != "/p/q" {
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
// another machine that has the harness does not hold back the folder's
// own runner, whose engine then says how to install it.
func TestHarnessRuleCountsOnlyEligibleRunners(t *testing.T) {
	q, _ := open(t)
	owner := capsRunner(t, q, nil, &Capabilities{Connector: conA, Harnesses: found("claude")})
	capsRunner(t, q, []string{"gpu"}, &Capabilities{Connector: conB, Harnesses: found("claude", "codex")})
	j := makeJob(t, q, Spec{Harness: "codex", FolderRef: refX, Machine: conA})
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

// Equal paths on two machines are two folders.
func TestFoldersOfTwoConnectors(t *testing.T) {
	q, _ := open(t)
	capsRunner(t, q, nil, &Capabilities{Connector: conA, Folders: []FolderInfo{{Ref: refX, Name: "p"}}})
	capsRunner(t, q, nil, &Capabilities{Connector: conB, Folders: []FolderInfo{{Ref: refX, Name: "p"}}})
	if list, err := q.Folders(DefaultTeam); err != nil || len(list) != 2 || list[0].Machine == list[1].Machine {
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

// DisplayFolder keeps a folder's path only when the folder is on this machine
// (machine empty or this one), never a folder on another machine, and never
// panics on a nil issuer.
func TestDisplayFolder(t *testing.T) {
	f := fakeIssuer{"fld_x": "/src/app"} // Machine() is conA
	cases := []struct {
		ref, machine, want string
	}{
		{"fld_x", "", "/src/app"},   // own machine, unspecified
		{"fld_x", conA, "/src/app"}, // own machine, named
		{"fld_x", conB, ""},         // another machine: no path
		{"fld_unknown", "", ""},     // unknown reference
	}
	for _, c := range cases {
		if got := DisplayFolder(f, c.ref, c.machine); got != c.want {
			t.Errorf("DisplayFolder(%q, %q) = %q, want %q", c.ref, c.machine, got, c.want)
		}
	}
	if got := DisplayFolder(nil, "fld_x", ""); got != "" {
		t.Errorf("nil issuer: %q, want empty", got)
	}
}

// compat 0.6: drop connector. Routing reads the machine id under either key
// on both the job's spec and the runner's caps, so a 0.6 control and a job
// stored by a build before 0.6 both route. A 0.6 spec names the id under both
// keys (Spec.MarshalJSON); a runner's caps name it under connector alone
// (runner/caps.go, for the strict control decode). This covers a 0.6 job
// against a connector-caps runner, and a pre-0.6 job whose stored spec names
// the machine under connector only.
func TestTakeRoutesMachineCrossVersion(t *testing.T) {
	q, _ := open(t)
	owner := capsRunner(t, q, nil, &Capabilities{Connector: conA})
	other := capsRunner(t, q, nil, &Capabilities{Connector: conB})
	// A 0.6 job: its stored spec names the id under both keys.
	j := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conA})
	if got := takes(t, q, other); got != "" {
		t.Fatalf("a runner on another machine took the 0.6 job: %s", got)
	}
	if got := takes(t, q, owner); got != j.ID {
		t.Fatalf("the machine's runner did not take the 0.6 job: %q want %s", got, j.ID)
	}
	// A pre-0.6 job: rewrite a fresh job's stored spec to name the machine
	// under connector only, as a build before 0.6 stored it, then route it.
	j2 := makeJob(t, q, Spec{Harness: "claude", FolderRef: refX, Machine: conB})
	if _, err := q.db.Exec(`UPDATE jobs SET spec = ? WHERE id = ?`,
		`{"harness":"claude","folder_ref":"`+refX+`","connector":"`+conB+`"}`, j2.ID); err != nil {
		t.Fatal(err)
	}
	if got := takes(t, q, owner); got != "" {
		t.Fatalf("a runner on another machine took the pre-0.6 job: %s", got)
	}
	if got := takes(t, q, other); got != j2.ID {
		t.Fatalf("the machine's runner did not take the pre-0.6 connector-only job: %q want %s", got, j2.ID)
	}
}

// compat 0.6: drop connector. A runner reports its machine id under the old
// key alone: control's runner-wire decode is strict (jobs.decode calls
// DisallowUnknownFields), and a control of a build before 0.6 has no "machine"
// field, so an unknown field there is a 400. This pins that the caps a runner
// sends carry connector and not machine, so a pre-0.6 control still enrolls it.
func TestCapabilitiesWireIsConnectorOnlyForStrictOldControl(t *testing.T) {
	// As runner/caps.go builds them: the id under the old key only.
	b, err := json.Marshal(Capabilities{OS: "darwin", Connector: conA, Folders: []FolderInfo{{Ref: refX, Name: "p"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"machine"`) {
		t.Fatalf("caps carry a machine key a strict pre-0.6 control would refuse: %s", b)
	}
	// A control of a build before 0.6 has no machine field and decodes strictly.
	type oldCaps struct {
		OS        string       `json:"os,omitempty"`
		Arch      string       `json:"arch,omitempty"`
		Connector string       `json:"connector,omitempty"`
		Folders   []FolderInfo `json:"folders,omitempty"`
		Role      string       `json:"role,omitempty"`
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	var oc oldCaps
	if err := d.Decode(&oc); err != nil {
		t.Fatalf("a strict pre-0.6 control refused the caps: %v", err)
	}
	if oc.Connector != conA {
		t.Fatalf("pre-0.6 control read connector %q, want %s", oc.Connector, conA)
	}
	// And this is why: caps that DO carry a machine key are refused by that
	// same strict decode, which is what a dual-send would have sent.
	bad, _ := json.Marshal(Capabilities{OS: "darwin", Machine: conA, Connector: conA})
	d2 := json.NewDecoder(strings.NewReader(string(bad)))
	d2.DisallowUnknownFields()
	if err := d2.Decode(&oldCaps{}); err == nil {
		t.Fatal("a strict pre-0.6 control accepted a machine key; dual-send would break enroll")
	}
}

// compat 0.6: drop connector. A 0.6 control sends the take spec with the
// machine id under both keys: a runner reads the take leniently, and one of a
// build before 0.6 takes the id from connector while a 0.6 runner takes it
// from machine. This pins that the spec a control sends carries both.
func TestSpecWireCarriesBothKeysForOldRunner(t *testing.T) {
	b, err := json.Marshal(Spec{Harness: "claude", FolderRef: refX, Machine: conA})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"machine":"`+conA+`"`) || !strings.Contains(s, `"connector":"`+conA+`"`) {
		t.Fatalf("the spec must carry the id under both keys for one release: %s", s)
	}
	// A runner of a build before 0.6 reads only connector.
	var old struct {
		FolderRef string `json:"folder_ref"`
		Connector string `json:"connector"`
	}
	if json.Unmarshal(b, &old) != nil || old.Connector != conA {
		t.Fatalf("a pre-0.6 runner could not read the machine from the spec: %q", old.Connector)
	}
	// A 0.6 reader takes it from machine; Spec has no connector field to leak
	// into (the old key lives only inside Marshal/UnmarshalJSON).
	var sp Spec
	if json.Unmarshal(b, &sp) != nil || sp.Machine != conA {
		t.Fatalf("0.6 read: machine %q", sp.Machine)
	}
}

// compat 0.6: drop connector. A spec a build before 0.6 stored under the old
// key survives a read and a re-store: UnmarshalJSON takes the id into Machine,
// and MarshalJSON writes it back under both keys, so neither a 0.6 reader nor
// a pre-0.6 runner loses it. Spec has no connector field for a caller to set.
func TestSpecRoundTripFromConnectorOnly(t *testing.T) {
	var sp Spec
	if err := json.Unmarshal([]byte(`{"folder_ref":"`+refX+`","connector":"`+conA+`"}`), &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Machine != conA {
		t.Fatalf("read connector-only: machine %q, want %s", sp.Machine, conA)
	}
	b, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"machine":"`+conA+`"`) || !strings.Contains(s, `"connector":"`+conA+`"`) {
		t.Fatalf("re-store dropped a key: %s", s)
	}
}

// compat 0.6.5: drop sandbox. A runner of a build before 0.6.5 still reports
// the retired wall under "sandbox". Control's runner-wire decode is strict,
// so the key must decode; nothing of it is kept, and no runner sends it now.
func TestCapabilitiesAcceptRetiredSandbox(t *testing.T) {
	old := `{"os":"linux","sandbox":{"can":false,"why":"AppArmor refuses user namespaces"}}`
	d := json.NewDecoder(strings.NewReader(old))
	d.DisallowUnknownFields()
	var c Capabilities
	if err := d.Decode(&c); err != nil {
		t.Fatalf("a strict decode refused a pre-0.6.5 runner's caps: %v", err)
	}
	if c.OS != "linux" || c.Check() != "" {
		t.Fatalf("caps %+v, check %q", c, c.Check())
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sandbox") {
		t.Fatalf("the retired key was kept or sent: %s", b)
	}
}
