package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/store"
)

// POST /api/v0/workspaces with folders records each folder, in order, with a
// name from the last part of its path, all on one machine. The workspace reads
// back with the list, and without folder or folder_ref, so no reader takes one
// folder for the whole workspace.
func TestCreateWorkspaceFolders(t *testing.T) {
	e := newJobsEnv(t)
	dir := t.TempDir()
	var refs []string
	for _, n := range []string{"framework", "cockpit", "notes"} {
		p := filepath.Join(dir, n)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		ref, _ := e.folderRef(t, p)
		refs = append(refs, ref)
	}
	machine := e.fr.Machine()
	body := fmt.Sprintf(`{"folders":[{"folder_ref":%q},{"folder_ref":%q},{"folder_ref":%q}],"machine":%q}`, refs[0], refs[1], refs[2], machine)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(body)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	var ws store.Workspace
	if err := json.Unmarshal([]byte(raw), &ws); err != nil {
		t.Fatal(err)
	}
	if ws.Folder != "" || ws.FolderRef != "" || !ws.Sandbox || ws.Machine != machine || len(ws.Folders) != 3 {
		t.Fatalf("workspace: %s", raw)
	}
	for i, n := range []ids.FolderName{"framework", "cockpit", "notes"} {
		if f := ws.Folders[i]; f.Name != n || f.FolderRef != refs[i] {
			t.Errorf("folder %d: %+v, want %s %s", i, f, n, refs[i])
		}
	}
	// The record reads it the same way.
	got, ok, err := e.store.Workspace(ids.WorkspaceID(ws.ID))
	if err != nil || !ok || len(got.Folders) != 3 || got.Folders[2].Name != "notes" {
		t.Fatalf("store: %+v %v %v", got, ok, err)
	}
}

// One entry in folders is today's workspace: folder and folder_ref set, one
// folder with no name, as its copy is the folder itself.
func TestCreateWorkspaceOneFolderList(t *testing.T) {
	e := newJobsEnv(t)
	ref, machine := e.folderRef(t, t.TempDir())
	resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(fmt.Sprintf(`{"folders":[{"folder_ref":%q}],"machine":%q}`, ref, machine))))
	var ws store.Workspace
	if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &ws) != nil {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	if ws.FolderRef != ref || len(ws.Folders) != 1 || ws.Folders[0].Name != "" || ws.Folders[0].FolderRef != ref {
		t.Fatalf("one folder: %s", raw)
	}
}

// Two folders of one name get name and name-2, without regard to case.
func TestCreateWorkspaceFolderNamesUnique(t *testing.T) {
	e := newJobsEnv(t)
	a, b := filepath.Join(t.TempDir(), "app"), filepath.Join(t.TempDir(), "App")
	for _, p := range []string{a, b} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ra, machine := e.folderRef(t, a)
	rb, _ := e.folderRef(t, b)
	resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(fmt.Sprintf(`{"folders":[{"folder_ref":%q},{"folder_ref":%q}],"machine":%q}`, ra, rb, machine))))
	var ws store.Workspace
	if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &ws) != nil {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	if ws.Folders[0].Name != "app" || ws.Folders[1].Name != "App-2" {
		t.Fatalf("names: %s", raw)
	}
}

// What a workspace with several folders refuses: direct mode, both shapes at
// once, the same folder twice, an entry with neither field, a folder of
// another machine, and more than the limit.
func TestCreateWorkspaceFoldersRefused(t *testing.T) {
	e := newJobsEnv(t)
	ra, machine := e.folderRef(t, t.TempDir())
	rb, _ := e.folderRef(t, t.TempDir())
	many := make([]string, MaxWorkspaceFolders+1)
	for i := range many {
		r, _ := e.folderRef(t, t.TempDir())
		many[i] = fmt.Sprintf(`{"folder_ref":%q}`, r)
	}
	for _, body := range []string{
		fmt.Sprintf(`{"folders":[{"folder_ref":%q},{"folder_ref":%q}],"machine":%q,"sandbox":false}`, ra, rb, machine),
		fmt.Sprintf(`{"folders":[{"folder_ref":%q},{"folder_ref":%q}],"folder_ref":%q,"machine":%q}`, ra, rb, ra, machine),
		fmt.Sprintf(`{"folders":[{"folder_ref":%q},{"folder_ref":%q}],"machine":%q}`, ra, ra, machine),
		fmt.Sprintf(`{"folders":[{"folder_ref":%q},{}],"machine":%q}`, ra, machine),
		fmt.Sprintf(`{"folders":[{"folder_ref":%q},{"folder_ref":"fld_9999999999999999"}],"machine":%q}`, ra, machine),
		fmt.Sprintf(`{"folders":[%s],"machine":%q}`, strings.Join(many, ","), machine),
	} {
		if resp, raw := do(t, e.req(t, "POST", "/api/v0/workspaces", strings.NewReader(body))); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /workspaces %s: %d %s, want 400", body, resp.StatusCode, raw)
		}
	}
}

// A folder's name is one safe path part: accents and other characters become
// -, a long name is cut to fit with its suffix, and . or .git is no name.
func TestFolderName(t *testing.T) {
	for in, want := range map[string]string{
		"agentbus-framework":     "agentbus-framework",
		"caf\u00e9":              "caf-",
		"cafe\u0301":             "cafe-",
		"my repo":                "my-repo",
		".":                      "folder",
		"..":                     "folder",
		".git":                   "folder",
		".dotfiles":              ".dotfiles",
		strings.Repeat("a", 300): strings.Repeat("a", maxFolderName),
	} {
		if got := folderName(in); got != want {
			t.Errorf("folderName(%q) = %q, want %q", in, got, want)
		}
	}
	used := map[string]bool{}
	long := folderName(strings.Repeat("b", 300))
	if a, b := uniqueName(long, used), uniqueName(long, used); a == b || len(b) > 255 {
		t.Errorf("unique long names: %q %q", a, b)
	}
}
