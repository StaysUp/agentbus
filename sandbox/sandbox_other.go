//go:build !darwin && !linux

package sandbox

import (
	"os/exec"

	"github.com/staysup/agentbus/netjail"
)

// Supported: this platform has no sandbox.
const Supported = false

type platform struct{}

func (b *Box) build(netjail.Spec) error {
	return &Error{Why: "this platform has no sandbox; it is built on macOS and Linux"}
}

// Start refuses: there is no wall here.
func (b *Box) Start(*exec.Cmd) error {
	return &Error{Why: "this platform has no sandbox; it is built on macOS and Linux"}
}
