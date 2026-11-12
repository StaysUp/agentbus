package settings

// Files is the store of one data folder: permissions.toml and instructions.md,
// read fresh on every call, so an edit by hand applies to the next turn
// without a restart, and the person may keep editing them. A write through
// the API replaces the file whole, by rename, so a reader never sees half of
// one.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Files keeps the settings as files in a data folder.
type Files struct {
	dir string
	mu  sync.Mutex // one write at a time from this process
}

// NewFiles is the store of the data folder dir.
func NewFiles(dir string) *Files { return &Files{dir: dir} }

// noFile is the version of a file that is not there.
const noFile Version = "none"

// What a turn's refusal says of each file.
const (
	policyWhere       = "the permissions file " + PolicyFile + " in the data folder"
	policyFix         = "Fix the file, or remove it for the defaults"
	instructionsWhere = "the instructions file " + InstructionsFile + " in the data folder"
	instructionsFix   = "Fix the file, or remove it for the default text"
)

// Init writes each file that is missing, with mode 0600: the starter
// permissions.toml, with every default written out, and instructions.md, with
// the default text. An existing file is never written: it is the person's.
func (f *Files) Init() error {
	for _, s := range []struct{ file, text, name string }{
		{PolicyFile, PolicyStarter, "permissions"},
		{InstructionsFile, DefaultInstructions, "instructions"},
	} {
		if err := makeStarter(filepath.Join(f.dir, s.file), s.text, s.name); err != nil {
			return err
		}
	}
	return nil
}

func makeStarter(path, text, name string) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the %s file %s could not be made", name, path)
	}
	if _, err := out.WriteString(text); err != nil {
		out.Close()
		return fmt.Errorf("the %s file %s could not be written", name, path)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("the %s file %s could not be written", name, path)
	}
	return nil
}

// Protected are the data folder's own files.
func (f *Files) Protected() []string { return protected(f.dir) }

// Policy reads permissions.toml. A missing file is every default.
func (f *Files) Policy() (Policy, Version, error) {
	b, v, err := readFile(filepath.Join(f.dir, PolicyFile))
	if err != nil {
		return Policy{}, v, &Error{Setting: "permissions", Where: policyWhere, Why: err.Error(), Fix: policyFix, Version: v}
	}
	p, err := ParsePolicy(b)
	if err != nil {
		return Policy{}, v, &Error{Setting: "permissions", Where: policyWhere, Why: err.Error(), Fix: policyFix, Version: v}
	}
	p.where, p.fix = policyWhere, policyFix
	return p, v, nil
}

// policyOver reads permissions.toml as Policy does, with each key the file
// leaves out taking base's value (the defaults the import runs under).
func (f *Files) policyOver(base Policy) (Policy, error) {
	b, _, err := readFile(filepath.Join(f.dir, PolicyFile))
	if err != nil {
		return Policy{}, err
	}
	p, err := parsePolicy(b)
	if err != nil {
		return Policy{}, err
	}
	return p.over(base)
}

// SetPolicy writes p as permissions.toml (FormatPolicy).
func (f *Files) SetPolicy(p Policy, match Version) (Policy, Version, error) {
	if err := p.Check(); err != nil {
		return Policy{}, "", &Invalid{Why: err.Error()}
	}
	p.fill()
	b := FormatPolicy(p)
	v, err := f.write(PolicyFile, b, match)
	if err != nil {
		return Policy{}, "", err
	}
	p.where, p.fix = policyWhere, policyFix
	return p, v, nil
}

// Instructions reads instructions.md. A missing file is the default text.
func (f *Files) Instructions() (string, Version, error) {
	b, v, err := readFile(filepath.Join(f.dir, InstructionsFile))
	if err == nil {
		if v == noFile {
			return DefaultInstructions, v, nil
		}
		err = CheckInstructions(b)
	}
	if err != nil {
		return "", v, &Error{Setting: "instructions", Where: instructionsWhere, Why: err.Error(), Fix: instructionsFix, Version: v}
	}
	return string(b), v, nil
}

// SetInstructions writes text as instructions.md.
func (f *Files) SetInstructions(text string, match Version) (Version, error) {
	if err := CheckInstructions([]byte(text)); err != nil {
		return "", &Invalid{Why: "text: " + err.Error()}
	}
	return f.write(InstructionsFile, []byte(text), match)
}

// write replaces the file name with b when it is still at match. A file that
// is a link is written where the link points, so the link stays.
func (f *Files) write(name string, b []byte, match Version) (Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := filepath.Join(f.dir, name)
	_, cur, err := readFile(path)
	if err != nil && cur == "" {
		// Nothing there can be read, so nothing can be matched.
		return "", fmt.Errorf("settings: %s cannot be replaced: %v", name, err)
	}
	if match != Any && match != cur {
		return "", ErrChanged
	}
	target := path
	if cur != noFile {
		if target, err = filepath.EvalSymlinks(path); err != nil {
			return "", fmt.Errorf("settings: %s cannot be replaced", name)
		}
	}
	mode := fs.FileMode(0o600)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*")
	if err != nil {
		return "", fmt.Errorf("settings: %s could not be written", name)
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", fmt.Errorf("settings: %s could not be written", name)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", fmt.Errorf("settings: %s could not be written", name)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("settings: %s could not be written", name)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("settings: %s could not be written", name)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return "", fmt.Errorf("settings: %s could not be written", name)
	}
	ok = true
	return versionOf(b), nil
}

// readFile reads path and its version. A missing file is no bytes at
// version noFile. The error says what is wrong in words of its own and never
// names the path. Its version is "" when nothing could be read, so no writer
// can match it.
func readFile(path string) ([]byte, Version, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A symlink whose target is gone is not a missing file: the person
		// meant a policy, and it is not there (found in review).
		if _, lerr := os.Lstat(path); lerr == nil {
			return nil, "", errors.New("it is a link to a file that does not exist")
		}
		return nil, noFile, nil
	}
	if err != nil {
		return nil, "", errors.New("it could not be read")
	}
	return b, versionOf(b), nil
}

// versionOf names the bytes b.
func versionOf(b []byte) Version {
	sum := sha256.Sum256(b)
	return Version(hex.EncodeToString(sum[:16]))
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
