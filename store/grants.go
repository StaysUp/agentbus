package store

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"
)

// FolderGrant is one folder grant recorded on control: tied to a session or to
// the folder for all sessions.
type FolderGrant struct {
	ID      string    `json:"id"`
	Session string    `json:"session,omitempty"`
	Folder  string    `json:"folder"`
	Created time.Time `json:"created"`
}

func newGrantID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "grt_" + hex.EncodeToString(b[:])
}

// AddFolderGrant writes a folder grant row on control. If session is empty,
// the grant is tied to the folder for every session. Duplicate grants are ignored.
func (s *Store) AddFolderGrant(session, folder string) error {
	folder = filepath.Clean(folder)
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.w.Exec(`
		INSERT INTO folder_grants (id, session, folder, created)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session, folder) DO NOTHING
	`, newGrantID(), session, folder, time.Now().UnixNano())
	return err
}

// IsInside reports whether target is inside folder or matches folder.
func IsInside(target, folder string) bool {
	tClean := filepath.Clean(target)
	fClean := filepath.Clean(folder)
	if fClean == "/" {
		return strings.HasPrefix(tClean, "/")
	}
	if tClean == fClean || strings.HasPrefix(tClean, fClean+string(filepath.Separator)) {
		return true
	}
	// On macOS /var, /tmp, /etc are symlinks under /private.
	canonical := func(p string) string {
		for _, prefix := range []string{"/private/var/", "/private/tmp/", "/private/etc/"} {
			if strings.HasPrefix(p, prefix) {
				return "/" + strings.TrimPrefix(p, "/private/")
			}
		}
		return p
	}
	tCan := canonical(tClean)
	fCan := canonical(fClean)
	if fCan == "/" {
		return strings.HasPrefix(tCan, "/")
	}
	return tCan == fCan || strings.HasPrefix(tCan, fCan+string(filepath.Separator))
}

// MatchingFolderGrant returns the most specific (longest) granted folder for targetPath
// for session (or folder-wide), among the grants made by asOf: a turn holds the
// grants made before it started, and one made during it applies from the next
// turn. A zero asOf is every grant.
func (s *Store) MatchingFolderGrant(session, targetPath string, asOf time.Time) (string, bool, error) {
	target := filepath.Clean(targetPath)
	until := int64(1<<63 - 1)
	if !asOf.IsZero() {
		until = asOf.UnixNano()
	}
	rows, err := s.r.Query(`
		SELECT folder FROM folder_grants
		WHERE (session = ? OR session = '') AND created <= ?
	`, session, until)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()

	var best string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return "", false, err
		}
		fClean := filepath.Clean(f)
		if IsInside(target, fClean) {
			if len(fClean) > len(best) {
				best = fClean
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if best != "" {
		return best, true, nil
	}
	return "", false, nil
}

// HasFolderGrant reports whether targetPath is within a folder granted to
// session or granted folder-wide (session=""), by asOf (MatchingFolderGrant).
func (s *Store) HasFolderGrant(session, targetPath string, asOf time.Time) (bool, error) {
	_, ok, err := s.MatchingFolderGrant(session, targetPath, asOf)
	return ok, err
}

// ListFolderGrants returns all folder grants for session (including folder-wide).
func (s *Store) ListFolderGrants(session string) ([]FolderGrant, error) {
	rows, err := s.r.Query(`
		SELECT id, session, folder, created FROM folder_grants
		WHERE session = ? OR session = ''
		ORDER BY created ASC
	`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var grants []FolderGrant
	for rows.Next() {
		var g FolderGrant
		var created int64
		if err := rows.Scan(&g.ID, &g.Session, &g.Folder, &created); err != nil {
			return nil, err
		}
		g.Created = time.Unix(0, created).UTC()
		grants = append(grants, g)
	}
	return grants, rows.Err()
}
