package settings

import (
	"path/filepath"
	"testing"
)

// The database keeps the rules each turn took, by their version, so a turn's
// later requests are decided by them after the policy has changed (O2.4).
func TestRulesKeptByVersion(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), DBFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, v1, err := db.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if err := Keep(db, first, v1); err != nil {
		t.Fatal(err)
	}
	changed := first
	yes := true
	changed.AgentAPI.Apply = &yes
	if _, _, err := db.SetPolicy(changed, Any); err != nil {
		t.Fatal(err)
	}
	got, found, err := RulesOf(db, v1)
	if err != nil || !found {
		t.Fatalf("the rules a turn took: %v %v", found, err)
	}
	if got.AgentAPI.Apply == nil || *got.AgentAPI.Apply {
		t.Errorf("the kept rules took the later change: %v", got.AgentAPI.Apply)
	}
	// Kept once: keeping the same version again changes nothing.
	if err := Keep(db, changed, v1); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := RulesOf(db, v1); again.AgentAPI.Apply == nil || *again.AgentAPI.Apply {
		t.Errorf("a version kept twice moved")
	}
	// The current policy is found by its version without being kept.
	_, v2, _ := db.Policy()
	if _, found, err := RulesOf(db, v2); err != nil || !found {
		t.Errorf("the current policy by its version: %v %v", found, err)
	}
	if _, found, err := RulesOf(db, "never"); err != nil || found {
		t.Errorf("an unknown version: %v %v", found, err)
	}
}
