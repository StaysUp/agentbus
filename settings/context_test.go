package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fptr(f float64) *float64 { return &f }

// TestCompactionDefaultsAndSource: the built-in defaults are the researched
// numbers, and the source says which are the vendor's own and which a stated
// default, so the view never shows an invented number as the vendor's.
func TestCompactionDefaultsAndSource(t *testing.T) {
	d := DefaultCompaction()
	for _, c := range []struct {
		harness string
		want    float64
		source  string
	}{
		{"opencode", 0.85, SourceVendor},
		{"gemini", 0.70, SourceVendor},
		{"claude", 0.80, SourceDefault},
		{"codex", 0.80, SourceDefault},
	} {
		got, ok := d.Point(c.harness)
		if !ok || got != c.want {
			t.Errorf("%s default = %v (ok %v), want %v", c.harness, got, ok, c.want)
		}
		if s := CompactionSource(c.harness); s != c.source {
			t.Errorf("%s source = %q, want %q", c.harness, s, c.source)
		}
		if !d.IsDefault(c.harness) {
			t.Errorf("%s: fresh defaults should read as default", c.harness)
		}
	}
	// An unknown harness has no point and no source.
	if _, ok := d.Point("nope"); ok {
		t.Error("unknown harness has a point")
	}
	if CompactionSource("nope") != "" {
		t.Error("unknown harness has a source")
	}
}

// TestCompactionCheck: a share must be in (0, 1]; anything else names its
// vendor.
func TestCompactionCheck(t *testing.T) {
	for _, bad := range []Compaction{
		{Claude: fptr(0)}, {Codex: fptr(-0.1)}, {Opencode: fptr(1.5)},
	} {
		if err := bad.Check(); err == nil {
			t.Errorf("%+v passed Check", bad)
		}
	}
	if err := (Compaction{Claude: fptr(0.5), Gemini: fptr(1)}).Check(); err != nil {
		t.Errorf("a valid compaction was refused: %v", err)
	}
}

// TestCompactionDBRoundTrip: a changed point is stored, read back filled, moves
// the version, keeps only what differs from the default, and reads as not the
// default. An untouched vendor keeps its default and reads as default.
func TestCompactionDBRoundTrip(t *testing.T) {
	d := openTestDB(t, t.TempDir())
	c0, v0, err := d.Compaction()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c0.Point("claude"); got != 0.80 {
		t.Fatalf("fresh claude = %v, want 0.8", got)
	}

	c1, v1, err := d.SetCompaction(Compaction{Claude: fptr(0.6)}, v0)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v0 {
		t.Error("version did not move on a change")
	}
	if got, _ := c1.Point("claude"); got != 0.6 {
		t.Errorf("claude = %v, want 0.6", got)
	}
	// opencode left out keeps its default and still reads as default.
	if got, _ := c1.Point("opencode"); got != 0.85 || !c1.IsDefault("opencode") {
		t.Errorf("opencode = %v default %v, want 0.85 default", got, c1.IsDefault("opencode"))
	}
	if c1.IsDefault("claude") {
		t.Error("a set claude should not read as the default")
	}

	// Only the changed vendor is stored, so a later default shift reaches the
	// rest.
	raw := stored(t, d, SettingCompaction)
	if !strings.Contains(raw, "claude") || strings.Contains(raw, "opencode") {
		t.Errorf("stored row = %q, want only claude", raw)
	}
}

// TestCompactionKeepsOnlyChanged: storing then reading back after a default
// shift would reach an untouched vendor. Checked by inspecting onlyChanged.
func TestCompactionKeepsOnlyChanged(t *testing.T) {
	c := DefaultCompaction()
	c.Claude = fptr(0.6)
	only := c.onlyChanged()
	if only.Claude == nil || *only.Claude != 0.6 {
		t.Errorf("claude not kept: %+v", only)
	}
	if only.Codex != nil || only.Opencode != nil || only.Gemini != nil {
		t.Errorf("unchanged vendors were kept: %+v", only)
	}
}

// TestCompactionIfMatch: a stale If-Match is refused.
func TestCompactionIfMatch(t *testing.T) {
	d := openTestDB(t, t.TempDir())
	_, v0, _ := d.Compaction()
	if _, _, err := d.SetCompaction(Compaction{Codex: fptr(0.7)}, "not-the-version"); err != ErrChanged {
		t.Errorf("stale match err = %v, want ErrChanged", err)
	}
	if _, _, err := d.SetCompaction(Compaction{Codex: fptr(0.7)}, v0); err != nil {
		t.Errorf("fresh match refused: %v", err)
	}
}

// A runner's own replay reads the history budget from its server (Remote),
// as control's reads its database: every runner keeps the person's budget.
// A server that sends none gives the default; so does one that cannot be read.
func TestBudgetOnEveryStore(t *testing.T) {
	d := openTestDB(t, t.TempDir())
	if got := BudgetOf(d)(); got != 0.4 {
		t.Fatalf("a fresh database's budget = %v, want 0.4", got)
	}
	if _, _, err := d.SetCompaction(Compaction{Budget: fptr(0.3)}, Any); err != nil {
		t.Fatal(err)
	}
	if got := BudgetOf(d)(); got != 0.3 {
		t.Fatalf("the database's budget = %v, want 0.3", got)
	}
	send := 0.25
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RunnerSettings{Permissions: DefaultPolicy(), Budget: send})
	}))
	defer srv.Close()
	r := &Remote{Server: srv.URL, Local: "/data"}
	if got := BudgetOf(r)(); got != 0.4 {
		t.Fatalf("a runner not enrolled yet: %v, want the default", got)
	}
	r.Enrolled("rn_1", "rt_x")
	if got := BudgetOf(r)(); got != 0.25 {
		t.Fatalf("the runner's budget = %v, want the server's 0.25", got)
	}
	send = 0
	if got := BudgetOf(r)(); got != 0.4 {
		t.Fatalf("a server that sends no budget: %v, want the default", got)
	}
}
