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
	// Connector: the connector id of the folders this runner owns, when it
	// owns any. Folders are those folders, at most MaxFolders.
	Connector string       `json:"connector,omitempty"`
	Folders   []FolderInfo `json:"folders,omitempty"`
	// Role: RoleConnector for a connector, which takes only snapshot and
	// apply jobs of its own folders; empty for a runner, which takes every
	// other job. A runner that reports Connector shares the connector's
	// machine and disk: it can use a folder as it is, and copy it by the
	// local fast path.
	Role string `json:"role,omitempty"`
}

// RoleConnector is a connector's Capabilities.Role.
const RoleConnector = "connector"

// FolderInfo is one folder a connector owns, as control may know it: its
// reference and the last part of its path. The path itself stays with the
// connector.
type FolderInfo struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

// MaxFolders bounds the folders one runner reports.
const MaxFolders = 1000

// Shapes of the ids a connector issues.
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
	if c.Role != "" && c.Role != RoleConnector {
		return "capabilities.role must be connector, or absent"
	}
	if c.Role == RoleConnector && c.Connector == "" {
		return "a connector needs capabilities.connector"
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
// runner that could take the job (seen within RunnerFresh, with its
// connector and every label it asks for) does, or this runner has not
// reported yet, any runner may take it, and its engine refuses it with how
// to install the program, as on one machine. A job of a known session goes
// to the session's runner whatever it reports.
const harnessTake = `(j.session IS NOT NULL AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = j.session AND s.team = j.team))
	OR COALESCE(json_extract(j.spec, '$.agent'), '') = ''
	OR json_extract(r.caps, '$.harnesses') IS NULL
	OR EXISTS (SELECT 1 FROM json_each(r.caps, '$.harnesses') h
		WHERE json_extract(h.value, '$.name') = json_extract(j.spec, '$.agent') AND json_extract(h.value, '$.found'))
	OR NOT EXISTS (SELECT 1 FROM runners o, json_each(o.caps, '$.harnesses') h WHERE o.team = j.team AND o.seen >= ?
		AND json_extract(h.value, '$.name') = json_extract(j.spec, '$.agent') AND json_extract(h.value, '$.found')
		AND COALESCE(json_extract(o.caps, '$.role'), '') != 'connector'
		AND (COALESCE(json_extract(j.spec, '$.connector'), '') = '' OR ` + remoteIsolated + `
			OR json_extract(j.spec, '$.connector') = json_extract(o.caps, '$.connector'))
		AND NOT EXISTS (SELECT 1 FROM json_each(j.spec, '$.labels') l WHERE l.value NOT IN (SELECT value FROM json_each(o.labels))))`

// remoteIsolated: a job j that any runner can run, wherever its folder is: a
// first turn in an isolated copy of a folder, built from its snapshot.
const remoteIsolated = `(j.kind = 'turn' AND json_extract(j.spec, '$.isolate') AND COALESCE(json_extract(j.spec, '$.folder_ref'), '') != ''
	AND COALESCE(json_extract(j.spec, '$.repo_ref'), '') = '')`

// connectorTake: a job that names a folder's connector goes to a runner on
// the connector's machine (one that reports that connector), unless it is a
// first turn in an isolated copy of a folder, which any runner can build
// from the folder's snapshot.
const connectorTake = `COALESCE(json_extract(j.spec, '$.connector'), '') = '' OR ` + remoteIsolated + `
	OR json_extract(j.spec, '$.connector') = json_extract(r.caps, '$.connector')`

// roleTake: snapshot and apply jobs go only to the connector of their
// folder, and every other job only to a runner.
const roleTake = `CASE WHEN j.kind IN ('snapshot','apply')
	THEN COALESCE(json_extract(r.caps, '$.role'), '') = 'connector' AND json_extract(j.spec, '$.connector') = json_extract(r.caps, '$.connector')
	ELSE COALESCE(json_extract(r.caps, '$.role'), '') != 'connector' END`

// SpaceShape is a copy's id on its runner (isolation): a job id.
var SpaceShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// labelsTake: a job with labels goes only to a runner with every one.
const labelsTake = `NOT EXISTS (SELECT 1 FROM json_each(j.spec, '$.labels') l
	WHERE l.value NOT IN (SELECT value FROM json_each(r.labels)))`

// Folders is every folder the team's runners report, by runner, newest
// report first. A folder (its connector and reference) reported by two
// runners is listed once, from the one seen last. Equal paths on two
// connectors have one reference, and are two folders.
func (q *Queue) Folders(team string) ([]RunnerFolder, error) {
	rows, err := q.db.Query(`SELECT id, caps FROM runners WHERE team = ? AND caps IS NOT NULL ORDER BY seen DESC, id`, team)
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
	Name      string `json:"name" doc:"The last part of the folder's path, for a screen. The path stays on the connector's machine."`
	Connector string `json:"connector" doc:"The connector that owns the folder."`
	Runner    string `json:"runner" doc:"The runner that reported it."`
}

// ConnectorSeen says whether the connector of this id asked for work within
// RunnerFresh. A connected one asks about once a second, so one not seen
// that long is gone: its folder's snapshots and applies wait for nothing.
func (q *Queue) ConnectorSeen(team, connector string) (bool, error) {
	var n int
	err := q.db.QueryRow(`SELECT COUNT(*) FROM runners WHERE team = ? AND seen >= ?
		AND json_extract(caps, '$.role') = ? AND json_extract(caps, '$.connector') = ?`,
		team, nanos(q.now().Add(-RunnerFresh)), RoleConnector, connector).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("queue: read runners: %w", err)
	}
	return n > 0, nil
}

// Runners is every runner of the team, newest seen first.
func (q *Queue) Runners(team string) ([]Runner, error) {
	rows, err := q.db.Query(`SELECT `+runnerCols+` FROM runners WHERE team = ? ORDER BY seen DESC, id`, team)
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
// runners.caps, sessions.snapshot and jobs.turn. The first two start empty.
// jobs.turn is filled for the turn and review jobs made before it: the id of
// the attempt that started the turn, which was the turn's id then, or a new
// id for a job whose turn no runner has started.
func addColumns(db *sql.DB) error {
	for _, c := range []struct{ table, column, decl string }{
		{"runners", "caps", "TEXT"},
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

// FolderIssuer is a connector on this machine, as the control plane of
// agentbus up reaches it: package folders. It turns a path a request names
// into the reference the queue keeps, and says whether one folder is inside
// another, so a check that needs the path (a turn token's folder) runs where
// the folder is. agentbus server has none, and takes references only.
type FolderIssuer interface {
	Connector() string
	Register(path string) (ref string, err error)
	Path(ref string) (string, bool)
	// Within says the folder at path is the folder ref or inside it, and
	// WithinRef the same of the folder child: on the connector, where the
	// folders are, once every symbolic link is resolved.
	Within(path, ref string) bool
	WithinRef(child, ref string) bool
	// WithinDir says the same of two paths on the connector's machine.
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
				return "a folder_ref needs connector, from GET /api/v0/folders"
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
// start; a server has no connector, and its runners register such a path
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
				// A path the connector cannot keep stays, and its runner
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
