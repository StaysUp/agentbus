package main

import "testing"

func TestLoopbackOnly(t *testing.T) {
	ok := []string{"127.0.0.1:8787", "127.0.0.2:1", "[::1]:8787"}
	bad := []string{"localhost:8787", "0.0.0.0:8787", ":8787", "[::]:8787", "192.168.1.5:8787", "example.com:80", "127.0.0.1"}
	for _, a := range ok {
		if err := loopbackOnly(a); err != nil {
			t.Errorf("%s refused: %v", a, err)
		}
	}
	for _, a := range bad {
		if loopbackOnly(a) == nil {
			t.Errorf("%s accepted", a)
		}
	}
}
