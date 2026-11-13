package queue

// What a runner can do, and the folders it owns. A runner reports them at
// enroll and on its heartbeat; Take routes a job only to a runner that can
// run it. plans/IDENTITIES.md in the notes says who issues each id.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/harness"
)

// Capabilities is what one runner reports of itself. Runners hold no
// secrets: which credential an agent uses is the control plane's business,
// so none is listed here.
type Capabilities struct {
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
	// Isolation: the isolation backends the runner can use.
	Isolation []string `json:"isolation,omitempty"`
	// Harnesses: each harness's program on the runner's machine. Nil until
	// the runner's first probe has ended.
	Harnesses []harness.Report `json:"harnesses,omitempty"`
	// Connector: the id of the runner's machine (con_ and 16 hex digits),
	// the id of the folders it holds. A job that names it goes to this
	// runner only. Folders are those folders, at most MaxFolders. The field
	// keeps its name from when a separate connector held the folders.
	Connector string       `json:"connector,omitempty"`
	Folders   []FolderInfo `json:"folders,omitempty"`
	// Sandbox: whether the runner can build a session's wall (package
	// sandbox), and why not. Nil from a runner of a build before it reported
	// it.
	Sandbox *SandboxReport `json:"sandbox,omitempty"`
	// Role was RoleConnector for a connector of a build before 0.5. A
	// runner stored with it is listed as such; none may report it now.
	Role string `json:"role,omitempty"`
}

// SandboxReport is whether a runner can build a session's wall: the system's
// sandbox program is there (sandbox-exec on macOS, bwrap on Linux) and starts
// a program in a wall, which on Linux needs unprivileged user namespaces.
type SandboxReport struct {
	Can bool `json:"can"`
	// Why says, in a plain sentence, why it cannot. Empty when it can.
	Why string `json:"why,omitempty"`
}

// MaxSandboxWhy bounds SandboxReport.Why.
const MaxSandboxWhy = 500

// NewSandboxReport is the report a runner sends: why cut to MaxSandboxWhy
// bytes, on a character's edge, so a long reason never stops it enrolling.
func NewSandboxReport(can bool, why string) *SandboxReport {
	if len(why) > MaxSandboxWhy {
		cut := MaxSandboxWhy - len("…")
		for cut > 0 && !utf8.RuneStart(why[cut]) {
			cut--
		}
		why = why[:cut] + "…"
	}
	return &SandboxReport{Can: can, Why: why}
}

// RoleConnector is the Capabilities.Role of a connector of a build before
// 0.5, when a machine's folders had a program of their own.
const RoleConnector = "connector"

// FolderInfo is one folder a runner holds, as control may know it: its
// reference and the last part of its path. The path itself stays on the
// runner's machine.
type FolderInfo struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

// MaxFolders bounds the folders one runner reports.
const MaxFolders = 1000

// Shapes of the ids a runner issues for its machine and its folders.
var (
	ConnectorShape = regexp.MustCompile(`^con_[0-9a-f]{16}$`)
	FolderRefShape = regexp.MustCompile(`^fld_[0-9a-f]{16}$`)
	labelShape     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// LabelOK says a label is 1 to 64 of A-Z, a-z, 0-9 and . _ : -, starting with a
// letter or digit.
func LabelOK(l string) bool { return labelShape.MatchString(l) }

// Check says what is wrong with c, or "".
func (c Capabilities) Check() string {
	if c.Connector != "" && !ConnectorShape.MatchString(c.Connector) {
		return "capabilities.connector must be con_ and 16 hex digits"
	}
	if c.Role != "" {
		return "agentbus connector was retired in 0.5: run agentbus runner on the folder's machine, with the same data folder"
	}
	if len(c.Folders) > MaxFolders || len(c.Harnesses) > 64 || len(c.Isolation) > 16 || len(c.OS) > 32 || len(c.Arch) > 32 {
		return "capabilities lists too much"
	}
	if len(c.Folders) > 0 && c.Connector == "" {
		return "capabilities.folders need capabilities.connector"
	}
	for _, f := range c.Folders {
		if !FolderRefShape.MatchString(f.Ref) || len(f.Name) > 255 {
			return "each folder needs ref fld_ and 16 hex digits, and a name of at most 255 bytes"
		}
	}
	for _, h := range c.Harnesses {
		if len(h.Name) > 64 || len(h.Version) > 64 {
			return "a harness's name and version are at most 64 bytes"
		}
	}
	if c.Sandbox != nil && len(c.Sandbox.Why) > MaxSandboxWhy {
		return "capabilities.sandbox.why is at most 500 bytes"
	}
	for _, b := range c.Isolation {
		if len(b) > 32 {
			return "an isolation backend's name is at most 32 bytes"
		}
	}
	return ""
}

// RunnerFresh is how recently a runner must have been seen for its report
// to count when Take decides whether some runner has a job's harness.
const RunnerFresh = time.Minute

// harnessTake is Take's harness rule for a job j and runner r: a session's
// first job on a harness goes to a runner that reports it found. When no
// runner that could take the job (seen within RunnerFresh, on the job's
// machine, with every label it asks for) does, or this runner has not
// reported yet, any runner may take it, and its engine refuses it with how
// to install the program, as on one machine. A job of a known session goes
// to the session's runner whatever it reports.
const harnessTake = `(j.session IS NOT NULL AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = j.session AND s.team = j.team))
	OR COALESCE(json_extract(j.spec, '$.agent'), '') = ''
	OR json_extract(r.caps, '$.harnesses') IS NULL
	OR EXISTS (SELECT 1 FROM json_each(r.caps, '$.harnesses') h
		WHERE json_extract(h.value, '$.name') = json_extract(j.spec, '$.agent') AND json_extract(h.value, '$.found'))
	OR NOT EXISTS (SELECT 1 FROM runners o, json_each(o.caps, '$.harnesses') h WHERE o.team = j.team AND o.seen >= ? AND o.removed IS NULL
		AND json_extract(h.value, '$.name') = json_extract(j.spec, '$.agent') AND json_extract(h.value, '$.found')
		AND (COALESCE(json_extract(j.spec, '$.connector'), '') = '' OR json_extract(j.spec, '$.connector') = json_extract(o.caps, '$.connector'))
		AND NOT EXISTS (SELECT 1 FROM json_each(j.spec, '$.labels') l WHERE l.value NOT IN (SELECT value FROM json_each(o.labels))))`

// machineTake: a job that names a machine (spec.connector) goes to the
// runner on that machine and no other. A session runs on the machine that
// holds its folder. One exception keeps a session made before 0.5 going: a
// next job of a session another machine's runner already holds (its first
// turn was built there from a snapshot) stays with that runner. Folder jobs
// never take it: an apply is always the folder's own machine's.
const machineTake = `COALESCE(json_extract(j.spec, '$.connector'), '') = ''
	OR json_extract(j.spec, '$.connector') = json_extract(r.caps, '$.connector')
	OR (j.kind NOT IN ` + folderKinds + ` AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = j.session AND s.team = j.team AND s.runner = r.id))`

// folderKinds are the jobs a runner does to its machine's folders, not in a
// session's copy: they take a runner's folder places (Take), not its turn
// places, so an apply never waits behind running turns.
const folderKinds = `('apply','preview','folder')`

// FolderCapacity is how many folder jobs (folderKinds) one runner runs at
// once, besides its capacity of turns and reviews. Applies of one folder
// take turns (package connector).
const FolderCapacity = 4

// SpaceShape is a copy's id on its runner (isolation): a job id.
var SpaceShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// labelsTake: a job with labels goes only to a runner with every one.
const labelsTake = `NOT EXISTS (SELECT 1 FROM json_each(j.spec, '$.labels') l
	WHERE l.value NOT IN (SELECT value FROM json_each(r.labels)))`

// Folders is every folder the team's runners report, by runner, newest
// report first. A folder (its machine and reference) reported by two
// runners is listed once, from the one seen last. Equal paths on two
// machines have one reference, and are two folders.
func (q *Queue) Folders(team string) ([]RunnerFolder, error) {
	rows, err := q.db.Query(`SELECT id, caps FROM runners WHERE team = ? AND caps IS NOT NULL AND removed IS NULL ORDER BY seen DESC, id`, team)
	if err != nil {
		return nil, fmt.Errorf("queue: read runners: %w", err)
	}
	defer rows.Close()
	out := []RunnerFolder{}
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		var raw sql.NullString
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("queue: read runners: %w", err)
		}
		var c Capabilities
		if json.Unmarshal([]byte(raw.String), &c) != nil {
			continue
		}
		for _, f := range c.Folders {
			key := c.Connector + "/" + f.Ref
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, RunnerFolder{Ref: f.Ref, Name: f.Name, Connector: c.Connector, Runner: id})
		}
	}
	return out, rows.Err()
}

// RunnerFolder is one folder in Folders.
type RunnerFolder struct {
	Ref       string `json:"ref" doc:"The folder's reference: name it as folder_ref."`
	Name      string `json:"name" doc:"The last part of the folder's path, for a screen. The path stays on the folder's machine."`
	Connector string `json:"connector" doc:"The machine that holds the folder (con_ and 16 hex digits): a session in it names it as connector, and runs there."`
	Runner    string `json:"runner" doc:"The runner that reported it: the one on that machine."`
}

// MachineSeen says whether a runner on the machine of this id (one that
// reports it as its connector) asked for work within RunnerFresh. A
// connected one asks about once a second, so one not seen that long is
// gone: its folders' applies and checks wait for nothing.
func (q *Queue) MachineSeen(team, machine string) (bool, error) {
	var n int
	err := q.db.QueryRow(`SELECT COUNT(*) FROM runners WHERE team = ? AND seen >= ? AND removed IS NULL
		AND COALESCE(json_extract(caps, '$.role'), '') = '' AND json_extract(caps, '$.connector') = ?`,
		team, nanos(q.now().Add(-RunnerFresh)), machine).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("queue: read runners: %w", err)
	}
	return n > 0, nil
}

// Runners is every runner of the team not removed, newest seen first.
func (q *Queue) Runners(team string) ([]Runner, error) {
	rows, err := q.db.Query(`SELECT `+runnerCols+` FROM runners WHERE team = ? AND removed IS NULL ORDER BY seen DESC, id`, team)
	if err != nil {
		return nil, fmt.Errorf("queue: read runners: %w", err)
	}
	defer rows.Close()
	var out []Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: read runners: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// addColumns adds the columns a queue made by an earlier build lacks:
// runners.caps, runners.removed, sessions.snapshot and jobs.turn. The first
// three start empty.
// jobs.turn is filled for the turn and review jobs made before it: the id of
// the attempt that started the turn, which was the turn's id then, or a new
// id for a job whose turn no runner has started.
func addColumns(db *sql.DB) error {
	for _, c := range []struct{ table, column, decl string }{
		{"runners", "caps", "TEXT"},
		{"runners", "removed", "INTEGER"},
		{"sessions", "snapshot", "TEXT NOT NULL DEFAULT ''"},
		{"jobs", "turn", "TEXT"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n); err != nil {
			return fmt.Errorf("queue: read tables: %w", err)
		}
		if n > 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.column + ` ` + c.decl); err != nil {
			return fmt.Errorf("queue: add %s.%s: %w", c.table, c.column, err)
		}
		if c.column == "turn" {
			// The attempt that started the turn named it then. Expiry clears
			// jobs.attempt, so the attempt is found by its turn.created.
			if _, err := db.Exec(`UPDATE jobs SET turn = COALESCE(
					(SELECT a.id FROM attempts a WHERE a.job = jobs.id AND EXISTS (SELECT 1 FROM attempt_events e
						WHERE e.attempt = a.id AND e.kind = 'turn.created') ORDER BY a.number DESC LIMIT 1),
					attempt, 't_' || lower(hex(randomblob(8))))
				WHERE turn IS NULL AND kind IN ('turn', 'review')`); err != nil {
				return fmt.Errorf("queue: fill jobs.turn: %w", err)
			}
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS jobs_turn ON jobs(turn) WHERE turn IS NOT NULL`); err != nil {
		return fmt.Errorf("queue: index jobs.turn: %w", err)
	}
	return nil
}

// SnapshotShape is a snapshot id: the hex SHA-256 of a checkpoint's root
// tree (package isolation).
var SnapshotShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FolderIssuer is this machine's folders, as the control plane of agentbus
// up reaches them: package folders. It turns a path a request names
// into the reference the queue keeps, and says whether one folder is inside
// another, so a check that needs the path (a turn token's folder) runs where
// the folder is. agentbus server has none, and takes references only.
type FolderIssuer interface {
	Connector() string
	Register(path string) (ref string, err error)
	// Add is Register for a folder a person typed: it must be an existing
	// directory they can read, and its error says why not in one sentence.
	Add(path string) (ref string, err error)
	Path(ref string) (string, bool)
	// Within says the folder at path is the folder ref or inside it, and
	// WithinRef the same of the folder child: on the folders' machine, once
	// every symbolic link is resolved.
	Within(path, ref string) bool
	WithinRef(child, ref string) bool
	// WithinDir says the same of two paths on the folders' machine.
	WithinDir(path, dir string) bool
}

// ResolveFolders turns the paths spec names into references issued by f, and
// clears the paths, so only references are kept. A spec that names
// references keeps them. It answers what is wrong, or "".
func ResolveFolders(spec *Spec, f FolderIssuer) string {
	if spec.Folder == "" && spec.Repo == "" {
		if spec.FolderRef != "" || spec.RepoRef != "" {
			if (spec.FolderRef != "" && !FolderRefShape.MatchString(spec.FolderRef)) ||
				(spec.RepoRef != "" && !FolderRefShape.MatchString(spec.RepoRef)) {
				return "folder_ref and repo_ref are fld_ and 16 hex digits, from GET /api/v0/folders"
			}
			if spec.Connector == "" && f != nil {
				spec.Connector = f.Connector()
			}
			if !ConnectorShape.MatchString(spec.Connector) {
				return "a folder_ref needs connector, its machine, from GET /api/v0/folders"
			}
		}
		return ""
	}
	if spec.FolderRef != "" || spec.RepoRef != "" {
		return "name a folder by its path or by its reference, not both"
	}
	if f == nil {
		return "this server has no folders of its own: name folder_ref and connector from GET /api/v0/folders, which lists the folders its runners own"
	}
	for _, p := range []struct{ path, ref *string }{{&spec.Folder, &spec.FolderRef}, {&spec.Repo, &spec.RepoRef}} {
		if *p.path == "" {
			continue
		}
		ref, err := f.Register(*p.path)
		if err != nil {
			return "folder must be an absolute path to an existing directory, at most 4096 bytes"
		}
		*p.ref, *p.path = ref, ""
	}
	spec.Connector = f.Connector()
	return ""
}

// MigrateFolders rewrites every job whose spec still names a path, as a queue
// made before folder references holds them, to the references f issues: the
// paths go. It answers how many jobs it rewrote. agentbus up runs it at
// start; a server has no folders, and its runners register such a path
// when they take the job.
//
// The job's request hash is left as it was: a retry, after the upgrade, of
// a request made before it gets 409 rather than the first job.
func (q *Queue) MigrateFolders(f FolderIssuer) (int, error) {
	n := 0
	err := q.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id, spec FROM jobs WHERE COALESCE(json_extract(spec, '$.folder'), '') != ''
			OR COALESCE(json_extract(spec, '$.repo'), '') != ''`)
		if err != nil {
			return fmt.Errorf("queue: read jobs: %w", err)
		}
		type row struct{ id, spec string }
		var todo []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.spec); err != nil {
				rows.Close()
				return fmt.Errorf("queue: read jobs: %w", err)
			}
			todo = append(todo, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range todo {
			var sp Spec
			if json.Unmarshal([]byte(r.spec), &sp) != nil {
				continue
			}
			if ResolveFolders(&sp, f) != "" {
				// A path the folders cannot keep stays, and its runner
				// refuses it as it would have.
				continue
			}
			sb, _ := json.Marshal(sp)
			if _, err := tx.Exec(`UPDATE jobs SET spec = ? WHERE id = ?`, string(sb), r.id); err != nil {
				return fmt.Errorf("queue: write job: %w", err)
			}
			n++
		}
		return nil
	})
	return n, err
}
