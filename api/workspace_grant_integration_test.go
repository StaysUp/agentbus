//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/store"
)

// An agent's turn token makes a workspace within its own authority (0.6.6, his
// call 2026-11-24): a copy, never direct; on its own machine; a Direct agent of
// any folder there, a Copy agent only of the folders its own workspace holds,
// named by folder_ref. Its session then opens sessions and runs turns there,
// reads it, registers an on for its turn, and applies it when that adds no
// power (its own workspace is Direct) or the person's switch says so.
func TestAgentMakesWorkspaceWithinAuthority(t *testing.T) {
	perm := filepath.Join(t.TempDir(), "permissions.toml")
	os.WriteFile(perm, nil, 0o600) // Agents may apply: off
	e, dir := askEnvWith(t, perm)
	defer func() {
		for _, p := range []string{"direct", "copy", "builder", "nested", "copybuilder", "sibling", "copy2", "copybuilder2"} {
			release(dir, p)
		}
	}()
	f1, f2 := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(f1, "a.txt"), []byte("one\n"), 0o600)
	ref1, _ := e.folderRef(t, f1)
	ref2, _ := e.folderRef(t, f2)

	mustWS := func(tok, body string) store.Workspace {
		t.Helper()
		code, out := as(t, e, tok, "POST", "/api/v0/workspaces", body)
		if code != 201 {
			t.Fatalf("make workspace %s: %d %s", body, code, out)
		}
		var w store.Workspace
		if json.Unmarshal([]byte(out), &w) != nil || !w.Sandbox {
			t.Fatalf("workspace %s: %s", body, out)
		}
		return w
	}
	refused := func(tok, method, path, body, want string) {
		t.Helper()
		code, out := as(t, e, tok, method, path, body)
		if code != 403 || !strings.Contains(out, want) {
			t.Fatalf("%s %s %s: %d %s, want 403 %q", method, path, body, code, out, want)
		}
	}
	session := func(tok, ws string) string {
		t.Helper()
		code, out := as(t, e, tok, "POST", "/api/v0/sessions", `{"workspace":"`+ws+`"}`)
		if code != 201 {
			t.Fatalf("session in %s: %d %s", ws, code, out)
		}
		var sc SessionCreated
		if json.Unmarshal([]byte(out), &sc) != nil || sc.Workspace != ws {
			t.Fatalf("session: %s", out)
		}
		return sc.Session
	}
	start := func(tok, sess, prompt string) Started {
		t.Helper()
		code, out := as(t, e, tok, "POST", "/api/v0/turns", `{"session":"`+sess+`","agent":"claude","prompt":"`+prompt+`"}`)
		if code != 202 {
			t.Fatalf("start %s: %d %s", prompt, code, out)
		}
		var s Started
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A Direct agent: it has no wall, so a copy of any folder of its machine
	// adds no power.
	parent := e.firstTurn(t, f1, "claude", "direct", false)
	dtok := tokenOf(t, dir, "direct")
	w := mustWS(dtok, `{"folder":"`+f2+`"}`)
	if len(w.Folders) != 1 || w.Folders[0].FolderRef != ref2 {
		t.Fatalf("direct agent's workspace: %+v", w)
	}
	if maker, err := e.store.WorkspaceMaker(ids.WorkspaceID(w.ID)); err != nil || maker == "" {
		t.Fatalf("the workspace's maker is not recorded: %q %v", maker, err)
	}
	// A Direct workspace of its machine is in its reach too (O2.3).
	if code, out := as(t, e, dtok, "POST", "/api/v0/workspaces", `{"folder":"`+f2+`","sandbox":false}`); code != 201 || !strings.Contains(out, `"sandbox":false`) {
		t.Fatalf("a Direct agent's Direct workspace: %d %s", code, out)
	}
	refused(dtok, "POST", "/api/v0/workspaces", `{"folder":"`+f2+`","machine":"con_0000000000000000"}`, agentMachineRefused)
	builder := start(dtok, session(dtok, w.ID), "builder")
	if pwd := pwdOf(t, dir, "builder"); pwd == f2 {
		t.Fatalf("the builder runs in the folder itself, not a copy: %s", pwd)
	}
	// It hears back only if it chooses: an on over the session it started.
	if code, out := as(t, e, dtok, "POST", "/api/v0/ons", `{"event":"turn.completed","filter":{"session":"`+builder.Session+`"}}`); code != 201 {
		t.Fatalf("on over the builder's session: %d %s", code, out)
	}
	if code, out := as(t, e, dtok, "GET", "/api/v0/workspaces/"+w.ID, ""); code != 200 {
		t.Fatalf("read the made workspace: %d %s", code, out)
	}
	// Apply adds no power for a Direct maker: allowed with the switch off.
	if code, out := as(t, e, dtok, "POST", "/api/v0/workspaces/"+w.ID+"/apply", "{}"); code != 200 {
		t.Fatalf("a Direct agent's apply of a workspace it made: %d %s", code, out)
	}
	// The maker reaches what it made: the builder's session and turn.
	if code, out := as(t, e, dtok, "GET", "/api/v0/sessions/"+builder.Session+"/events", ""); code != 200 {
		t.Fatalf("the maker reads its builder's session: %d %s", code, out)
	}
	// The builder, walled to its copy of f2, does not reach back into f1's
	// sessions of the same request tree: not their history, not their turns
	// (review 2026-11-24, P1).
	sibling := ask(t, e, dtok, `{"agent":"claude","prompt":"sibling"}`)
	btok := tokenOf(t, dir, "builder")
	for _, c := range [][3]string{
		{"GET", "/api/v0/sessions/" + parent.Session + "/events", ""},
		{"GET", "/api/v0/sessions/" + parent.Session, ""},
		{"GET", "/api/v0/turns?session=" + parent.Session, ""},
		{"GET", "/api/v0/turns/" + parent.Turn, ""},
		{"GET", "/api/v0/sessions/" + sibling.Session + "/events", ""},
		{"PATCH", "/api/v0/sessions/" + parent.Session, `{"title":"x"}`},
	} {
		refused(btok, c[0], c[1], c[2], turnSessionRefused)
	}
	refused(btok, "POST", "/api/v0/turns/"+sibling.Turn+"/stop", "", turnStopRefused)

	// A Copy agent: only its own folders, by reference.
	e.firstTurn(t, f1, "claude", "copy", true)
	ctok := tokenOf(t, dir, "copy")
	cw := mustWS(ctok, `{"folder_ref":"`+ref1+`"}`)
	refused(ctok, "POST", "/api/v0/workspaces", `{"folder_ref":"`+ref2+`"}`, agentFolderRefused)
	refused(ctok, "POST", "/api/v0/workspaces", `{"folders":[{"folder_ref":"`+ref1+`"},{"folder_ref":"`+ref2+`"}]}`, agentFolderRefused)
	refused(ctok, "POST", "/api/v0/workspaces", `{"folder":"`+f1+`"}`, agentPathRefused)
	refused(ctok, "POST", "/api/v0/workspaces", `{"folder_ref":"`+ref1+`","sandbox":false}`, agentDirectRefused)
	// Another session's workspace is not its own.
	refused(ctok, "POST", "/api/v0/sessions", `{"workspace":"`+w.ID+`"}`, turnWorkspaceRefused)
	refused(ctok, "GET", "/api/v0/workspaces/"+w.ID, "", turnSessionRefused)
	// The same idempotency_key and body from another maker is another
	// request, never the first maker's workspace handed back; the first maker's
	// retry gets its own.
	keyed := `{"folder_ref":"` + ref1 + `","idempotency_key":"k1"}`
	first := mustWS(dtok, keyed)
	if code, out := as(t, e, ctok, "POST", "/api/v0/workspaces", keyed); code != 409 {
		t.Fatalf("another maker's key: %d %s, want 409", code, out)
	}
	if again := mustWS(dtok, keyed); again.ID != first.ID {
		t.Fatalf("the maker's retry: %s, want %s", again.ID, first.ID)
	}
	// Apply of a workspace a Copy agent made takes the person's switch, as
	// its turn took it when it started: turned on mid-turn, it reaches the
	// next turn only (O2.4).
	start(ctok, session(ctok, cw.ID), "copybuilder")
	refused(ctok, "POST", "/api/v0/workspaces/"+cw.ID+"/apply", "{}", "Agents may apply is off in Settings")
	os.WriteFile(perm, []byte("[agent_api]\napply = true\n"), 0o600)
	refused(ctok, "POST", "/api/v0/workspaces/"+cw.ID+"/apply", "{}", "Agents may apply is off in Settings")
	e.firstTurn(t, f1, "claude", "copy2", true)
	c2tok := tokenOf(t, dir, "copy2")
	cw2 := mustWS(c2tok, `{"folder_ref":"`+ref1+`"}`)
	start(c2tok, session(c2tok, cw2.ID), "copybuilder2")
	if code, out := as(t, e, c2tok, "POST", "/api/v0/workspaces/"+cw2.ID+"/apply", "{}"); code != 200 {
		t.Fatalf("a Copy agent's apply with the switch on when its turn started: %d %s", code, out)
	}
	os.WriteFile(perm, nil, 0o600)

	// Chaining reaches no further: the builder in the Direct agent's copy of f2
	// is itself walled to f2.
	refused(btok, "POST", "/api/v0/workspaces", `{"folder_ref":"`+ref1+`"}`, agentFolderRefused)
	refused(btok, "POST", "/api/v0/workspaces", `{"folder":"`+f1+`"}`, agentPathRefused)
	bw := mustWS(btok, `{"folder_ref":"`+ref2+`"}`)
	start(btok, session(btok, bw.ID), "nested")
	ntok := tokenOf(t, dir, "nested")
	refused(ntok, "POST", "/api/v0/workspaces", `{"folder_ref":"`+ref1+`"}`, agentFolderRefused)
	mustWS(ntok, `{"folder_ref":"`+ref2+`"}`)
}
