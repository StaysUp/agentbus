package run

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The folder's AGENTS.md goes to a harness that does not read it by itself,
// once, cut at MaxFolderDoc, and never from outside the folder.
func TestFolderDoc(t *testing.T) {
	write := func(dir, name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if got, why := folderDoc(hn("claude"), dir); got != "" || why != "" {
		t.Errorf("no AGENTS.md: %q %q", got, why)
	}
	write(dir, "AGENTS.md", "Run make test.\n")
	if got, _ := folderDoc(hn("claude"), dir); got != "" {
		t.Errorf("claude reads AGENTS.md by itself with no CLAUDE.md, yet got %q", got)
	}
	write(dir, "CLAUDE.md", "Be brief.\n")
	if got, _ := folderDoc(hn("claude"), dir); got != "The AGENTS.md of your working folder:\n\nRun make test." {
		t.Errorf("claude, with a CLAUDE.md of its own: %q", got)
	}
	if got, _ := folderDoc(hn("codex"), dir); got != "" {
		t.Errorf("codex reads it by itself, yet got %q", got)
	}
	if err := os.Remove(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if got, _ := folderDoc(hn("claude"), dir); got != "" {
		t.Errorf("CLAUDE.md is AGENTS.md, yet got %q", got)
	}

	// Cut at the bound, on a whole character, with a NUL byte left out.
	big := t.TempDir()
	write(big, "CLAUDE.md", "")
	write(big, "AGENTS.md", "a\x00"+strings.Repeat("é", MaxFolderDoc))
	got, _ := folderDoc(hn("claude"), big)
	if strings.ContainsRune(got, 0) || strings.ContainsRune(got, '�') || !strings.HasSuffix(got, "é\n\n(AGENTS.md goes on past 32 KiB; read the file for the rest.)") {
		t.Errorf("cut: ...%q", got[max(0, len(got)-80):])
	}

	// A symlink out of the folder gives nothing.
	out := t.TempDir()
	write(out, "secret", "outside")
	far := t.TempDir()
	write(far, "CLAUDE.md", "")
	if err := os.Symlink(filepath.Join(out, "secret"), filepath.Join(far, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if got, _ := folderDoc(hn("claude"), far); got != "" {
		t.Errorf("a symlink out of the folder: %q", got)
	}
	// A folder named AGENTS.md is left out, and the log says so.
	odd := t.TempDir()
	write(odd, "CLAUDE.md", "")
	if err := os.Mkdir(filepath.Join(odd, "AGENTS.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, why := folderDoc(hn("claude"), odd); got != "" || why != "not_a_file" {
		t.Errorf("a folder: %q %q", got, why)
	}
}

func TestJoinText(t *testing.T) {
	if got := joinText("", "a", "", "b", ""); got != "a\n\nb" {
		t.Errorf("%q", got)
	}
	if got := joinText("", ""); got != "" {
		t.Errorf("%q", got)
	}
}

// A turn in a home of its own: HOME and the harness's config variable point
// into it, the runner's own XDG folders are gone, and nothing else changes.
// RemoveHome removes it, read-only folders and all.
func TestHome(t *testing.T) {
	homes := filepath.Join(t.TempDir(), "homes")
	e := &Engine{cfg: Config{Homes: homes}}
	env := []string{"PATH=/bin", "HOME=/home/runner", "XDG_CONFIG_HOME=/home/runner/.config", "CODEX_HOME=/home/runner/.codex", "KEEP=1"}
	// A next turn of a session started before homes: no home, env as it is.
	got, made, err := e.home("s0", false, hn("codex"), env)
	if err != nil || made || !slices.Equal(got, env) {
		t.Fatalf("an older session: %q %t %v", got, made, err)
	}
	if _, err := os.Stat(filepath.Join(homes, "s0")); !os.IsNotExist(err) {
		t.Fatalf("an older session got a home: %v", err)
	}
	got, made, err = e.home("s1", true, hn("codex"), env)
	if err != nil || !made {
		t.Fatal(made, err)
	}
	dir := filepath.Join(homes, "s1")
	want := []string{"PATH=/bin", "KEEP=1", "HOME=" + dir, "CODEX_HOME=" + filepath.Join(dir, ".codex")}
	if !slices.Equal(got, want) {
		t.Errorf("env\n got %q\nwant %q", got, want)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".codex")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("codex's folder: %v %v", fi, err)
	}
	got, made, err = e.home("s1", false, hn("claude"), env)
	if err != nil || made || !slices.Contains(got, "CLAUDE_CONFIG_DIR="+filepath.Join(dir, ".claude")) {
		t.Fatalf("a next turn on claude: %q %t %v", got, made, err)
	}
	ro := filepath.Join(dir, "go", "pkg", "mod")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "f"), nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveHome("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the home is still there: %v", err)
	}
	if err := e.RemoveHome("../x"); err != nil {
		t.Fatal(err)
	}
	if err := (&Engine{}).RemoveHome("s1"); err != nil {
		t.Fatal(err)
	}
}

// A named pipe as AGENTS.md or CLAUDE.md is refused at once: nothing waits
// for a writer that never comes (found in review).
func TestFolderDocPipe(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		dir := t.TempDir()
		if name == "CLAUDE.md" {
			if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("a"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := syscall.Mkfifo(filepath.Join(dir, name), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan string, 1)
		go func() { got, why := folderDoc(hn("claude"), dir); done <- got + "|" + why }()
		select {
		case got := <-done:
			want := "|not_a_file"
			if name == "CLAUDE.md" {
				want = "The AGENTS.md of your working folder:\n\na|"
			}
			if got != want {
				t.Errorf("%s a pipe: %q, want %q", name, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s a pipe: folderDoc waits", name)
		}
	}
}

// A harness's files go into the session's own home only, readable by its
// owner, and never outside it. A session with no home gets none.
func TestWriteHome(t *testing.T) {
	homes := filepath.Join(t.TempDir(), "homes")
	e := &Engine{cfg: Config{Homes: homes}}
	files := map[string][]byte{".gemini/antigravity-cli/antigravity-oauth-token": []byte(`{"token":{}}`)}
	if why, err := e.writeHome("s1", files); err == nil || why != "home_files_no_home" {
		t.Fatalf("no home: %q %v", why, err)
	}
	if _, err := os.Stat(homes); !os.IsNotExist(err) {
		t.Fatalf("a home was made: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(homes, "s1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if why, err := e.writeHome("s1", files); err != nil {
		t.Fatalf("%q %v", why, err)
	}
	fi, err := os.Stat(filepath.Join(homes, "s1", ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", err, fi)
	}
	if _, err := e.writeHome("s1", map[string][]byte{"../s2/x": nil}); err == nil {
		t.Fatal("a file outside the home was written")
	}
	if _, err := (&Engine{}).writeHome("s1", files); err == nil {
		t.Fatal("a service without homes wrote a home's files")
	}
}
