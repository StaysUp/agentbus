package wall

import (
	"encoding/json"
	"strings"
	"testing"
)

// Step 1 builds no level, so every walled job is refused.
func TestBuildsNoLevelYet(t *testing.T) {
	for _, l := range []string{"copy", "sandbox", "read_only", ""} {
		if Builds(l) {
			t.Fatalf("Builds(%q) in step 1", l)
		}
	}
}

// The probe's settings allow writes to one folder and no host. srt 0.0.83
// refuses a settings file without both domain lists.
func TestProbeSettings(t *testing.T) {
	b, err := json.Marshal(probeSettings("/w/in"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"filesystem":{"allowWrite":["/w/in"],"denyRead":[],"denyWrite":[]},"network":{"allowedDomains":[],"deniedDomains":[]}}`
	if string(b) != want {
		t.Fatalf("settings:\n%s\nwant\n%s", b, want)
	}
	if !strings.Contains(NotInstalled, Package+"@"+Version) {
		t.Fatalf("the install hint does not pin the version: %s", NotInstalled)
	}
}
