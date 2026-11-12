//go:build !unix

package sandbox

import "errors"

func mkfifo(string) error { return errors.New("no named pipes") }
