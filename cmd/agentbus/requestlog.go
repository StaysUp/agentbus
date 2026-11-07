package main

// One service.log line per request: method, path without its query, status,
// duration, and for an error the service's own error text. Never a header, a
// body, a query or a token: the query can carry a cursor today and anything
// tomorrow, and headers carry the vendor sign-in and our token. A vendor's
// error reply is a body too, so a gateway path logs its status only.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/staysup/agentbus/api"
	"github.com/staysup/agentbus/logfile"
)

// maxErrorBody bounds what is read of an API error reply to find its error
// text. API errors are short fixed wording.
const maxErrorBody = 4 << 10

func logRequests(l *logfile.Log, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		begun := time.Now()
		rw := &statusRecorder{ResponseWriter: w, ours: api.Reserved(r)}
		next.ServeHTTP(rw, r)
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		line := "request method=" + r.Method + " path=" + r.URL.EscapedPath() + " status=" + strconv.Itoa(status) +
			" duration=" + time.Since(begun).Round(time.Microsecond).String()
		if msg := rw.errorText(); msg != "" {
			l.Printf("%s error=%q", line, msg)
			return
		}
		l.Printf("%s", line)
	})
}

// statusRecorder notes a response's status and, for an error from the API itself,
// enough of its body to read the fixed error text.
type statusRecorder struct {
	http.ResponseWriter
	ours   bool // the API answers this request, so its error body is our own wording
	status int
	body   bytes.Buffer
	local  string // the router's own error text
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.ours && r.status >= 400 && r.body.Len() < maxErrorBody {
		n := min(len(p), maxErrorBody-r.body.Len())
		r.body.Write(p[:n])
	}
	return r.ResponseWriter.Write(p)
}

// Flush keeps streams streaming: the API asserts http.Flusher.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.NewResponseController reach the connection, for read
// deadlines and the gateway's flushes.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) errorText() string {
	if r.local != "" {
		return r.local
	}
	if r.status < 400 || r.body.Len() == 0 {
		return ""
	}
	var v struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(r.body.Bytes(), &v) != nil {
		return ""
	}
	return v.Error
}

// localError answers with the router's own fixed text and notes it for the
// request's log line.
func localError(w http.ResponseWriter, msg string, code int) {
	if r, ok := w.(*statusRecorder); ok {
		r.local = msg
	}
	http.Error(w, msg, code)
}
