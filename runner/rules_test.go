package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// Preparing a turn's copy reads the settings control took for the turn when
// its attempt started, never the settings as they are now: a permissions file
// broken after the start does not refuse the turn, and control's refusal is
// the turn's reason (review t_12c10ac1e9371bd9).
func TestPrepareUsesTheAttemptsRules(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, settings.PolicyFile), []byte("this is [not toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No workdir: a preparation that gets past the rules stops at the copy.
	r := &Runner{cfg: Config{Settings: settings.NewFiles(data), Logf: t.Logf}}
	spec := queue.Spec{Harness: "claude", Folder: t.TempDir(), Isolate: true, Prompt: "hi"}
	control := &settings.RunnerSettings{Permissions: settings.DefaultPolicy(), PermissionsVersion: "v-start",
		Instructions: settings.DefaultInstructions}

	at := &attempt{job: queue.Job{ID: "j1", Kind: queue.KindTurn, Spec: spec}, settings: control}
	res := r.prepare(context.Background(), at)
	if res == nil || !strings.Contains(res.Refused, "no workdir") {
		t.Fatalf("prepare under the attempt's rules: %+v, want it past the rules", res)
	}

	at = &attempt{job: queue.Job{ID: "j2", Kind: queue.KindTurn, Spec: spec},
		rulesErr: &settings.Error{Setting: "permissions", Where: "the rules this turn started under", Why: "control no longer keeps them", Fix: "Start the turn again"}}
	res = r.prepare(context.Background(), at)
	if res == nil || !strings.Contains(res.Refused, "the rules this turn started under") {
		t.Fatalf("prepare with control's refusal: %+v", res)
	}
}
