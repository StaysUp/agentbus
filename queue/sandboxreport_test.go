package queue

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A reason past MaxSandboxWhy is cut on a character's edge, so the report
// still passes Capabilities' check and the runner enrolls. CI on Ubuntu
// 24.04 found a runner refused at enroll (400) for a long AppArmor reason.
func TestNewSandboxReportCuts(t *testing.T) {
	short := NewSandboxReport(false, "bubblewrap is not on this runner")
	if short.Why != "bubblewrap is not on this runner" || short.Can {
		t.Fatalf("a short reason changed: %+v", short)
	}
	for _, long := range []string{strings.Repeat("a", 900), strings.Repeat("ü", 400), strings.Repeat("a", 499) + "üü"} {
		r := NewSandboxReport(false, long)
		if len(r.Why) > MaxSandboxWhy || !utf8.ValidString(r.Why) || !strings.HasSuffix(r.Why, "…") {
			t.Errorf("cut to %d bytes, valid %v: %q", len(r.Why), utf8.ValidString(r.Why), r.Why[max(0, len(r.Why)-10):])
		}
		c := Capabilities{OS: "linux", Arch: "amd64", Sandbox: r}
		if why := c.Check(); why != "" {
			t.Errorf("the cut report is refused: %s", why)
		}
	}
}
