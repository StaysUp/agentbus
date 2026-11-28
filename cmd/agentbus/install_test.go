package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// install.sh prints the exact line to add for the user's shell when the
// install directory is not on PATH. It never edits a profile itself.
func TestInstallPathHintPerShell(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	fn := func(name string) string { return shellFunc(t, s, name) }
	for _, tt := range []struct {
		shell   string
		wants   []string
		notWant string
	}{
		{"/bin/zsh", []string{`echo 'export PATH="/opt/bin:$PATH"' >> ~/.zshrc`}, "~/.bashrc"},
		// macOS uses ~/.bash_profile, Linux ~/.bashrc.
		{"/bin/bash", []string{
			`echo 'export PATH="/opt/bin:$PATH"' >> ~/.bashrc`,
			`echo 'export PATH="/opt/bin:$PATH"' >> ~/.bash_profile`,
		}, "~/.zshrc"},
		{"/usr/bin/fish", []string{"fish_add_path /opt/bin"}, "~/.zshrc"},
		{"/usr/bin/unknown", []string{"Add /opt/bin to your shell's PATH."}, "fish_add_path"},
	} {
		t.Run(tt.shell, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", fn("say")+fn("path_hint")+`path_hint "$1"`, "sh", "/opt/bin")
			cmd.Env = append(os.Environ(), "SHELL="+tt.shell)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			found := false
			for _, want := range tt.wants {
				if strings.Contains(string(out), want) {
					found = true
				}
			}
			if !found {
				t.Errorf("output contains none of %q:\n%s", tt.wants, out)
			}
			if strings.Contains(string(out), tt.notWant) {
				t.Errorf("output contains %q:\n%s", tt.notWant, out)
			}
		})
	}
}

// shellFunc answers the shell function name from install.sh's text s, so a
// test can run it alone.
func shellFunc(t *testing.T, s, name string) string {
	t.Helper()
	i := strings.Index(s, name+"() {")
	if i < 0 {
		t.Fatalf("install.sh has no %s", name)
	}
	line := s[i : i+strings.Index(s[i:], "\n")+1]
	if strings.HasSuffix(line, "}\n") {
		return line
	}
	j := strings.Index(s[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("install.sh's %s does not end", name)
	}
	return s[i : i+j+3]
}

// On Windows, install.sh runs only in a Unix-like shell such as Git Bash,
// MSYS2 or Cygwin. It sends such a shell to WSL2, where the Linux build
// runs, instead of stopping with no next step (#59).
func TestInstallSendsWindowsShellsToWSL2(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	script := shellFunc(t, s, "say") + shellFunc(t, s, "fail") + shellFunc(t, s, "release_os") + `release_os "$1"`
	for _, tt := range []struct {
		uname string
		ok    bool
		want  string
	}{
		{"Linux", true, "linux"},
		{"Darwin", true, "darwin"},
		{"MINGW64_NT-10.0-19045", false, "WSL2"},
		{"MSYS_NT-10.0-19045", false, "WSL2"},
		{"CYGWIN_NT-10.0-19045", false, "WSL2"},
		{"FreeBSD", false, "unsupported system FreeBSD"},
	} {
		t.Run(tt.uname, func(t *testing.T) {
			out, err := exec.Command("sh", "-c", script, "sh", tt.uname).CombinedOutput()
			if tt.ok != (err == nil) {
				t.Fatalf("ok = %v, want %v: %s", err == nil, tt.ok, out)
			}
			if !strings.Contains(string(out), tt.want) {
				t.Errorf("output does not contain %q:\n%s", tt.want, out)
			}
		})
	}
}
