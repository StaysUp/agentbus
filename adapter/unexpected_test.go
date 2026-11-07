package adapter

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/staysup/agentbus/event"
)

// Values that are not shaped like a vendor name never reach a report, even
// when they look like identifiers.
func TestCleanKeepsOnlyNames(t *testing.T) {
	for _, bad := range []string{
		"PRIVATE_PAYLOAD_123",
		"sk-ant-api03-abcdefghijklmnop",
		"AKIAIOSFODNN7EXAMPLE",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJl",
		"text with spaces",
		"a" + strings.Repeat("b", 48),
	} {
		got := Clean(event.Unexpected{Where: "w", Value: bad, Field: bad})
		if got.Value != notAName || got.Field != notAName || got.Where != "w" {
			t.Errorf("Clean(%q) = %+v", bad, got)
		}
	}
	for _, ok := range []string{"server_tool_use_v2", "response.reasoning_summary_text.delta", "ephemeral_5m_input_tokens", "end_turn"} {
		if got := Clean(event.Unexpected{Where: "w", Value: ok, Field: ok}); got.Value != ok || got.Field != ok {
			t.Errorf("Clean(%q) = %+v", ok, got)
		}
	}
}

// Scanning a usage object reads keys only and stops early, so a huge object
// costs little.
func TestUnknownFieldsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 100000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"field_%d":{"nested":[1,2,3,"%s"]}`, i, strings.Repeat("x", 20))
	}
	b.WriteString("}")
	body := []byte(b.String())
	var out []event.Unexpected
	allocs := testing.AllocsPerRun(3, func() {
		out = UnknownFields(body, "usage", Names("input_tokens"), nil)
	})
	if len(out) != MaxUnexpected {
		t.Errorf("fields = %d, want %d", len(out), MaxUnexpected)
	}
	if allocs > 2000 {
		t.Errorf("allocations = %.0f, want at most 2000", allocs)
	}

	// Every key known, so the report cap never stops the scan: the key bound
	// must.
	b.Reset()
	b.WriteString("{")
	for i := 0; i < 100000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"input_tokens":{"nested":[1,2,3]}`)
	}
	b.WriteString("}")
	body = []byte(b.String())
	allocs = testing.AllocsPerRun(3, func() {
		out = UnknownFields(body, "usage", Names("input_tokens"), nil)
	})
	if len(out) != 0 || allocs > 2000 {
		t.Errorf("fields = %d, allocations = %.0f, want 0 and at most 2000", len(out), allocs)
	}
	// Bytes too: the value-copying scan this replaced cost about 16 MB here.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	UnknownFields(body, "usage", Names("input_tokens"), nil)
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 256<<10 {
		t.Errorf("allocated %d bytes, want at most 256 KiB", n)
	}
}

func TestUnknownFieldsInOrder(t *testing.T) {
	got := UnknownFields([]byte(`{"a":1,"known":{"x":[1,{"y":2}]},"b":"s","c":null}`), "usage", Names("known"), nil)
	want := []event.Unexpected{{Where: "usage", Field: "a"}, {Where: "usage", Field: "b"}, {Where: "usage", Field: "c"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// An untyped block or stream line must not produce a report with no name at
// all.
func TestShapelessReportGetsMissing(t *testing.T) {
	var u Unknowns
	u.Value("response.content[].type", "")
	got := u.Events()
	if len(got) != 1 || got[0].Unexpected == nil || got[0].Unexpected.Value != "(missing)" {
		t.Fatalf("reports = %+v, want one with Value (missing)", got)
	}
}

// Clean runs in the adapter and again in the gateway, so it must give the
// same result twice.
func TestCleanIsIdempotent(t *testing.T) {
	for _, x := range []event.Unexpected{
		{Where: "w", Value: ""},
		{Where: "w", Value: "PRIVATE_PAYLOAD_123"},
		{Where: "w", Value: "new_block"},
		{Where: "w", Field: "Odd Field"},
	} {
		once := Clean(x)
		if twice := Clean(once); twice != once {
			t.Errorf("Clean(%+v): once %+v, twice %+v", x, once, twice)
		}
	}
}
