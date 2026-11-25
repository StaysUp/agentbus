package workspace

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// The labels an update's merge (isolation, mode merge) gives the two sides of
// a conflict, so its markers read "<<<<<<< this session" and ">>>>>>> your
// folder". They are AgentBus's own, which is what lets an apply tell a file
// the merge left unresolved from one that merely shows git's markers (a doc
// about git, a test fixture): content, not a list of paths, so a rename or a
// later update cannot lose track of it.
const (
	MergeOurs   = "this session"
	MergeTheirs = "your folder"
)

var (
	markOpen  = []byte("<<<<<<< " + MergeOurs)
	markMid   = []byte("=======")
	markClose = []byte(">>>>>>> " + MergeTheirs)
)

// HasMergeMarkers reports whether r holds a conflict an update's merge left:
// a line "<<<<<<< this session", a later "=======" line and a later line
// ">>>>>>> your folder", in that order (a trailing \r allowed). It reads r as
// a stream, so a file of any size is checked without holding it.
func HasMergeMarkers(r io.Reader) (bool, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	stage := 0
	start := true // the next byte read begins a line
	for {
		chunk, err := br.ReadSlice('\n')
		if start && len(chunk) > 0 {
			line := bytes.TrimSuffix(bytes.TrimSuffix(chunk, []byte("\n")), []byte("\r"))
			switch {
			case stage == 0 && bytes.Equal(line, markOpen):
				stage = 1
			case stage == 1 && bytes.Equal(line, markMid):
				stage = 2
			case stage == 2 && bytes.Equal(line, markClose):
				return true, nil
			}
		}
		// A line longer than the buffer comes in pieces: only its first piece
		// starts a line.
		start = len(chunk) > 0 && chunk[len(chunk)-1] == '\n'
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return false, nil
		default:
			return false, err
		}
	}
}
