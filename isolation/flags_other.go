//go:build !darwin

package isolation

// unlock is macOS only: elsewhere a copy keeps no lock flags of the person's.
// A plain copy makes new files, and Linux's immutable attribute needs root.
func unlock(string) {}
