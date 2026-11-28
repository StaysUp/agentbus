//go:build !darwin && !linux

package main

import "errors"

func copyWay(string) finding {
	return finding{statusNote, "Copy copies a folder file by file on this system"}
}

func appArmorFinding() *finding { return nil }

func freeSpace(string) (uint64, error) { return 0, errors.New("not known here") }
