//go:build integration && (linux || darwin)

package connector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/staysup/agentbus/workspace"
)

// The look again measures from when the conflict check began, not from when
// the look again does: a fetch that outlasts Racy leaves a write made as it
// began older than Racy by then, and a look again measured from its own start
// would trust the file's old size, time and mode. These rows let the files
// rest for longer than Racy before the apply, so their change time is old
// enough for Keeps to trust a file's stat without reading it, which no row
// built on agedBy reaches.
func TestApplyMeasuresFromTheStartOfTheCheck(t *testing.T) {
	racy := workspace.Racy + 300*time.Millisecond
	for _, tc := range []struct {
		name string
		// stat: the base saw the file by its stat alone, with no hash.
		stat bool
		// fetch is how long the fetch goes on after the person writes.
		fetch     time.Duration
		person    func(t *testing.T, folder string)
		conflicts []ConflictFile
		want      string
	}{
		{
			name: "a write with the time put back, early in a slow fetch", fetch: racy,
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "own\n") },
			conflicts: []ConflictFile{{"a.txt", WhyChanged}}, want: "own\n",
		},
		{
			name:      "a write with the time put back, to a file not touched for longer than Racy",
			person:    func(t *testing.T, f string) { putBack(t, filepath.Join(f, "a.txt"), "own\n") },
			conflicts: []ConflictFile{{"a.txt", WhyChanged}}, want: "own\n",
		},
		{
			name:   "a file not touched for longer than Racy",
			person: func(*testing.T, string) {}, want: "agent\n",
		},
		{
			name:   "a file not touched for longer than Racy, its time moved and put back",
			person: touchAndPutBack, want: "agent\n",
		},
		{
			name: "a file the base saw by its stat alone, not touched for longer than Racy", stat: true,
			person: func(*testing.T, string) {}, want: "agent\n",
		},
		{
			// Kept on purpose: the base left no hash, so a file whose change time
			// moved since the check cannot be shown to be as it was. It refuses,
			// and running the apply again clears it (a chmod and back, a backup
			// tool's touch). The old look again, which went by the stat, let it by.
			name: "a file the base saw by its stat alone, its mode changed and put back", stat: true,
			person: func(t *testing.T, f string) {
				p := filepath.Join(f, "a.txt")
				must(t, os.Chmod(p, 0o755))
				must(t, os.Chmod(p, 0o644))
			},
			conflicts: []ConflictFile{{"a.txt", WhyChanged}}, want: "one\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			folder := t.TempDir()
			write(t, folder, map[string]string{"a.txt": "one\n"})
			agedBy(t, folder, -time.Hour)
			time.Sleep(racy)
			base := fileOf("one\n", 0o644)
			if tc.stat {
				base = statOf(t, folder, "a.txt")
			}
			cs := workspace.Changeset{Space: "s", Checkpoint: "t1", BaseTaken: time.Now().UnixNano(), Files: []workspace.FileChange{
				{Path: "a.txt", Base: base, Now: fileOf("agent\n", 0o644)},
			}}
			after := shot(t, folder)
			got, err := applyWhile(t, folder, cs, []string{"agent\n"}, "agent\n", func() {
				tc.person(t, folder)
				after = shot(t, folder)
				time.Sleep(tc.fetch)
			})
			if tc.conflicts != nil {
				refused(t, got, err, tc.conflicts)
				untouched(t, folder, after)
			} else if err != nil {
				t.Fatalf("apply refused: %v", err)
			}
			if g := text(t, filepath.Join(folder, "a.txt")); g != tc.want {
				t.Errorf("a.txt holds %q, want %q", g, tc.want)
			}
		})
	}
}
