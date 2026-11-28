package isolation

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// The cost of a checkpoint of a committed repo of 20,000 files, one file
// edited a turn. Run with AGENTBUS_MEASURE=1; it prints the median of five.
func TestCheckpointCostOf20kFiles(t *testing.T) {
	if os.Getenv("AGENTBUS_MEASURE") == "" {
		t.Skip("set AGENTBUS_MEASURE=1 to measure")
	}
	src := t.TempDir()
	files := map[string]string{".gitignore": "*.local\n"}
	for d := 0; d < 200; d++ {
		for f := 0; f < 100; f++ {
			files[fmt.Sprintf("src/d%03d/f%03d.go", d, f)] = fmt.Sprintf("package d%d // %d\n", d, f)
		}
	}
	commitIn(t, src, files)
	b, s := open(t, Copy, src)
	var took []time.Duration
	for i := 0; i < 5; i++ {
		fixture(t, s.Dir, map[string]string{fmt.Sprintf("src/d%03d/f000.go", i): fmt.Sprintf("package x // turn %d\n", i)})
		start := time.Now()
		if _, err := b.Checkpoint(s, fmt.Sprintf("t%d", i)); err != nil {
			t.Fatal(err)
		}
		took = append(took, time.Since(start))
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	n, _ := filepath.Glob(filepath.Join(s.Dir, "src", "*", "*.go"))
	t.Logf("checkpoint of %d files, one edit: median %v (min %v, max %v)", len(n), took[2].Round(time.Millisecond), took[0].Round(time.Millisecond), took[4].Round(time.Millisecond))
}
