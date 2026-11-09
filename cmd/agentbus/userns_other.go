//go:build !linux

package main

import "io"

// userns does nothing here: a user namespace is Linux's (userns_linux.go).
func userns([]string, func(string) string, io.Writer) (int, bool) { return 0, false }
