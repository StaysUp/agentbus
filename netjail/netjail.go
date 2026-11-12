// Package netjail closes a turn's agent off from the network, on Linux. The
// agent runs in a network namespace of its own, with loopback only: no
// route out and no DNS. It has two ways out, both on 127.0.0.1 inside its
// namespace and served by the runner outside it:
//
//   - the forwarded addresses (Spec.Forward): its runner's gateway and, on
//     one machine, the service's API, at the same address as outside;
//   - a proxy (HTTPS_PROXY, HTTP_PROXY) that lets through the hosts the
//     policy allows (Spec.Allow) and the server's API (Spec.Pass). HTTPS
//     goes by CONNECT with the host's name, so TLS stays end to end.
//
// The proxy refuses anything else, and reports each refusal
// (Spec.Refused). Other platforms have no network control: Check says so.
package netjail

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Spec is what one agent may reach.
type Spec struct {
	// Forward are loopback addresses (ip:port) the agent reaches as they
	// are: each is served at the same address inside and passed to the same
	// address outside.
	Forward []string
	// Pass are host:port addresses the proxy lets through, such as the
	// server's API on another machine.
	Pass []string
	// Allow are host names the proxy lets through on any port. An entry
	// "*.example.com" also matches every name under example.com.
	Allow []string
	// Open lets the proxy through to every host on the public internet
	// (public): the agent still reaches the network only through it, and
	// this machine and its networks only through Forward, Pass and Allow.
	Open bool
	// Refused is told each connection the proxy refused: the host:port the
	// agent asked for, and why. Nil tells no one.
	Refused func(target, why string)
}

// Error is why an agent could not be closed off. Its text is fixed
// wording, safe to show.
type Error struct{ Why string }

func (e *Error) Error() string { return "the agent's network could not be closed off: " + e.Why }

// dialTimeout bounds a connection the runner makes for the agent.
const dialTimeout = 10 * time.Second

// Jail is one agent's network. New makes it, Start starts the agent in it,
// Close ends it once the agent exited.
type Jail struct {
	spec  Spec
	proxy string // the proxy's ip:port inside

	plain *httputil.ReverseProxy // plain HTTP, through dial

	mu     sync.Mutex
	closed bool
	lns    []net.Listener
	conns  map[net.Conn]struct{}
	srv    *http.Server
}

// New is a jail for spec. Nothing is made until Start.
func New(spec Spec) *Jail {
	j := &Jail{spec: spec, conns: map[net.Conn]struct{}{}}
	j.plain = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = pr.In.URL
			pr.Out.Host = pr.In.Host
		},
		Transport:     &http.Transport{Proxy: nil, DialContext: j.dial, IdleConnTimeout: 30 * time.Second},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "agentbus: the host could not be reached", http.StatusBadGateway)
		},
	}
	// A free port inside: the namespace is new, so only the forwarded
	// addresses are taken.
	for port := 3128; ; port++ {
		addr := "127.0.0.1:" + strconv.Itoa(port)
		taken := false
		for _, f := range spec.Forward {
			taken = taken || f == addr
		}
		if !taken {
			j.proxy = addr
			break
		}
	}
	return j
}

// NewHost is a jail with no namespace: its proxy listens on a free port of
// this machine's loopback, now, and is served until Close. The forwarded
// addresses are not served: the agent reaches them as they are. It is for
// a wall that lets the agent reach only these ports, as Seatbelt does on
// macOS (package sandbox); on its own it closes nothing off.
func NewHost(spec Spec) (*Jail, error) {
	j := New(spec)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, &Error{Why: "the proxy could not listen on this machine's loopback"}
	}
	j.proxy = l.Addr().String()
	j.mu.Lock()
	j.lns = []net.Listener{l}
	srv := &http.Server{Handler: http.HandlerFunc(j.proxyHTTP), ErrorLog: log.New(io.Discard, "", 0),
		ReadHeaderTimeout: 30 * time.Second}
	j.srv = srv
	j.mu.Unlock()
	go srv.Serve(l)
	return j, nil
}

// Proxy is the proxy's ip:port, as the agent reaches it.
func (j *Jail) Proxy() string { return j.proxy }

// Env is what the agent's environment needs: the proxy, for every client
// that reads either spelling, and loopback left out of it.
func (j *Jail) Env() []string {
	p := "http://" + j.proxy
	no := "localhost,127.0.0.1,::1"
	return []string{"HTTPS_PROXY=" + p, "https_proxy=" + p, "HTTP_PROXY=" + p, "http_proxy=" + p,
		"NO_PROXY=" + no, "no_proxy=" + no}
}

// listen is every address the agent's namespace listens on: the forwarded
// ones, then the proxy.
func (j *Jail) listen() []string { return append(append([]string{}, j.spec.Forward...), j.proxy) }

// serve takes the listeners made inside, in listen's order, and serves
// them. It closes them if the jail is closed already.
func (j *Jail) serve(lns []net.Listener) {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		for _, l := range lns {
			l.Close()
		}
		return
	}
	j.lns = lns
	srv := &http.Server{Handler: http.HandlerFunc(j.proxyHTTP), ErrorLog: log.New(io.Discard, "", 0),
		ReadHeaderTimeout: 30 * time.Second}
	j.srv = srv
	j.mu.Unlock()
	for i, l := range lns[:len(lns)-1] {
		go j.forward(l, j.spec.Forward[i])
	}
	go srv.Serve(lns[len(lns)-1])
}

// Close stops serving the agent's namespace and ends its connections.
func (j *Jail) Close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	for _, l := range j.lns {
		l.Close()
	}
	if j.srv != nil {
		j.srv.Close()
	}
	for c := range j.conns {
		c.Close()
	}
}

// dial is every connection the runner makes for the agent: tracked, so
// Close ends it, whatever is still using it.
func (j *Jail) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if to, ok := ctx.Value(dialTo{}).(string); ok {
		addr = to
	}
	c, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if !j.track(c) {
		return nil, net.ErrClosed
	}
	return untrackOnClose{c, j}, nil
}

// untrackOnClose forgets its connection when it is closed.
type untrackOnClose struct {
	net.Conn
	j *Jail
}

// CloseWrite half-closes it, as splice asks, where the connection can.
func (u untrackOnClose) CloseWrite() error {
	if cw, ok := u.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (u untrackOnClose) Close() error {
	u.j.mu.Lock()
	delete(u.j.conns, u.Conn)
	u.j.mu.Unlock()
	return u.Conn.Close()
}

// track keeps c to close with the jail. It is false once the jail is
// closed, and c is closed then.
func (j *Jail) track(c net.Conn) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		c.Close()
		return false
	}
	j.conns[c] = struct{}{}
	return true
}

func (j *Jail) untrack(c net.Conn) {
	j.mu.Lock()
	delete(j.conns, c)
	j.mu.Unlock()
	c.Close()
}

// forward passes each connection on l to addr outside.
func (j *Jail) forward(l net.Listener, addr string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			if !j.track(c) {
				return
			}
			defer j.untrack(c)
			up, err := j.dial(context.Background(), "tcp", addr)
			if err != nil {
				return
			}
			defer up.Close()
			splice(c, up, nil)
		}()
	}
}

// splice copies both ways until both ends are done. pre is sent to b first.
func splice(a, b net.Conn, pre []byte) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if len(pre) > 0 {
			if _, err := b.Write(pre); err != nil {
				return
			}
		}
		io.Copy(b, a)
		closeWrite(b)
	}()
	io.Copy(a, b)
	closeWrite(a)
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// check says why the proxy refuses target (host:port), or "".
func (j *Jail) check(target string) string {
	why, _ := j.vet(target)
	return why
}

// vet says why the proxy refuses target (host:port), or "" and the address
// to dial: target as it is, or for a host an open proxy lets through, the
// address it checked, so a name cannot answer differently when dialled.
func (j *Jail) vet(target string) (why, dial string) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || port == "" {
		return "the address is not host:port", ""
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, p := range j.spec.Pass {
		if strings.EqualFold(p, net.JoinHostPort(host, port)) {
			return "", target
		}
	}
	if Allowed(j.spec.Allow, host) {
		return "", target
	}
	if j.spec.Open {
		ip := public(host)
		if ip == nil {
			return host + " is not on the public internet, which is all an open proxy reaches; name it in [network] allow", ""
		}
		return "", net.JoinHostPort(ip.String(), port)
	}
	return host + " is not in [network] allow of permissions.toml", ""
}

// public says host is, and resolves only to, addresses of the public
// internet: global unicast, not private (RFC 1918, fc00::/7), not shared
// (100.64.0.0/10, as Tailscale uses), and none of this machine's own. The
// one rule an open proxy keeps: whatever is local, on this machine or its
// networks, is reached only through Forward, Pass or allow. A name that
// does not resolve is not public. It returns the address to dial, the
// first one checked, or nil when host is not public.
func public(host string) net.IP {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addrs) == 0 {
			return nil
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	own, _ := net.InterfaceAddrs()
	for _, ip := range ips {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || shared.Contains(ip) {
			return nil
		}
		for _, a := range own {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
				return nil
			}
		}
	}
	return ips[0]
}

// shared is the shared address space, 100.64.0.0/10 (RFC 6598).
var shared = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// Allowed says whether host is in allow: the same name, or a name under an
// entry that starts with "*.". Case does not matter.
func Allowed(allow []string, host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, a := range allow {
		a = strings.ToLower(a)
		if rest, ok := strings.CutPrefix(a, "*."); ok {
			if strings.HasSuffix(host, "."+rest) {
				return true
			}
			continue
		}
		if host == a {
			return true
		}
	}
	return false
}

// refuse answers 403 and reports the refusal.
func (j *Jail) refuse(w http.ResponseWriter, target, why string) {
	if j.spec.Refused != nil {
		j.spec.Refused(target, why)
	}
	http.Error(w, "agentbus: "+why, http.StatusForbidden)
}

// proxyHTTP is the proxy: CONNECT for a tunnel, an absolute URL for plain
// HTTP.
func (j *Jail) proxyHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		why, addr := j.vet(r.Host)
		if why != "" {
			j.refuse(w, r.Host, why)
			return
		}
		j.tunnel(w, r, addr)
		return
	}
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "agentbus: this is a proxy; send it CONNECT or an absolute URL", http.StatusBadRequest)
		return
	}
	target := hostPort(r.URL)
	why, addr := j.vet(target)
	if why != "" {
		j.refuse(w, target, why)
		return
	}
	j.plain.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), dialTo{}, addr)))
}

// dialTo is the context key of the address a plain HTTP request is dialled
// at: the one vet checked.
type dialTo struct{}

// tunnel connects to addr, the address vet checked for r.Host, and passes
// bytes both ways.
func (j *Jail) tunnel(w http.ResponseWriter, r *http.Request, addr string) {
	up, err := j.dial(r.Context(), "tcp", addr)
	if err != nil {
		http.Error(w, "agentbus: the host could not be reached", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "agentbus: the connection cannot be tunnelled", http.StatusInternalServerError)
		return
	}
	c, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if !j.track(c) {
		up.Close()
		return
	}
	defer j.untrack(c)
	defer up.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	splice(c, up, buffered(buf))
}

// buffered is what the client sent after its CONNECT that the server read
// already.
func buffered(rw *bufio.ReadWriter) []byte {
	n := rw.Reader.Buffered()
	if n == 0 {
		return nil
	}
	b, _ := rw.Reader.Peek(n)
	return b
}

// hostPort is u's host:port, the port from its scheme when it names none.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

var errUnsupported = errors.New("network control is Linux-only")
