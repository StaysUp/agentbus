//go:build darwin

package api

import "golang.org/x/sys/unix"

// readProcs reads macOS's process table from the kern.proc.all sysctl. Start
// times are microseconds since the epoch. A session id is a getsid call, so
// only processes started at minStart or later get one.
func readProcs(minStart int64) ([]proc, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]proc, 0, len(kps))
	for i := range kps {
		k := &kps[i]
		p := proc{pid: int(k.Proc.P_pid), ppid: int(k.Eproc.Ppid), sid: -1,
			start: int64(k.Proc.P_starttime.Sec)*1e6 + int64(k.Proc.P_starttime.Usec)}
		if p.start >= minStart {
			if sid, err := unix.Getsid(p.pid); err == nil {
				p.sid = sid
			}
		}
		out = append(out, p)
	}
	return out, nil
}
