package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/body"
)

// TestStoreGrowthSample measures what ten calls of one session add to the
// store, with and without dedupe. It runs only with AGENTBUS_SIZE_SAMPLE set
// to a recorded Anthropic request body (system, tools, messages). Each later
// call repeats the conversation and adds one tool call and its ~3 KB result,
// as a working Claude Code session does.
func TestStoreGrowthSample(t *testing.T) {
	path := os.Getenv("AGENTBUS_SIZE_SAMPLE")
	if path == "" {
		t.Skip("AGENTBUS_SIZE_SAMPLE is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sample map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sample); err != nil {
		t.Fatal(err)
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(sample["messages"], &msgs); err != nil || len(msgs) == 0 {
		t.Fatalf("no messages: %v", err)
	}
	first, rest := msgs[0], msgs[1:]

	db := filepath.Join(t.TempDir(), "events.db")
	s := open(t, db)
	before := fileSize(t, s, db)
	conv := []json.RawMessage{first}
	sum := 0
	for k := 1; k <= 10; k++ {
		if k > 1 {
			id := fmt.Sprintf("toolu_%02d", k)
			use, _ := json.Marshal(map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": id, "name": "Read", "input": map[string]string{"file_path": fmt.Sprintf("/src/f%d.go", k)}}}})
			out := strings.Repeat(fmt.Sprintf("line %d of file %d: some Go code here\n", k, k), 80)
			res, _ := json.Marshal(map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": id, "content": out}}})
			conv = append(conv, use, res)
		}
		all := append(append([]json.RawMessage{}, conv...), rest...)
		m := map[string]any{"model": sample["model"], "max_tokens": 32000, "stream": true,
			"system": sample["system"], "tools": sample["tools"], "messages": all}
		req, _ := json.Marshal(m)
		sum += len(req)
		split, err := anthropic.Adapter{}.Split(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.KeepCall(body.Call{Exchange: fmt.Sprintf("x-g-%d", k), Session: "s1", Vendor: "anthropic",
			Time: time.Now(), Size: len(req), Request: split}); err != nil {
			t.Fatal(err)
		}
	}
	var pieces, rows int
	if err := s.r.QueryRow(`SELECT COALESCE(SUM(size),0) FROM pieces`).Scan(&pieces); err != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRow(`SELECT COALESCE(SUM(LENGTH(top)+LENGTH(pieces)),0) FROM calls`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	after := fileSize(t, s, db)
	t.Logf("one sample request: %d bytes", len(raw))
	t.Logf("10 calls, request bodies summed (no dedupe): %d bytes, %d per call", sum, sum/10)
	t.Logf("10 calls, unique piece bytes %d + call rows %d = %d bytes, %d per call", pieces, rows, pieces+rows, (pieces+rows)/10)
	t.Logf("10 calls, store file grew %d bytes, %d per call", after-before, (after-before)/10)
}

// fileSize checkpoints the WAL into the file and returns the file's size.
func fileSize(t *testing.T, s *Store, db string) int {
	t.Helper()
	if _, err := s.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Size())
}
