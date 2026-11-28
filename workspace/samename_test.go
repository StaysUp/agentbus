package workspace

import "testing"

func TestSameName(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"config.local", "Config.local", true},
		{"café.local", "café.local", true}, // composed and decomposed
		{"CAFÉ.local", "café.local", true},
		{"src/Button.tsx", "SRC/button.tsx", true},
		{"config.local", "Archive.local", false}, // a hard link's other name
		{"config.local", "config.locals", false},
		{"cafe.local", "café.local", false},
		// Unicode's full case folding, as APFS folds (2026-11-27, macOS 13).
		{"straße.local", "STRASSE.local", true},
		{"ﬁle", "FILE", true},
		{"ΣΑΣ", "σας", true},
		{"straße.local", "strase.local", false},
		{"İx", "ix", false}, // APFS keeps these apart too
	} {
		if got := SameName(c.a, c.b); got != c.same {
			t.Errorf("SameName(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}
