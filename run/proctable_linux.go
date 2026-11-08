//go:build linux

package run

import (
	"os"
	"strconv"
)

// readProcs reads Linux's process table from /proc/<pid>/stat. Start times
// are clock ticks after boot. Session ids cost nothing extra here, so every
// process has one; minStart is not needed.
func readProcs(minStart int64) ([]proc, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]proc, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // it exited since the listing
		}
		ppid, sid, start, ok := parseStat(string(b))
		if !ok {
			continue
		}
		out = append(out, proc{pid: pid, ppid: ppid, sid: sid, start: start})
	}
	return out, nil
}
