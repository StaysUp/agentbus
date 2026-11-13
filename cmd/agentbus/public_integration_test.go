//go:build integration

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"testing/fstest"
	"time"

	"github.com/staysup/agentbus/cockpit"
)

// A caller that asks for a public reply and never reads it holds the
// connection publicWait at most: the server stops writing and closes it.
// Without the write deadline it would wait for the reader for good.
func TestNeverReadingClientIsLetGo(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 4<<20)
	saved, savedWait := cockpitFiles, publicWait
	cockpitFiles = cockpit.New(fstest.MapFS{
		"index.html":      {Data: []byte("<!doctype html><html><head></head><body></body></html>")},
		"assets/big-1.js": {Data: big},
	})
	publicWait = time.Second
	svc := startService(t, t.TempDir(), "http://127.0.0.1:1")
	t.Cleanup(func() { cockpitFiles, publicWait = saved, savedWait })

	for _, tc := range []struct {
		path string
		size int
	}{
		{"/assets/big-1.js", len(big)},
		{"/docs/redoc.standalone.js", 1 << 20},
	} {
		const asks = 16
		c, err := net.Dial("tcp", svc.addr)
		if err != nil {
			t.Fatal(err)
		}
		for range asks {
			fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", tc.path, svc.addr)
		}
		// Read nothing until the server must have given up.
		time.Sleep(publicWait + 2*time.Second)
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, err := io.Copy(io.Discard, c)
		c.Close()
		// The server closes it: an end of file, or a reset for the bytes
		// it never sent. Only a read that times out means it waited.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Errorf("%s: the connection stayed open for a reader that never read (%d bytes)", tc.path, n)
			continue
		}
		if n >= int64(asks*tc.size) {
			t.Errorf("%s: all %d replies were written (%d bytes); the server waited for the reader", tc.path, asks, n)
		}
	}
}
