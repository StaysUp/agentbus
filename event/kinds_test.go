package event

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// constsOf reads the values of every constant of the named type in event.go.
func constsOf(t *testing.T, typ string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "event.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			v := s.(*ast.ValueSpec)
			if id, ok := v.Type.(*ast.Ident); !ok || id.Name != typ {
				continue
			}
			for _, val := range v.Values {
				lit := val.(*ast.BasicLit)
				s, _ := strconv.Unquote(lit.Value)
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no constants of type %s in event.go", typ)
	}
	return out
}

// Kinds and Outcomes list every constant, so the API's description, built
// from them, names every kind and outcome. Every field a kind needs is a field
// of Event.
func TestKindsListEveryKind(t *testing.T) {
	var kinds []string
	for _, k := range Kinds {
		kinds = append(kinds, string(k.Kind))
		if k.Doc == "" {
			t.Errorf("%s has no Doc", k.Kind)
		}
		for _, n := range k.Needs {
			if !slices.Contains(jsonNames(), n) {
				t.Errorf("%s needs %s, which Event does not have", k.Kind, n)
			}
		}
	}
	for _, c := range constsOf(t, "Kind") {
		if !slices.Contains(kinds, c) {
			t.Errorf("Kinds does not list %s", c)
		}
	}
	var outcomes []string
	for _, o := range Outcomes {
		outcomes = append(outcomes, string(o))
	}
	for _, c := range constsOf(t, "Outcome") {
		if !slices.Contains(outcomes, c) {
			t.Errorf("Outcomes does not list %s", c)
		}
	}
}

func jsonNames() []string {
	var names []string
	typ := reflect.TypeFor[Event]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		names = append(names, name)
	}
	return names
}

// turn.changed says its file list can be cut: workspace.Changes.Fit and
// MaxChanges leave files out, and say so in truncated (0.5.1).
func TestTurnChangedSaysTheListCanBeCut(t *testing.T) {
	for _, k := range Kinds {
		if k.Kind != TurnChanged {
			continue
		}
		if strings.Contains(k.Doc, "every changed file") || !strings.Contains(k.Doc, "truncated says it is not complete") {
			t.Errorf("turn.changed doc does not say the list can be cut: %q", k.Doc)
		}
		return
	}
	t.Fatal("turn.changed is not in Kinds")
}
