//go:build !linux

package isolation

import (
	"errors"

	"github.com/staysup/agentbus/workspace"
)

// overlayBackend is Linux only: overlayfs is a Linux filesystem.
type overlayBackend struct{}

var errOverlayLinux = errors.New("isolation: overlay is Linux only")

func (overlayBackend) Name() string { return Overlay }

// overlayWhy says why no overlay can be mounted: none can, here.
func overlayWhy(string) string { return "overlay is a Linux filesystem" }

// OverlayWhy says why no overlay can be mounted: none can, here.
func OverlayWhy(workdir string) string { return overlayWhy(workdir) }

func remount(*Space) error { return errOverlayLinux }

// Unmount has nothing to unmount here: no overlay is ever mounted.
func Unmount(string) (int, error) { return 0, nil }

// NoNamespace does nothing here: a user namespace is Linux's.
func NoNamespace(string) {}

func (overlayBackend) Open(r Request) (*Space, error) {
	if err := checkOpen(r); err != nil {
		return nil, err
	}
	return nil, &Unavailable{Overlay, overlayWhy(r.Workdir)}
}

func (overlayBackend) Checkpoint(*Space, string) (Checkpoint, error) {
	return Checkpoint{}, errOverlayLinux
}
func (overlayBackend) Checkpoints(s *Space) ([]Checkpoint, error) { return checkpoints(s) }
func (overlayBackend) Diff(*Space, string, string) (workspace.Changes, error) {
	return workspace.Changes{}, errOverlayLinux
}
func (overlayBackend) Apply(*Space, string, string) (Applied, error) {
	return Applied{}, errOverlayLinux
}
func (overlayBackend) Close(*Space) error { return errOverlayLinux }
