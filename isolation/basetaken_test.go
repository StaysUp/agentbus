package isolation

import (
	"runtime"
	"testing"
	"time"
)

// A copy made from the person's folder records when it began reading it, its
// changesets carry that time, and a reloaded space keeps it: the folder side
// trusts a Base entry without a hash only against it (connector asBase).
// Before, the changeset had no such time.
func TestChangesetCarriesWhenTheBaseRead(t *testing.T) {
	backends := []string{Copy}
	if runtime.GOOS == "linux" && OverlayWhy(t.TempDir()) == "" {
		backends = append(backends, Overlay)
	}
	for _, name := range backends {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			fixture(t, src, map[string]string{"a.txt": "a1"})
			before := time.Now().UnixNano()
			b, s := open(t, name, src)
			after := time.Now().UnixNano()
			fixture(t, s.Dir, map[string]string{"a.txt": "a2"})
			if _, err := b.Checkpoint(s, "t1"); err != nil {
				t.Fatal(err)
			}
			again, err := Load(s.Workdir, s.Root)
			if err != nil {
				t.Fatal(err)
			}
			cs, err := ChangesetOf(again, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if cs.BaseTaken < before || cs.BaseTaken > after {
				t.Fatalf("base_taken %d, want between %d and %d", cs.BaseTaken, before, after)
			}
		})
	}
}
