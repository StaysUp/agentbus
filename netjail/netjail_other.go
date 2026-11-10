//go:build !linux

package netjail

import "os/exec"

// Supported: this platform cannot close an agent off.
const Supported = false

// Start refuses: network control is Linux-only.
func (j *Jail) Start(*exec.Cmd) error { return &Error{Why: errUnsupported.Error()} }

// Check says this platform has no network control.
func Check() error { return &Error{Why: errUnsupported.Error()} }
