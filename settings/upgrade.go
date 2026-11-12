package settings

// The upgrade to the settings database. On the first start of a build that
// keeps settings in settings.db, control:
//
//  1. takes the data folder's lock, so no other service writes it;
//  2. makes a copy of the data folder beside its real path (links
//     followed), named with the date and "before-settings-db" (CopyName),
//     unless the folder is new;
//  3. imports permissions.toml, instructions.md and agents.json into the
//     database, in one transaction that also marks the import done;
//  4. writes out the old value of each default this build changed, so
//     nothing changes for the person, and removes the keys it retired
//     from the stored policy (writeSteps).
//
// The copy is made in a folder of its own name with .partial on the end and
// renamed when whole, and the database is made only once the copy is whole.
// So a start that stops anywhere in between starts over on the next start,
// and the import is either all in the database or none of it. Once the import
// is done, a start does nothing here, and the files are never read again:
// they stay where they are, for the person. Tokens and subscriptions/ stay
// files.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CopySuffix ends the name of the copy of the data folder.
const CopySuffix = "before-settings-db"

// Upgrade says how to make a data folder's settings database ready.
type Upgrade struct {
	// Dir is the data folder.
	Dir string
	// Lock is the lock file the service holds on the data folder (the
	// store's events.db.lock). The upgrade holds it while it copies and
	// imports, so no other service writes the folder meanwhile.
	Lock string
	// Skip are names at the top of the data folder the copy leaves out: the
	// session copies and the blob store, which this upgrade does not
	// change and which can be as large as the person's folders.
	Skip []string
	// Agents reads an agents.json as database rows (agents.ImportRows).
	Agents func([]byte) ([]AgentRow, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Report says what an upgrade did.
type Report struct {
	Copy     string   // the copy it made; "" when it made none
	Imported []string // the files it imported
	Done     bool     // false when the import was done already
	Kept     []string // the policy keys whose old default it wrote out
	Removed  []string // the retired policy keys it removed from the stored policy
}

// CopyName is the copy of the data folder dir made at t: beside it, its name
// with the date and CopySuffix, such as ~/.agentbus-2026-11-12-before-settings-db.
// dir is the folder's real path (realPath): a sibling in its real parent is
// never inside it.
func CopyName(dir string, t time.Time) string {
	dir = filepath.Clean(dir)
	return filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-"+t.Format("2006-01-02")+"-"+CopySuffix)
}

// realPath is dir absolute, with every link in it followed: the folder
// itself, however it was named.
func realPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Run makes the settings database ready and opens it.
func (u Upgrade) Run() (*DB, Report, error) {
	now := u.Now
	if now == nil {
		now = time.Now
	}
	var rep Report
	unlock, err := lockDir(u.Lock)
	if err != nil {
		return nil, rep, err
	}
	defer unlock()
	path := filepath.Join(u.Dir, DBFile)
	_, statErr := os.Stat(path)
	hasDB := statErr == nil
	if !hasDB && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, rep, fmt.Errorf("settings: %s could not be read: %w", path, statErr)
	}
	// A data folder from before settings.db keeps the defaults it ran under:
	// none of the upgrade steps are written yet. A new one has nothing to
	// keep, so it starts with all of them done. The database is made with
	// its count, so a start that stops after this keeps it.
	done := len(upgradeSteps)
	if !hasDB && !newFolder(u.Dir) {
		done = 0
		// One rule for where the copy comes from and where it goes: the
		// data folder's real path, however it was named (relative, or
		// through links). The walk starts at the folder itself, and the copy
		// is its sibling in its real parent, which is never inside it.
		src, err := realPath(u.Dir)
		if err != nil {
			return nil, rep, fmt.Errorf("settings: the data folder %s could not be found: %w", u.Dir, err)
		}
		if rep.Copy, err = copyFolder(src, CopyName(src, now()), u.Skip); err != nil {
			return nil, rep, err
		}
	}
	db, err := openDB(path, now, done)
	if err != nil {
		return nil, rep, err
	}
	db.SetProtected(u.Dir)
	if rep.Imported, rep.Done, err = db.importFiles(u.Dir, u.Agents); err != nil {
		db.Close()
		return nil, rep, err
	}
	if rep.Kept, rep.Removed, err = db.writeSteps(); err != nil {
		db.Close()
		return nil, rep, err
	}
	return db, rep, nil
}

// upgradesDone is how many of upgradeSteps the database has written. A
// database made by a build that kept a defaults version instead (meta
// defaults, 1 for 0.4's) has written all the steps before that version.
func upgradesDone(q querier) (int, error) {
	v, err := meta(q, "upgrades")
	if err != nil {
		return 0, err
	}
	if v == "" {
		if v, err = meta(q, "defaults"); err != nil {
			return 0, err
		}
		n, _ := strconv.Atoi(v)
		return min(max(n-1, 0), len(upgradeSteps)), nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("the stored upgrade count %q is not a number", v)
	}
	return min(n, len(upgradeSteps)), nil
}

// setUpgradesDone stores that every upgrade step is written.
func setUpgradesDone(tx *sql.Tx) error {
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('upgrades', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(len(upgradeSteps))); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM meta WHERE key = 'defaults'`)
	return err
}

// writeSteps runs, once, the upgrade steps the database has not written.
// A changed default's old value goes into the person's policy, unless they
// set that key themselves: so an upgrade never changes what a turn may do. A
// retired key is removed from the stored policy, where a save before this
// build left it. The write is one change, with its settings.changed event
// naming the keys (never a value), in the transaction that marks the steps
// done. kept and removed are the keys it wrote and removed; none when
// nothing was waiting.
func (d *DB) writeSteps() (kept, removed []string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fail := func(err error) ([]string, []string, error) {
		return nil, nil, fmt.Errorf("settings: the upgrade steps could not be written: %w", err)
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	n, err := upgradesDone(tx)
	if err != nil {
		return fail(err)
	}
	legacy, err := meta(tx, "defaults")
	if err != nil {
		return fail(err)
	}
	if n == len(upgradeSteps) && legacy == "" {
		return nil, nil, nil
	}
	s, err := readState(tx)
	if err != nil {
		return fail(err)
	}
	// An overlay that is not readable has no value to keep and no key to
	// remove: turns refuse until the person writes the policy again, and
	// that write is theirs.
	if s.overlay != nil {
		o := flat{}
		for k, v := range s.overlay {
			o[k] = v
		}
		for _, st := range upgradeSteps[n:] {
			_, set := o[st.Key]
			switch {
			case st.Retired && set:
				delete(o, st.Key)
				if i := slices.Index(kept, st.Key); i >= 0 {
					// Written by an earlier step of this start: never stored.
					kept = slices.Delete(kept, i, i+1)
				} else {
					removed = append(removed, st.Key)
				}
			case !st.Retired && !set:
				o[st.Key] = st.Old
				kept = append(kept, st.Key)
			}
		}
		if keys := append(slices.Clone(kept), removed...); len(keys) > 0 {
			if err := putSetting(tx, SettingPermissions, o, d.now()); err != nil {
				return fail(err)
			}
			if err := addChanges(tx, s.version, []Change{{Setting: SettingPermissions, Keys: keys}}, d.now()); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := setUpgradesDone(tx); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return kept, removed, nil
}

// lockDir takes the lock at path, refusing when another service holds it.
func lockDir(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("settings: the lock file %s could not be opened: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another agentbus service uses this data folder")
		}
		return nil, fmt.Errorf("settings: lock: %w", err)
	}
	return func() { f.Close() }, nil
}

// newFolder says dir holds none of a service's files yet: there is nothing
// to keep a copy of.
func newFolder(dir string) bool {
	for _, f := range []string{"events.db", "jobs.db", PolicyFile, InstructionsFile, AgentsFile} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			return false
		}
	}
	return true
}

// copyFolder copies dir to dst, leaving out the names in skip at its top.
// It copies into dst.partial and renames it when whole. A dst there already
// is a copy an earlier start made whole today: it is kept, and nothing is
// copied.
func copyFolder(dir, dst string, skip []string) (string, error) {
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	partial := dst + ".partial"
	// Only a copy of ours, left by a start that stopped, is ever there.
	if err := os.RemoveAll(partial); err != nil {
		return "", fmt.Errorf("settings: the unfinished copy %s could not be cleared: %w", partial, err)
	}
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("settings: the data folder could not be copied to %s before the upgrade, so it did not start: %w; free space there, or move the folder's parent to a place agentbus can write", dst, err)
	}
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel != "." && !strings.Contains(rel, string(filepath.Separator)) && skipped[rel] {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		out := filepath.Join(partial, rel)
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			if err := os.Mkdir(out, info.Mode().Perm()|0o700); err != nil {
				return err
			}
			return nil
		case e.Type()&fs.ModeSymlink != 0:
			to, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(to, out)
		case e.Type().IsRegular():
			return copyFile(path, out, info.Mode().Perm())
		}
		return nil // sockets and the like are not data
	})
	if err != nil {
		return fail(err)
	}
	if err := os.Chmod(partial, 0o700); err != nil {
		return fail(err)
	}
	if err := os.Rename(partial, dst); err != nil {
		return fail(err)
	}
	return dst, nil
}

// copyFile copies one file: a clone where the file system makes one, else
// its bytes.
func copyFile(src, dst string, mode fs.FileMode) error {
	if cloneFile(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// importFiles imports dir's settings files, once. done is false when the
// import was done before.
func (d *DB) importFiles(dir string, readAgents func([]byte) ([]AgentRow, error)) (files []string, done bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, err := meta(d.db, "imported"); err != nil {
		return nil, false, fmt.Errorf("settings: %w", err)
	} else if t != "" {
		return nil, false, nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("settings: %w", err)
	}
	defer tx.Rollback()
	s, err := readState(tx)
	if err != nil {
		return nil, false, fmt.Errorf("settings: %w", err)
	}
	now := d.now()
	old := NewFiles(dir)
	var cs []Change
	fail := func(file string, err error) ([]string, bool, error) {
		return nil, false, fmt.Errorf("settings: %s in the data folder could not be imported into %s, so the service did not start: %v. Fix the file, or move it out of the data folder for the defaults, and start again", file, DBFile, err)
	}
	if exists(filepath.Join(dir, PolicyFile)) {
		// A key the file leaves out held its default under 0.4: it takes the
		// value it had then, kept as the person's own, so the policy after
		// the import is the one the file gave. The steps are then done.
		n, err := upgradesDone(tx)
		if err != nil {
			return nil, false, fmt.Errorf("settings: %w", err)
		}
		p, err := old.policyOver(policyBefore(n))
		if err != nil {
			return fail(PolicyFile, err)
		}
		if err := putSetting(tx, SettingPermissions, overlayOf(defaultPolicy(), p), now); err != nil {
			return nil, false, err
		}
		if err := setUpgradesDone(tx); err != nil {
			return nil, false, err
		}
		files = append(files, PolicyFile)
		cs = append(cs, Change{Setting: SettingPermissions, Keys: changedKeys(defaultPolicy(), p), File: PolicyFile})
	}
	if exists(filepath.Join(dir, InstructionsFile)) {
		text, _, err := old.Instructions()
		if err != nil {
			var se *Error
			if errors.As(err, &se) {
				err = errors.New(se.Why)
			}
			return fail(InstructionsFile, err)
		}
		if err := putInstructions(tx, text, now); err != nil {
			return nil, false, err
		}
		files = append(files, InstructionsFile)
		keys := []string{}
		if text != DefaultInstructions {
			keys = []string{"text"}
		}
		cs = append(cs, Change{Setting: SettingInstructions, Keys: keys, File: InstructionsFile})
	}
	if b, err := os.ReadFile(filepath.Join(dir, AgentsFile)); err == nil {
		if readAgents == nil {
			return fail(AgentsFile, errors.New("this service cannot read saved agents"))
		}
		rows, err := readAgents(b)
		if err != nil {
			return fail(AgentsFile, err)
		}
		keys := []string{}
		for _, r := range rows {
			if err := putAgent(tx, r, now); err != nil {
				return nil, false, err
			}
			keys = append(keys, r.ID)
		}
		files = append(files, AgentsFile)
		cs = append(cs, Change{Setting: SettingAgents, Keys: keys, File: AgentsFile})
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(AgentsFile, errors.New("it could not be read"))
	}
	if len(cs) > 0 {
		if err := addChanges(tx, s.version, cs, now); err != nil {
			return nil, false, err
		}
	}
	list, _ := json.Marshal(append([]string{}, files...))
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('imported', ?), ('imported_files', ?)`,
		now.UTC().Format(time.RFC3339Nano), string(list)); err != nil {
		return nil, false, fmt.Errorf("settings: the import could not be stored: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("settings: the import could not be stored: %w", err)
	}
	return files, true, nil
}

// exists says a file is at path, or a link that points nowhere, which the
// file store reads as a file that cannot be used.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
