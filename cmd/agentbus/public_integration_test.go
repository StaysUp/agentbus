//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
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

// A caller that pipelines many small public replies on one connection and
// never reads holds it publicWait at most. Each reply fits net/http's buffer,
// so the handler returns before anything is written and the flush comes
// after it: the write deadline must still hold then. A reply without a token
// closes its connection, so the pipelined requests after the first are never
// served.
func TestPipelinedSmallPublicRepliesAreLetGo(t *testing.T) {
	saved, savedWait := cockpitFiles, publicWait
	cockpitFiles = cockpit.New(fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><html><head></head><body></body></html>")},
		"assets/small-1.js": {Data: bytes.Repeat([]byte("y"), 512)},
	})
	publicWait = time.Second
	svc := startService(t, t.TempDir(), "http://127.0.0.1:1")
	t.Cleanup(func() { cockpitFiles, publicWait = saved, savedWait })

	c, err := net.Dial("tcp", svc.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Enough replies to fill both sockets' buffers many times over. The
	// writes go on in the background, since the server may stop reading.
	req := []byte(fmt.Sprintf("GET /assets/small-1.js HTTP/1.1\r\nHost: %s\r\n\r\n", svc.addr))
	go func() {
		batch := bytes.Repeat(req, 1000)
		for range 100 {
			if _, err := c.Write(batch); err != nil {
				return
			}
		}
	}()
	time.Sleep(publicWait + 2*time.Second)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := io.Copy(io.Discard, c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the connection stayed open for a reader that never read (%d bytes read)", n)
	}
	// One reply, then the close: well under two replies' worth.
	if n > 4096 {
		t.Errorf("%d bytes were written on a connection that should have closed after one reply", n)
	}
}

// A public reply never shares its connection, so a token's request after it
// goes on a new one and a stream there outlives publicWait.
func TestTokenRequestAfterPublicReplyIsUnaffected(t *testing.T) {
	saved := publicWait
	publicWait = time.Second
	t.Cleanup(func() { publicWait = saved })
	home := t.TempDir()
	svc := startService(t, home, "http://127.0.0.1:1")
	tok := readToken(t, home)
	client := &http.Client{Transport: &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}}
	defer client.CloseIdleConnections()
	base := "http://" + svc.addr

	resp, err := client.Get(base + "/docs/llms.txt")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !resp.Close {
		t.Fatalf("public reply: status %d, close %v; want 200 and close", resp.StatusCode, resp.Close)
	}

	reused := true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }})
	r, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/v0/events", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	stream, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream: status %d", stream.StatusCode)
	}
	if reused {
		t.Fatal("the token's stream reused the public reply's connection")
	}

	// Past publicWait, an event posted now still arrives on the stream.
	time.Sleep(publicWait + time.Second)
	post, _ := http.NewRequest("POST", base+"/api/v0/events", strings.NewReader(`{"kind":"x.public.after","session":"default"}`))
	post.Header.Set("Authorization", "Bearer "+tok)
	pr, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	pr.Body.Close()
	if pr.StatusCode != http.StatusCreated {
		t.Fatalf("post: status %d", pr.StatusCode)
	}
	sc := bufio.NewScanner(stream.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "x.public.after") {
			return
		}
	}
	t.Fatalf("the stream ended before the event arrived: %v", sc.Err())
}
