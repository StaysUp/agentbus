package gateway

// Why an upstream call failed, for the service log. The agent's reply stays
// fixed words (upstreamFailure); the log says which error it was, so a 502
// can be traced to the vendor or to the gateway's own transport. Go's error
// text never reaches the log: it can quote a header or a URL. Only the error
// types and a reason from a fixed list do.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"syscall"
)

// maxCauseDepth bounds how many wrapped errors class names.
const maxCauseDepth = 6

// causeOf answers an upstream error's class, the Go types of it and the
// errors it wraps, outermost first, such as "*net.OpError>*os.SyscallError",
// and its reason, one word from a fixed list.
func causeOf(err error) (class, reason string) {
	var types []string
	for e := err; e != nil && len(types) < maxCauseDepth; {
		types = append(types, fmt.Sprintf("%T", e))
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			e = u.Unwrap()
		case interface{ Unwrap() []error }:
			if l := u.Unwrap(); len(l) > 0 {
				e = l[0]
			} else {
				e = nil
			}
		default:
			e = nil
		}
	}
	return strings.Join(types, ">"), reasonOf(err)
}

// reasonOf is one fixed word for what err is, "other" when none fits.
func reasonOf(err error) string {
	var dns *net.DNSError
	var ne net.Error
	var op *net.OpError
	var rec tls.RecordHeaderError
	var alert tls.AlertError
	var verify *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var proto textproto.ProtocolError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &rec), errors.As(err, &alert), errors.As(err, &verify),
		errors.As(err, &unknownCA), errors.As(err, &hostname):
		return "tls"
	case errors.As(err, &proto):
		// The vendor's reply was not HTTP the transport could parse.
		return "protocol"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.As(err, &op):
		// Op is Go's own word for the step: dial, read or write.
		return op.Op
	}
	return "other"
}
