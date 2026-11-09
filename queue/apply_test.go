package queue

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// An apply's key holds a "/", as no caller's key may, so no other job can
// take it first.
func TestApplyKeyIsReserved(t *testing.T) {
	if k := ApplyKey("t_1", 0); !strings.Contains(k, "/") || !strings.Contains(ResultKey("job_1"), "/") {
		t.Fatalf("apply key %q", k)
	}
}

// A result with many files fits its budget, keeps the whole count, and says
// it was cut.
func TestApplyResultFit(t *testing.T) {
	a := ApplyResult{}
	for i := range 9000 {
		a.Files = append(a.Files, fmt.Sprintf("folder/with/a/long/name/file-%05d.txt", i))
	}
	a.Count = len(a.Files)
	a.Fit(64 << 10)
	b, _ := json.Marshal(a)
	if len(b) > 64<<10 || !a.Truncated || a.Count != 9000 || len(a.Files) == 0 {
		t.Fatalf("%d bytes, truncated %v, count %d, files %d", len(b), a.Truncated, a.Count, len(a.Files))
	}
	small := ApplyResult{Files: []string{"a.txt"}, Count: 1}
	small.Fit(64 << 10)
	if small.Truncated || len(small.Files) != 1 {
		t.Fatalf("a small result was cut: %+v", small)
	}
}
