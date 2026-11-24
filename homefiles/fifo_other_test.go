//go:build !unix

package homefiles

import "errors"

func mkfifo(string) error { return errors.New("no named pipes") }
