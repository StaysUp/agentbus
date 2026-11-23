package toolreg

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// memBackend keeps the overlay in memory, so a second registry built on it
// reads what the first one saved.
type memBackend struct {
	added   []Tool
	removed []string
	saves   int
}

func (m *memBackend) Load() ([]Tool, []string, error) { return m.added, m.removed, nil }
func (m *memBackend) Save(added []Tool, removed []string) error {
	m.added, m.removed, m.saves = added, removed, m.saves+1
	return nil
}

// errBackend fails every save, so a registry can show it leaves its state as
// it was when the overlay cannot be written.
type errBackend struct{}

func (errBackend) Load() ([]Tool, []string, error) { return nil, nil, nil }
func (errBackend) Save([]Tool, []string) error     { return errors.New("disk full") }

// tool is a valid custom tool named name.
func tool(name string) Tool {
	return Tool{
		Name:        name,
		Description: "does a thing for a model",
		Inputs:      map[string]any{"type": "object", "properties": map[string]any{}},
		Where:       WhereControl,
		Run:         Run{Kind: RunAPI, Method: "GET", Path: "/api/v0/turns"},
	}
}

func names(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func withDefaults(t *testing.T, b Backend) *Registry {
	t.Helper()
	r, err := New(b)
	if err != nil {
		t.Fatal(err)
	}
	r.SetDefaults([]Tool{tool("list_turns"), tool("get_turn"), tool("apply_turn")})
	return r
}

// List is the defaults in order, then the added tools by name.
func TestListOrder(t *testing.T) {
	r := withDefaults(t, nil)
	if got := names(r.List()); !reflect.DeepEqual(got, []string{"list_turns", "get_turn", "apply_turn"}) {
		t.Fatalf("defaults: %v", got)
	}
	if _, err := r.Add(tool("zed_tool")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(tool("my_tool")); err != nil {
		t.Fatal(err)
	}
	if got := names(r.List()); !reflect.DeepEqual(got, []string{"list_turns", "get_turn", "apply_turn", "my_tool", "zed_tool"}) {
		t.Fatalf("with custom: %v", got)
	}
}

// Add refuses a name already served and a tool that does not check out.
func TestAddRefuses(t *testing.T) {
	r := withDefaults(t, nil)
	if _, err := r.Add(tool("get_turn")); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate default: %v", err)
	}
	if _, err := r.Add(tool("my_tool")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(tool("my_tool")); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate custom: %v", err)
	}
	var bad *Invalid
	for _, broken := range []Tool{
		func() Tool { x := tool("Bad_Name"); return x }(),
		func() Tool { x := tool("ok_tool"); x.Run.Method = "TRACE"; return x }(),
		func() Tool { x := tool("ok_tool"); x.Inputs = nil; return x }(),
		func() Tool { x := tool("ok_tool"); x.Run.Path = "/elsewhere"; return x }(),
		func() Tool { x := tool("ok_tool"); x.Run.Path = "/api/v0/turns/{id}"; x.Run.IDInput = ""; return x }(),
		// A name longer than a path id can hold: addable but never removable.
		func() Tool { x := tool("a_" + strings.Repeat("b", 63)); return x }(),
		// A property that is not a schema object: a client would choke on it.
		func() Tool {
			x := tool("ok_tool")
			x.Inputs = map[string]any{"type": "object", "properties": map[string]any{"session": 17}}
			return x
		}(),
		// required that is not an array of names.
		func() Tool {
			x := tool("ok_tool")
			x.Inputs = map[string]any{"type": "object", "properties": map[string]any{}, "required": "session"}
			return x
		}(),
		// required names an input that is not a property: the call could never
		// supply it.
		func() Tool {
			x := tool("ok_tool")
			x.Inputs = map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{"limit"}}
			return x
		}(),
		// id_input is not a property: the id could never be supplied.
		func() Tool {
			x := tool("ok_tool")
			x.Run.Path, x.Run.IDInput = "/api/v0/turns/{id}", "turn"
			return x
		}(),
		// a query input is not a property.
		func() Tool {
			x := tool("ok_tool")
			x.Run.Query = []string{"limit"}
			return x
		}(),
	} {
		if _, err := r.Add(broken); !errors.As(err, &bad) {
			t.Errorf("%s: %v, want Invalid", broken.Name, err)
		}
	}
}

// Remove drops a custom tool, marks a default removed, and refuses a name that
// is not served. A removed default can be re-added as a custom tool.
func TestRemoveAndReadd(t *testing.T) {
	r := withDefaults(t, nil)
	if err := r.Remove("no_such"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove missing: %v", err)
	}
	if err := r.Remove("apply_turn"); err != nil {
		t.Fatal(err)
	}
	if got := names(r.List()); !reflect.DeepEqual(got, []string{"list_turns", "get_turn"}) {
		t.Fatalf("after removing a default: %v", got)
	}
	if err := r.Remove("apply_turn"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing it twice: %v", err)
	}
	// Re-added as a custom tool with a changed description: served from the
	// overlay, behaves as any custom tool.
	custom := tool("apply_turn")
	custom.Description = "a custom apply"
	custom.Inputs = map[string]any{"type": "object", "properties": map[string]any{"turn": map[string]any{"type": "string"}}}
	custom.Run = Run{Kind: RunAPI, Method: "POST", Path: "/api/v0/workspaces/{id}/apply", IDInput: "turn", Body: true}
	if _, err := r.Add(custom); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get("apply_turn")
	if err != nil || got.Description != "a custom apply" || got.Run.Method != "POST" {
		t.Fatalf("re-added: %+v %v", got, err)
	}
	if n := names(r.List()); !reflect.DeepEqual(n, []string{"list_turns", "get_turn", "apply_turn"}) {
		t.Fatalf("list after re-add: %v", n)
	}
}

// The overlay is read back: a second registry on the same backend serves what
// the first one changed, and the defaults stay in code.
func TestOverlayPersists(t *testing.T) {
	b := &memBackend{}
	r := withDefaults(t, b)
	if _, err := r.Add(tool("my_tool")); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("get_turn"); err != nil {
		t.Fatal(err)
	}
	// A second registry, same overlay, same code defaults.
	r2 := withDefaults(t, b)
	if got := names(r2.List()); !reflect.DeepEqual(got, []string{"list_turns", "apply_turn", "my_tool"}) {
		t.Fatalf("reloaded: %v", got)
	}
}

// A save that fails leaves the registry as it was, so a caller is not told a
// change landed that was never written.
func TestSaveFailureLeavesStateAlone(t *testing.T) {
	r := withDefaults(t, errBackend{})
	if _, err := r.Add(tool("my_tool")); err == nil {
		t.Fatal("add with a failing backend should error")
	}
	if got := names(r.List()); !reflect.DeepEqual(got, []string{"list_turns", "get_turn", "apply_turn"}) {
		t.Fatalf("add left state changed: %v", got)
	}
	if err := r.Remove("get_turn"); err == nil {
		t.Fatal("remove with a failing backend should error")
	}
	if got := names(r.List()); !reflect.DeepEqual(got, []string{"list_turns", "get_turn", "apply_turn"}) {
		t.Fatalf("remove left state changed: %v", got)
	}
}
