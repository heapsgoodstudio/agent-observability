package server

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// distFS is the compiled-in Next.js static export. `all:` (rather than a
// bare `dist`) is required because Next's export writes an underscore-
// prefixed `_next/` asset directory, and go:embed's default patterns skip
// any path element starting with `_` or `.`. embed also requires at least
// one match at compile time, which the committed internal/server/dist/
// README.md placeholder guarantees even before `just build` has ever run
// for this checkout.
//
//go:embed all:dist
var distFS embed.FS

// embeddedRoot returns the dist/ subtree of distFS, rooted so a request for
// "/" maps to dist/index.html rather than /dist/index.html.
func embeddedRoot() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// distFS is always rooted at the fixed, committed "dist" directory,
		// so this can't fail; a panic here would mean the embed directive
		// itself is broken, not a runtime condition callers can recover from.
		panic(err)
	}
	return sub
}

// staticHandler implements the serving precedence: Options.StaticDir wins
// when set (dev/test override, unchanged); else embedded when it contains
// index.html (the built frontend); else a 503 telling the operator to build
// it. embedded is embeddedRoot() in production, swapped for a fake fs.FS in
// tests so they don't depend on whether a real `just build` has actually
// populated internal/server/dist/ on the machine running go test. Both real
// branches route through exportHandler so the route-fallback logic below
// lives once, over an fs.FS, rather than twice.
func staticHandler(o Options, embedded fs.FS) http.Handler {
	if o.StaticDir != "" {
		return exportHandler(os.DirFS(o.StaticDir))
	}
	if _, err := fs.Stat(embedded, "index.html"); err == nil {
		return exportHandler(embedded)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("frontend not built; run just build"))
	})
}

// exportHandler wraps http.FileServerFS(fsys) with the routing fallback a
// Next.js static export needs. The export writes each route as a page beside
// a directory of prefetch segments: index.html, health.html and health/,
// 404.html, _next/. A bare file server misses any app route on a hard load
// or reload (e.g. /health, /spend, /sessions?id=...): no file is literally
// named that, so it returns either net/http's plain-text 404 or, where the
// segment directory exists, a listing of that directory instead of the
// exported page.
//
// For a GET/HEAD request whose path has no extension and whose exact file
// is absent from fsys, or is a directory without an index.html, this serves
// "<path>.html" when it exists (200, text/html). Otherwise an absent path
// gets "404.html" with status 404, else the same plain 404
// http.FileServerFS already gives every other miss. A path carrying an
// extension (_next/ assets, segment files, favicon.ico), and any other exact
// file or directory hit (including "/", whose root always resolves), passes
// straight through to http.FileServerFS untouched. The query string, if any,
// plays no part in file resolution: only r.URL.Path is examined.
func exportHandler(fsys fs.FS) http.Handler {
	inner := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isFallbackCandidate(r) {
			name := fsName(r.URL.Path)
			fi, err := fs.Stat(fsys, name)
			pageless := err != nil || (fi.IsDir() && !statOK(fsys, path.Join(name, "index.html")))
			if htmlName := name + ".html"; pageless && statOK(fsys, htmlName) {
				inner.ServeHTTP(w, withPath(r, "/"+htmlName))
				return
			}
			if err != nil {
				if statOK(fsys, "404.html") {
					inner.ServeHTTP(&statusOverride{ResponseWriter: w, status: http.StatusNotFound}, withPath(r, "/404.html"))
					return
				}
			}
		}
		inner.ServeHTTP(w, r)
	})
}

// isFallbackCandidate reports whether r is the kind of request the export
// fallback applies to at all: a GET/HEAD for a path with no extension.
// Everything else (other methods, asset paths) is left for inner to handle
// exactly as it always has.
func isFallbackCandidate(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return path.Ext(r.URL.Path) == ""
}

// fsName turns a request path into the fs.FS-relative name http.FileServerFS
// would resolve it to: cleaned, leading slash stripped, "/" itself mapped to
// ".", the root every fs.FS always has.
func fsName(urlPath string) string {
	name := strings.TrimPrefix(path.Clean(urlPath), "/")
	if name == "" {
		return "."
	}
	return name
}

func statOK(fsys fs.FS, name string) bool {
	_, err := fs.Stat(fsys, name)
	return err == nil
}

// withPath returns a clone of r with its URL.Path replaced, so inner can be
// handed a different file (the "<path>.html" or "404.html" fallback) than
// the one originally requested, without disturbing the request otherwise:
// the query string rides along on the clone but plays no part in file
// resolution either way.
func withPath(r *http.Request, p string) *http.Request {
	r2 := r.Clone(r.Context())
	r2.URL.Path = p
	r2.URL.RawPath = ""
	return r2
}

// statusOverride forces every response written through it to status,
// regardless of what the wrapped handler tries to write; used so
// http.FileServerFS can serve 404.html's actual bytes and Content-Type
// (still set normally via Header(), since only the status code passed to
// WriteHeader is substituted) while the response still reads as a 404.
//
// The one exception is http.StatusNotModified: a conditional request
// (If-Modified-Since/If-None-Match) against 404.html is answered by
// http.FileServerFS's own precondition check with a bodyless WriteHeader(304)
// before it ever gets to Write the file. Substituting status there would
// produce a 404 with an empty body and no Content-Type, a malformed
// response the client can't make sense of; passing 304 through unchanged is
// coherent instead, since it just means the client's cached copy of the 404
// page is still good.
type statusOverride struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusOverride) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if status == http.StatusNotModified {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.ResponseWriter.WriteHeader(w.status)
}

func (w *statusOverride) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(w.status)
	}
	return w.ResponseWriter.Write(b)
}
