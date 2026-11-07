package event

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// An event that cannot be encoded still leaves a line.
func TestJSONLinesNeverDropsAnEvent(t *testing.T) {
	var out bytes.Buffer
	j := NewJSONLines(&out)
	j.Emit(Event{ID: "e1", Kind: ToolRequested, ToolInput: json.RawMessage(`{"path":`)})
	j.Emit(Event{ID: "e2", Kind: TurnCompleted})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), out.String())
	}
	var e Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.ID != "e1" || e.Error == "" {
		t.Errorf("first line = %q", lines[0])
	}
}
