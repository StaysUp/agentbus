package workspace

import (
	"strings"
	"testing"
)

func TestHasMergeMarkers(t *testing.T) {
	for in, want := range map[string]bool{
		"<<<<<<< this session\nx\n=======\ny\n>>>>>>> your folder\n":           true,
		"<<<<<<< this session\r\nx\r\n=======\r\ny\r\n>>>>>>> your folder\r\n": true,
		"<<<<<<< this session\n=======\n>>>>>>> your folder":                   true,
		// git's own markers, as a doc or a fixture shows them: not ours.
		"<<<<<<< HEAD\nx\n=======\ny\n>>>>>>> main\n":            false,
		"x <<<<<<< this session\n=======\n>>>>>>> your folder\n": false,
		"<<<<<<< this session\nx\n>>>>>>> your folder\n":         false,
		"": false,
	} {
		if got, err := HasMergeMarkers(strings.NewReader(in)); err != nil || got != want {
			t.Errorf("HasMergeMarkers(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// A file far larger than the read buffer, with lines longer than it, is read
// as a stream: markers past them are found, and a long line's middle that
// happens to hold a marker's text is not taken as a line start.
func TestHasMergeMarkersLarge(t *testing.T) {
	long := strings.Repeat("a", 200<<10)
	trap := long + "\n" + strings.Repeat("b", 70<<10) + "<<<<<<< this session\n=======\n>>>>>>> your folder-ish\n"
	if got, err := HasMergeMarkers(strings.NewReader(trap)); err != nil || got {
		t.Fatalf("a marker inside a long line counted: %v %v", got, err)
	}
	real := long + "\n<<<<<<< this session\nx\n=======\ny\n>>>>>>> your folder\n"
	if got, err := HasMergeMarkers(strings.NewReader(real)); err != nil || !got {
		t.Fatalf("markers after a long line missed: %v %v", got, err)
	}
}
