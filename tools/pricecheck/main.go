// Command pricecheck reports where the built-in price table (cost/table.go)
// and a LiteLLM price file disagree. It is read-only: it prints the
// differences and never rewrites the table.
//
//	go run ./tools/pricecheck                  # fetch LiteLLM's current file
//	go run ./tools/pricecheck -file prices.json
//
// It prints one line per rate that differs and nothing when every shared
// rate matches. It exits 0 when nothing differs, 1 when something does and 2
// when the file cannot be read or the command line is wrong. See
// tools/pricecheck/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/staysup/agentbus/cost"
)

// liteLLMURL is LiteLLM's price file on its main branch.
const liteLLMURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pricecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "read this LiteLLM price file instead of fetching one")
	url := fs.String("url", liteLLMURL, "fetch the LiteLLM price file from this URL")
	missing := fs.Bool("missing", false, "also list built-in models the file has no entry for")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// A path given without -file would otherwise be ignored, and the live
	// file checked in its place.
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "pricecheck: unexpected argument %q; to read a price file, use -file %s\n", fs.Arg(0), fs.Arg(0))
		return 2
	}

	path := *file
	if path == "" {
		dir, err := os.MkdirTemp("", "pricecheck")
		if err != nil {
			fmt.Fprintln(stderr, "pricecheck:", err)
			return 2
		}
		defer os.RemoveAll(dir)
		path = filepath.Join(dir, "prices.json")
		if err := fetch(*url, path); err != nil {
			fmt.Fprintln(stderr, "pricecheck:", err)
			return 2
		}
	}
	theirs, err := cost.LoadLiteLLM(path)
	if err != nil {
		fmt.Fprintln(stderr, "pricecheck:", err)
		return 2
	}

	drift, absent := cost.Default().Diff(theirs)
	if len(drift) > 0 {
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "MODEL\tRATE\tBUILT-IN\tLITELLM")
		for _, d := range drift {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Model, d.Rate, num(d.Have), num(d.Other))
		}
		w.Flush()
	}
	if *missing {
		for _, m := range absent {
			fmt.Fprintf(stdout, "not in the file: %s\n", m)
		}
	}
	if len(drift) > 0 {
		return 1
	}
	return 0
}

// num prints a rate without the noise of scaling a per-token price to per
// million tokens (2.9999999999999996 prints as 3).
func num(f float64) string {
	return strconv.FormatFloat(f, 'g', 10, 64)
}

func fetch(url, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch price file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch price file: %s from %s", resp.Status, url)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	return errors.Join(err, f.Close())
}
