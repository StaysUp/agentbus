// Command abgateway runs the local gateway and prints one event per line.
//
//	abgateway -listen 127.0.0.1:8787
//	ANTHROPIC_BASE_URL=http://127.0.0.1:8787/s/my-session claude
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/gateway"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8787", "address to listen on; must be this machine")
	upstream := flag.String("upstream", "https://api.anthropic.com", "the vendor's API address")
	flag.Parse()

	if err := loopbackOnly(*listen); err != nil {
		fmt.Fprintln(os.Stderr, "abgateway:", err)
		os.Exit(2)
	}
	up, err := url.Parse(*upstream)
	if err != nil || up.Scheme == "" || up.Host == "" {
		fmt.Fprintf(os.Stderr, "abgateway: -upstream %q is not an address\n", *upstream)
		os.Exit(2)
	}

	// net/http writes some of its own messages to the global logger, and they
	// can quote a vendor reply's headers. Nothing else here uses that logger.
	log.SetOutput(io.Discard)
	own := log.New(os.Stderr, "abgateway: ", log.LstdFlags)

	g := gateway.New(up, anthropic.Adapter{}, event.NewJSONLines(os.Stdout))
	srv := &http.Server{
		Addr:              *listen,
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	own.Printf("%s -> %s", *listen, up)
	if err := srv.ListenAndServe(); err != nil {
		own.Printf("stopped: %v", err)
		os.Exit(1)
	}
}

// loopbackOnly keeps the gateway on this machine. It carries the user's own
// sign-in, and a sign-in never leaves the machine it belongs to. Only an IP
// literal is accepted: a name such as localhost resolves through the hosts
// file, which can map it to any address.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-listen %q is not a loopback IP such as 127.0.0.1 or [::1]; the gateway stays on this machine", addr)
}
