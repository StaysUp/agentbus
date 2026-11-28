package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/staysup/agentbus/cost"
)

// The tests build their price files from the built-in table, so a change to
// cost/table.go does not break them.
const model = "claude-opus-5-5"

// priceFile writes a LiteLLM file with one entry: model's built-in standard
// rates, per token, with change applied on top, and returns its path.
func priceFile(t *testing.T, change map[string]float64) string {
	t.Helper()
	p, ok := cost.Default().Lookup(cost.Anthropic, model)
	if !ok {
		t.Fatalf("%s is not in the built-in table", model)
	}
	std := p.Tiers[cost.Standard]
	entry := map[string]any{"litellm_provider": "anthropic"}
	for key, class := range map[string]cost.Class{
		"input_cost_per_token":                      cost.Input,
		"output_cost_per_token":                     cost.Output,
		"cache_read_input_token_cost":               cost.CacheRead,
		"cache_creation_input_token_cost":           cost.CacheWrite5m,
		"cache_creation_input_token_cost_above_1hr": cost.CacheWrite1h,
	} {
		if rate, ok := std[class]; ok {
			entry[key] = rate / 1e6
		}
	}
	for k, v := range change {
		entry[k] = v
	}
	data, err := json.Marshal(map[string]any{model: entry})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func builtin(t *testing.T, tier cost.Tier, class cost.Class) float64 {
	t.Helper()
	p, _ := cost.Default().Lookup(cost.Anthropic, model)
	rate, ok := p.Tiers[tier][class]
	if !ok {
		t.Fatalf("%s has no built-in %s %s rate", model, tier, class)
	}
	return rate
}

func check(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestMatchPrintsNothing(t *testing.T) {
	code, out, errOut := check(t, "-file", priceFile(t, nil))
	if code != 0 || out != "" || errOut != "" {
		t.Errorf("code %d, stdout %q, stderr %q; want 0 and no output", code, out, errOut)
	}
}

func TestDriftIsPrinted(t *testing.T) {
	output := builtin(t, cost.Standard, cost.Output)
	batchInput := builtin(t, cost.Batch, cost.Input)
	path := priceFile(t, map[string]float64{
		"output_cost_per_token":        (output + 5) / 1e6,
		"input_cost_per_token_batches": (batchInput + 0.5) / 1e6,
	})
	code, out, _ := check(t, "-file", path)
	if code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	space := regexp.MustCompile(` +`)
	got := strings.Split(space.ReplaceAllString(strings.TrimRight(out, "\n"), " "), "\n")
	want := []string{
		"MODEL RATE BUILT-IN LITELLM",
		"anthropic/" + model + " standard output " + num(output) + " " + num(output+5),
		"anthropic/" + model + " batch input " + num(batchInput) + " " + num(batchInput+0.5),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("stdout:\n%s\nwant (columns collapsed):\n%s", out, strings.Join(want, "\n"))
	}
}

func TestMissingListsAbsentModels(t *testing.T) {
	code, out, _ := check(t, "-file", priceFile(t, nil), "-missing")
	if code != 0 {
		t.Errorf("code %d, want 0: a missing model is not drift", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if n := len(cost.Default().Models()) - 1; len(lines) != n {
		t.Errorf("%d lines, want one per built-in model but %s (%d):\n%s", len(lines), model, n, out)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "not in the file: ") || strings.HasSuffix(l, "/"+model) {
			t.Errorf("line %q", l)
		}
	}
}

func TestFetch(t *testing.T) {
	body, err := os.ReadFile(priceFile(t, map[string]float64{"output_cost_per_token": 1}))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prices.json" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	if code, out, _ := check(t, "-url", srv.URL+"/prices.json"); code != 1 || !strings.Contains(out, "standard output") {
		t.Errorf("fetched file: code %d, stdout %q; want 1 and the drift", code, out)
	}
	if code, _, errOut := check(t, "-url", srv.URL+"/gone.json"); code != 2 || !strings.Contains(errOut, "404") {
		t.Errorf("404: code %d, stderr %q; want 2 and the status", code, errOut)
	}
}

func TestUnreadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.json")
	if code, _, errOut := check(t, "-file", missing); code != 2 || errOut == "" {
		t.Errorf("code %d, stderr %q; want 2 and an error", code, errOut)
	}
}

// A path given without -file is refused, not ignored in favour of the live file.
func TestPositionalArgumentIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fetched %s; want no fetch", r.URL)
	}))
	defer srv.Close()

	path := priceFile(t, nil)
	code, out, errOut := check(t, "-url", srv.URL, path)
	if code != 2 || out != "" || !strings.Contains(errOut, "-file") || !strings.Contains(errOut, path) {
		t.Errorf("code %d, stdout %q, stderr %q; want 2, no output and a hint to use -file", code, out, errOut)
	}
}

func TestNum(t *testing.T) {
	// A per-token price scaled back to per million prints as written.
	if got := num(3e-06 * 1e6); got != "3" {
		t.Errorf("num = %q, want 3", got)
	}
	if got := num(0.25); got != strconv.FormatFloat(0.25, 'g', -1, 64) {
		t.Errorf("num(0.25) = %q", got)
	}
}
