package sandbox

import (
	"slices"
	"testing"
)

// A read path under /etc that etcRead does not bind is bound; one it binds
// is there already (review of 2026-11-12, round 6).
func TestBwrapArgsEtc(t *testing.T) {
	a := bwrapArgs("/c", false, "/h", []path{{real: "/etc/nginx", dir: true}, {real: "/etc/ssl", dir: true}})
	bound := func(p string) bool {
		for i := 0; i+2 < len(a); i++ {
			if a[i] == "--ro-bind" && a[i+1] == p && a[i+2] == p {
				return true
			}
		}
		return false
	}
	if !bound("/etc/nginx") {
		t.Errorf("/etc/nginx not bound: %v", a)
	}
	if slices.Contains(a, "/etc") {
		t.Errorf("all of /etc bound: %v", a)
	}
	if !inSystem("/etc/ssl/certs") || inSystem("/etc/nginx") || !inSystem("/usr/bin/sh") {
		t.Error("inSystem")
	}
}
