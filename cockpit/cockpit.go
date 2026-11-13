// Package cockpit serves the cockpit's built files at / of agentbus up and
// agentbus server, from the same address as the API.
//
// The files are the cockpit repository's `npm run build` output, copied into
// dist at release time (tools/pack-cockpit.sh, run by .goreleaser.yaml). Git
// keeps only dist/.keep, so a source build has no cockpit: it answers / with a
// plain page that says so, and the API works as before.
package cockpit

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Handler serves the cockpit packed into this binary, or the plain page when
// none is.
func Handler() *Site {
	sub, _ := fs.Sub(embedded, "dist")
	return New(sub)
}

// Packed reports whether this binary holds the cockpit's files.
func Packed() bool {
	sub, _ := fs.Sub(embedded, "dist")
	_, err := fs.Stat(sub, "index.html")
	return err == nil
}

// marker goes into index.html's head, so the cockpit knows agentbus itself
// served it: its API is its own origin, and a sign-in link may be waiting.
const marker = `<meta name="agentbus" content="served">`

// Site is the cockpit's files. Serves says which requests it answers; the
// caller answers the rest itself, as it did before there was a cockpit.
type Site struct {
	files fs.FS
	index []byte // index.html with the marker; nil: not packed
}

// New serves the built files in files: index.html at /, each file at its
// path, and index.html again for any other path without a file extension
// that a browser opens as a page (Accept: text/html), since the cockpit's
// pages (/settings, /runners) are its own routes. files without index.html
// serve the plain page at / only.
//
// The caller sends only GET and HEAD, and only paths that are not the API's,
// the docs', the relay's or a vendor's.
func New(files fs.FS) *Site {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return &Site{files: files}
	}
	return &Site{files: files, index: bytes.Replace(index, []byte("<head>"), []byte("<head>"+marker), 1)}
}

// Serves reports whether s answers r: / always; a file it has; and, for a
// browser opening a page, any other path without a file extension. An agent
// or a script that calls a path nobody claims is not served a page it would
// read as an answer.
func (s *Site) Serves(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case name == "":
		return true
	case s.index == nil:
		return false
	case s.file(name) != nil:
		return true
	default:
		return !strings.Contains(path.Base(name), ".") && wantsHTML(r)
	}
}

// file is the file at name, or nil. Dot files, such as .keep, are not served.
func (s *Site) file(name string) []byte {
	if name == "index.html" {
		return s.index
	}
	if !fs.ValidPath(name) || strings.HasPrefix(path.Base(name), ".") {
		return nil
	}
	data, err := fs.ReadFile(s.files, name)
	if err != nil {
		return nil
	}
	return data
}

// ServeHTTP answers a request Serves accepts; any other gets 404.
func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.Serves(r) {
		http.Error(w, "agentbus: no such cockpit path", http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case s.index == nil:
		page(w, r, []byte(NotPackedPage))
		return
	case name == "" || name == "index.html":
		page(w, r, s.index)
		return
	}
	data := s.file(name)
	if data == nil {
		page(w, r, s.index)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType(name))
	h.Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(name, "assets/") {
		// Vite names each file under assets/ by its content.
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

// wantsHTML reports whether r is a browser opening a page: its Accept names
// text/html, as every browser's navigation does.
func wantsHTML(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, _ := strings.Cut(part, ";")
		if strings.EqualFold(strings.TrimSpace(mt), "text/html") {
			return true
		}
	}
	return false
}

// page answers with an HTML page: index.html, or the plain one.
func page(w http.ResponseWriter, r *http.Request, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	// The sign-in link's code is in the page's URL: no other page may
	// frame it, and no request it makes names it in Referer.
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// NotPackedPage is what / answers in a build without the cockpit.
const NotPackedPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>AgentBus</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 40em; margin: 4em auto; padding: 0 1em; line-height: 1.5">
<h1>AgentBus is running</h1>
<p>The cockpit is not packed in this build. Release builds include it.</p>
<p>The API works: see <a href="/docs">/docs</a>. To use the cockpit from its own repository, run its dev server
(<code>npm run dev</code> in agentbus-cockpit) and connect it to this address.</p>
</body></html>
`

// types are the content types of the files a Vite build writes. Fixed here,
// not read from the system's MIME tables, so every machine serves the same.
var types = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".txt":   "text/plain; charset=utf-8",
	".wasm":  "application/wasm",
}

func contentType(name string) string {
	if t, ok := types[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
