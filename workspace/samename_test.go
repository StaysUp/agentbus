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
	} {
		if got := SameName(c.a, c.b); got != c.same {
			t.Errorf("SameName(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}
