//go:build integration

package main

// The launch golden pins what each harness's child is started with: its
// arguments, its environment, its folder, how the turn continues its session,
// the resume id read back from its output, and the refusals. It drives the
// engine through the service's own Wire, so it pins the whole path from a
// request to the child, whichever package holds each part.
// Run with -update-launch to rewrite testdata/launch.golden.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/staysup/agentbus/event"
	engine "github.com/staysup/agentbus/run"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

var updateLaunch = flag.Bool("update-launch", false, "rewrite testdata/launch.golden")

// launchDirEnv, set, makes the test binary a fake agent that records how it
// was started (fakeLaunch).
const launchDirEnv = "AGENTBUS_LAUNCH_DIR"

// fakeLaunch writes its arguments, environment and folder to the next free
// <dir>/<n>.json, prints a resume id in both harnesses' shapes, and exits 0.
func fakeLaunch(dir string) {
	n := 0
	for ; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, strconv.Itoa(n)+".json")); err != nil {
			break
		}
	}
	cwd, _ := os.Getwd()
	b, _ := json.Marshal(fakeReport{Args: os.Args[1:], Env: os.Environ(), Cwd: cwd})
	p := filepath.Join(dir, strconv.Itoa(n)+".json")
	os.WriteFile(p+".tmp", b, 0o600)
	os.Rename(p+".tmp", p)
	fmt.Printf(`{"type":"thread.started","thread_id":"tid-%d"}`+"\n", n)
	fmt.Printf(`{"type":"result","session_id":"sid-%d"}`+"\n", n)
	os.Exit(0)
}

type launchCase struct {
	name        string
	perm        string   // permissions.toml; "" for none
	instr       *string  // instructions.md; nil for none
	env         []string // more of the service's environment
	missing     bool     // no binary on PATH
	replay      bool     // own replay on (a stub ReplayBase)
	oddFolder   bool     // a folder with a comma in its name
	dataInside  bool     // the turn's folder holds the data folder
	codexConfig bool     // developer_instructions in ~/.codex/config.toml
	turns       []engine.Request
}

func strp(s string) *string { return &s }

func TestLaunchGolden(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	first := func(h string) engine.Request {
		return engine.Request{Harness: h, NewSession: "s1", Turn: "t_1", Prompt: "one"}
	}
	next := func(turn string) engine.Request {
		return engine.Request{Session: "s1", Turn: turn, Prompt: "next " + turn}
	}
	with := func(r engine.Request, f func(*engine.Request)) engine.Request { f(&r); return r }
	or := func(r *engine.Request) { r.Access, r.KeyEnv = "openrouter", "OPENROUTER_API_KEY" }
	lists := "read_folders = [\"<readfolder>\"]\nallow = [\"go test\", \"git commit\"]\nforbid = [\"git push\", \"rm -rf\"]\n"
	var cases []launchCase
	for _, h := range []string{"claude", "codex"} {
		cases = append(cases,
			launchCase{name: h + " subscription, then resume", turns: []engine.Request{first(h), next("t_2")}},
			launchCase{name: h + " read-only", turns: []engine.Request{with(first(h), func(r *engine.Request) { r.ReadOnly = true })}},
			launchCase{name: h + " edit none", perm: "edit = \"none\"\n", turns: []engine.Request{first(h)}},
			launchCase{name: h + " lists, model, own instructions", perm: lists, instr: strp("Be brief in {session}.\n"),
				turns: []engine.Request{with(first(h), func(r *engine.Request) { r.Model = "opus" })}},
			launchCase{name: h + " read-only with lists", perm: lists,
				turns: []engine.Request{with(first(h), func(r *engine.Request) { r.ReadOnly = true })}},
			launchCase{name: h + " empty instructions", instr: strp(""), turns: []engine.Request{first(h)}},
			launchCase{name: h + " openrouter", env: []string{"OPENROUTER_API_KEY=test-key"},
				turns: []engine.Request{with(first(h), func(r *engine.Request) { or(r); r.Model = "deepseek/deepseek-v4.1-flash" })}},
			launchCase{name: h + " openrouter without a model", env: []string{"OPENROUTER_API_KEY=test-key"},
				turns: []engine.Request{with(first(h), or)}},
			launchCase{name: h + " openrouter, key not set", turns: []engine.Request{with(first(h), or)}},
			launchCase{name: h + " own endpoint", env: []string{"ANTHROPIC_BASE_URL=http://elsewhere", "OPENAI_BASE_URL=http://elsewhere"},
				turns: []engine.Request{first(h)}},
			launchCase{name: h + " missing", missing: true, turns: []engine.Request{first(h)}},
			launchCase{name: h + " odd folder", oddFolder: true, turns: []engine.Request{first(h)}},
			launchCase{name: h + " folder holds the data folder", dataInside: true, turns: []engine.Request{first(h)}},
			launchCase{name: h + " codex config sets developer_instructions", codexConfig: true, turns: []engine.Request{first(h)}},
			launchCase{name: h + " replay", replay: true, turns: []engine.Request{first(h), next("t_2")}},
			launchCase{name: h + " openrouter replay", replay: true, env: []string{"OPENROUTER_API_KEY=test-key"},
				turns: []engine.Request{with(first(h), func(r *engine.Request) { or(r); r.Model = "m/x" }), next("t_2")}},
			launchCase{name: h + " access unknown", turns: []engine.Request{with(first(h), func(r *engine.Request) { r.Access = "api" })}},
			launchCase{name: h + " subagents off, then resume", perm: "subagents = false\n", turns: []engine.Request{first(h), next("t_2")}},
			launchCase{name: h + " subagents off, read-only, with lists", perm: lists + "subagents = false\n",
				turns: []engine.Request{with(first(h), func(r *engine.Request) { r.ReadOnly = true })}},
			launchCase{name: h + " subagents on, said", perm: "subagents = true\n", turns: []engine.Request{first(h)}},
		)
	}
	cases = append(cases,
		launchCase{name: "switch claude to codex, replay", replay: true,
			turns: []engine.Request{first("claude"), with(next("t_2"), func(r *engine.Request) { r.Harness = "codex" })}},
		launchCase{name: "switch codex to claude, resume",
			turns: []engine.Request{first("codex"), with(next("t_2"), func(r *engine.Request) { r.Harness = "claude"; r.AgentModel = "sonnet" })}},
		launchCase{name: "unknown harness", turns: []engine.Request{first("opencode")}},
		launchCase{name: "unknown harness on a next turn", turns: []engine.Request{first("claude"),
			with(next("t_2"), func(r *engine.Request) { r.Harness = "opencode" })}},
	)

	var out strings.Builder
	for _, c := range cases {
		out.WriteString(runLaunchCase(t, self, c))
	}
	golden := filepath.Join("testdata", "launch.golden")
	if *updateLaunch {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update-launch to write it)", err)
	}
	if got := out.String(); got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("launch golden differs at line %d:\n got %s\nwant %s", i+1, g, w)
			}
		}
	}
}

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var tokenValue = regexp.MustCompile(`AGENTBUS_TOKEN=\S+`)

func runLaunchCase(t *testing.T, self string, c launchCase) string {
	t.Helper()
	home, launch, readFolder := realTemp(t), realTemp(t), realTemp(t)
	folder := realTemp(t)
	data := filepath.Join(realTemp(t), "data")
	if c.dataInside {
		folder = filepath.Dir(data)
	}
	if c.oddFolder {
		folder = filepath.Join(folder, "a,b")
	}
	for _, d := range []string{folder, data, filepath.Join(data, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	permPath, instrPath := filepath.Join(data, "permissions.toml"), filepath.Join(data, "instructions.md")
	if c.perm != "" {
		if err := os.WriteFile(permPath, []byte(strings.ReplaceAll(c.perm, "<readfolder>", readFolder)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if c.instr != nil {
		if err := os.WriteFile(instrPath, []byte(*c.instr), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if c.codexConfig {
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("developer_instructions = \"mine\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	environ := append([]string{"PATH=/usr/bin:/bin", "HOME=" + home, launchDirEnv + "=" + launch}, c.env...)
	getenv := func(k string) string {
		for _, kv := range environ {
			if n, v, _ := strings.Cut(kv, "="); n == k {
				return v
			}
		}
		return ""
	}
	lookPath := func(name string) (string, error) {
		if c.missing {
			return "", errors.New("not found")
		}
		return self, nil
	}
	st, err := store.Open(filepath.Join(realTemp(t), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var mu sync.Mutex
	var logs, replayAsked []string
	lastTurn := map[string]string{}
	cfg := engine.Config{Store: st, BaseURL: "http://127.0.0.1:8787", Wire: runWire(getenv, func() []string { return environ }, lookPath),
		Settings: settings.NewFiles(data), APIDir: filepath.Join(data, "bin"),
		Protect: []string{permPath, instrPath, filepath.Join(data, "agents.json")},
		Logf: func(f string, a ...any) {
			l := fmt.Sprintf(f, a...)
			if strings.HasPrefix(l, "run.edit") || strings.HasPrefix(l, "run.hint") || strings.HasPrefix(l, "run.refused") {
				mu.Lock()
				logs = append(logs, l)
				mu.Unlock()
			}
		}}
	if c.replay {
		cfg.ReplayBase = func(agent, session string) (engine.ReplayPlan, error) {
			replayAsked = append(replayAsked, agent)
			return engine.ReplayPlan{Turn: lastTurn[session]}, nil
		}
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mask := func(s string) string {
		s = tokenValue.ReplaceAllString(s, "AGENTBUS_TOKEN=<token>")
		for _, r := range [][2]string{{self, "<bin>"}, {data, "<data>"}, {folder, "<folder>"}, {home, "<home>"},
			{launch, "<launch>"}, {readFolder, "<readfolder>"}} {
			s = strings.ReplaceAll(s, r[0], r[1])
		}
		return s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "=== %s\n", c.name)
	n := 0
	for _, req := range c.turns {
		if req.Session == "" {
			req.Folder = folder
		}
		fmt.Fprintf(&b, "--- turn %s harness=%q access=%q model=%q read_only=%t\n", req.Turn, req.Harness, req.Access, req.Model, req.ReadOnly)
		logs, replayAsked = nil, nil
		tn, err := eng.Start(req)
		if err != nil {
			var r *engine.Refusal
			if errors.As(err, &r) {
				fmt.Fprintf(&b, "refused %d %s\n", r.Status, mask(r.Msg))
			} else {
				fmt.Fprintf(&b, "error %v\n", err)
			}
		} else {
			<-tn.Done()
			lastTurn["s1"] = tn.ID
			raw, err := os.ReadFile(filepath.Join(launch, strconv.Itoa(n)+".json"))
			if err != nil {
				t.Fatalf("%s: the child left no report: %v", c.name, err)
			}
			n++
			var rep fakeReport
			if err := json.Unmarshal(raw, &rep); err != nil {
				t.Fatal(err)
			}
			for _, a := range rep.Args {
				fmt.Fprintf(&b, "arg %s\n", mask(strconv.Quote(a)))
			}
			env := append([]string(nil), rep.Env...)
			sort.Strings(env)
			for _, kv := range env {
				fmt.Fprintf(&b, "env %s\n", mask(strconv.Quote(kv)))
			}
			fmt.Fprintf(&b, "cwd %s\n", mask(rep.Cwd))
			recs, err := st.After(0, store.Filter{}, 10000)
			if err != nil {
				t.Fatal(err)
			}
			for _, rec := range recs {
				var ev event.Event
				if err := json.Unmarshal(rec.Data, &ev); err != nil {
					t.Fatal(err)
				}
				if ev.Turn != tn.ID {
					continue
				}
				switch ev.Kind {
				case event.TurnStarted:
					fmt.Fprintf(&b, "turn.started agent=%q continuity=%q replay_from=%q model=%q context=%s\n",
						ev.Harness, ev.Continuity, ev.ReplayFrom, ev.Model, strconv.Quote(ev.Context))
				case event.SessionClosed:
					fmt.Fprintf(&b, "session.closed resume_id=%q\n", ev.ResumeID)
				}
			}
		}
		if len(replayAsked) > 0 {
			fmt.Fprintf(&b, "replay_base %s\n", strings.Join(replayAsked, ","))
		}
		mu.Lock()
		for _, l := range logs {
			fmt.Fprintf(&b, "log %s\n", mask(l))
		}
		mu.Unlock()
	}
	return b.String()
}
