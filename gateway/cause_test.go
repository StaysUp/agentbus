package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/event"
)

// The fixed reason for each kind of upstream error, and the types it wraps.
func TestCauseOf(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	for _, c := range []struct {
		err           error
		class, reason string
	}{
		{reset, "*net.OpError>*os.SyscallError>syscall.Errno", "connection_reset"},
		{&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "*net.OpError>*os.SyscallError>syscall.Errno", "connection_refused"},
		{&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "SECRET_HOST"}}, "*net.OpError>*net.DNSError", "dns"},
		{fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF), "*fmt.wrapError>*errors.errorString", "unexpected_eof"},
		{io.EOF, "*errors.errorString", "eof"},
		{context.DeadlineExceeded, "context.deadlineExceededError", "deadline"},
		{errors.Join(errors.New("first"), io.EOF), "*errors.joinError>*errors.errorString", "eof"},
		{errors.New("http2: something SECRET_TEXT"), "*errors.errorString", "other"},
	} {
		class, reason := causeOf(c.err)
		if class != c.class || reason != c.reason {
			t.Errorf("%v: %s %s, want %s %s", c.err, class, reason, c.class, c.reason)
		}
	}
}

// A failed upstream call keeps its fixed reply to the agent, and the service
// log says which error it was, with ids, types and fixed words only: never a
// header value or Go's error text (API test bug 7, 2026-11-11).
func TestUpstreamFailureIsLogged(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	// A header the transport cannot parse, its value a marker.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, b, _ := w.(http.Hijacker).Hijack()
		defer c.Close()
		b.WriteString("HTTP/1.1 200 OK\r\nX-Review: RESPONSE_HEADER_MARKER\x00\r\nContent-Length: 2\r\n\r\n{}")
		b.Flush()
	}))
	defer bad.Close()
	// A vendor that closes the connection before it answers.
	hangup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}))
	defer hangup.Close()

	for _, c := range []struct {
		name, upstream, reply, reason string
	}{
		{"down", downURL, "gateway: the vendor could not be reached (dial)", "reason=connection_refused"},
		{"bad header", bad.URL, "gateway: the vendor's reply could not be read", "reason=protocol"},
		{"hang-up", hangup.URL, "gateway: the vendor's reply could not be read", "reason=eof"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, _ := url.Parse(c.upstream)
			mem := &event.Memory{}
			g := New(u, anthropic.Adapter{}, mem)
			var mu sync.Mutex
			var lines []string
			g.SetLog(func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, fmt.Sprintf(format, args...))
			})
			srv := httptest.NewServer(g)
			defer srv.Close()
			req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(okRequest))
			req.Header.Set("Authorization", "Bearer SECRET_TOKEN_MARKER")
			resp, err := rawClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway || strings.TrimSpace(string(body)) != c.reply {
				t.Errorf("reply: %d %q, want 502 %q", resp.StatusCode, body, c.reply)
			}
			waitFor(t, mem, 3)
			mu.Lock()
			defer mu.Unlock()
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "gateway.upstream_failed vendor=anthropic ") ||
				!strings.Contains(lines[0], " class=*") || !strings.Contains(lines[0], c.reason) || !strings.Contains(lines[0], " exchange=") {
				t.Fatalf("log %q, want one upstream_failed line with %s", lines, c.reason)
			}
			t.Log(lines[0])
			for _, m := range []string{"MARKER", "127.0.0.1", "\n"} {
				if strings.Contains(lines[0], m) {
					t.Errorf("the log line holds %q: %q", m, lines[0])
				}
			}
		})
	}
}
