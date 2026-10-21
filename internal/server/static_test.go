package server

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// Serving precedence (internal/server/server.go's Handler): Options.StaticDir
// wins when set (dev/test override, unchanged); else the embedded Next.js
// build when it has an index.html; else a 503 telling the operator to build
// it. staticHandler takes the embedded root as a parameter so these tests
// don't depend on whether a real `just build` has actually populated
// internal/server/dist/ on the machine running go test.

func getBody(t *testing.T, url string) (status int, body string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestStaticDirWinsOverEmbedded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>from StaticDir</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Embedded root has its own index.html too, so a pass here proves
	// StaticDir really takes precedence rather than merely being the only
	// option available.
	embedded := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<title>from embedded</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{StaticDir: dir}, embedded))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/")
	if status != http.StatusOK || !strings.Contains(body, "from StaticDir") {
		t.Fatalf("status = %d, body = %q, want 200 containing %q", status, body, "from StaticDir")
	}
}

func TestEmbeddedDistServesIndexWhenStaticDirEmpty(t *testing.T) {
	embedded := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<title>Agent Observability</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/")
	if status != http.StatusOK || !strings.Contains(body, "Agent Observability") {
		t.Fatalf("status = %d, body = %q, want 200 containing %q", status, body, "Agent Observability")
	}
}

func TestEmptyDistYields503(t *testing.T) {
	embedded := fstest.MapFS{} // no index.html: the frontend was never built
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/")
	want := "frontend not built; run just build"
	if status != http.StatusServiceUnavailable || !strings.Contains(body, want) {
		t.Fatalf("status = %d, body = %q, want 503 containing %q", status, body, want)
	}
}

// TestEmbeddedRootAlwaysResolves confirms embeddedRoot() (the real,
// compiled-in internal/server/dist/) always resolves to a valid fs.FS
// rooted at dist/, even when it only contains the committed README.md
// placeholder: fs.Stat for a missing file is a normal, non-fatal error, not
// a panic.
func TestEmbeddedRootAlwaysResolves(t *testing.T) {
	root := embeddedRoot()
	if _, err := fs.Stat(root, "README.md"); err != nil {
		t.Fatalf("stat README.md in embedded root: %v", err)
	}
}

// Static-export route fallback: Next's static export writes each route as a
// page (health.html) and, since Next 16, a directory of prefetch segments
// (health/), beside index.html, 404.html and _next/. A hard load or reload
// of an app route like /health has no file literally named "health" and,
// served by a bare http.FileServer, misses into net/http's plain-text 404 or
// lists the segment directory. These tests cover the fallback: an
// extension-less path whose exact file is absent, or is a directory with no
// index.html, tries "<path>.html", and an absent one then "404.html", before
// giving up; a path carrying an extension (assets, favicon.ico) is untouched
// either way.

func TestExtensionlessHitServesMatchingHTMLFile(t *testing.T) {
	embedded := fstest.MapFS{
		"index.html":  &fstest.MapFile{Data: []byte("<title>index</title>")},
		"health.html": &fstest.MapFile{Data: []byte("<title>health page</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/health")
	if status != http.StatusOK || !strings.Contains(body, "health page") {
		t.Fatalf("status = %d, body = %q, want 200 containing %q", status, body, "health page")
	}
}

// Next 16's export also writes a health/ directory of prefetch segments
// beside health.html. The directory has no index.html, so the page must win
// over it; serving the directory gives a bare listing instead of the app.
func TestExtensionlessHitPrefersHTMLFileOverSegmentDirectory(t *testing.T) {
	embedded := fstest.MapFS{
		"index.html":              &fstest.MapFile{Data: []byte("<title>index</title>")},
		"health.html":             &fstest.MapFile{Data: []byte("<title>health page</title>")},
		"health/__next._tree.txt": &fstest.MapFile{Data: []byte("health tree segment")},
		"404.html":                &fstest.MapFile{Data: []byte("<title>not found here</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	for _, p := range []string{"/health", "/health/"} {
		status, body := getBody(t, srv.URL+p)
		if status != http.StatusOK || !strings.Contains(body, "health page") {
			t.Fatalf("%s: status = %d, body = %q, want 200 containing %q", p, status, body, "health page")
		}
	}
	status, body := getBody(t, srv.URL+"/health/__next._tree.txt")
	if status != http.StatusOK || body != "health tree segment" {
		t.Fatalf("segment: status = %d, body = %q, want 200 with the segment file", status, body)
	}
}

func TestExtensionlessHitIgnoresQueryString(t *testing.T) {
	embedded := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<title>index</title>")},
		"sessions.html": &fstest.MapFile{Data: []byte("<title>sessions page</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/sessions?id=abc123")
	if status != http.StatusOK || !strings.Contains(body, "sessions page") {
		t.Fatalf("status = %d, body = %q, want 200 containing %q", status, body, "sessions page")
	}
}

func TestExtensionlessMissServes404Page(t *testing.T) {
	embedded := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<title>index</title>")},
		"404.html":   &fstest.MapFile{Data: []byte("<title>not found here</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/nonexistent")
	if status != http.StatusNotFound || !strings.Contains(body, "not found here") {
		t.Fatalf("status = %d, body = %q, want 404 containing %q", status, body, "not found here")
	}
}

func TestExtensionlessMissWithoutCustom404FallsBackToPlain404(t *testing.T) {
	embedded := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<title>index</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	status, _ := getBody(t, srv.URL+"/nonexistent")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestAssetPathWithExtensionUntouchedOnHit(t *testing.T) {
	embedded := fstest.MapFS{
		"index.html":          &fstest.MapFile{Data: []byte("<title>index</title>")},
		"_next/static/app.js": &fstest.MapFile{Data: []byte("console.log('app')")},
		"404.html":            &fstest.MapFile{Data: []byte("<title>not found here</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/_next/static/app.js")
	if status != http.StatusOK || !strings.Contains(body, "console.log") {
		t.Fatalf("status = %d, body = %q, want 200 containing %q", status, body, "console.log")
	}
}

func TestAssetPathWithExtensionUntouchedOnMiss(t *testing.T) {
	embedded := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<title>index</title>")},
		"404.html":   &fstest.MapFile{Data: []byte("<title>not found here</title>")},
	}
	srv := httptest.NewServer(staticHandler(Options{}, embedded))
	defer srv.Close()

	// A missing asset that carries an extension must NOT fall back to
	// 404.html: the fallback is for extension-less paths only, so this
	// stays net/http's own plain-text 404, not our custom page.
	status, body := getBody(t, srv.URL+"/_next/static/missing.js")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if strings.Contains(body, "not found here") {
		t.Fatalf("body = %q, want the plain net/http 404, not the custom 404.html fallback", body)
	}
}

func TestStaticDirExtensionlessHitServesMatchingHTMLFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>index</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "health.html"), []byte("<title>health page</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(staticHandler(Options{StaticDir: dir}, fstest.MapFS{}))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/health")
	if status != http.StatusOK || !strings.Contains(body, "health page") {
		t.Fatalf("status = %d, body = %q, want 200 containing %q", status, body, "health page")
	}
}

func TestStaticDirExtensionlessMissServes404Page(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>index</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "404.html"), []byte("<title>not found here</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(staticHandler(Options{StaticDir: dir}, fstest.MapFS{}))
	defer srv.Close()

	status, body := getBody(t, srv.URL+"/nonexistent")
	if status != http.StatusNotFound || !strings.Contains(body, "not found here") {
		t.Fatalf("status = %d, body = %q, want 404 containing %q", status, body, "not found here")
	}
}

// TestExtensionlessMiss304PassesThrough covers a real bug found in review: a
// conditional request against the 404.html fallback (If-Modified-Since from
// an earlier response's Last-Modified) makes http.FileServerFS's own
// precondition check short-circuit to a bodyless WriteHeader(304), before it
// ever calls Write. statusOverride must let that 304 through unchanged
// rather than substituting its usual 404: forcing 404 there would produce a
// wire response with status 404, an empty body, and no Content-Type,
// self-contradictory rather than merely wrong. A 304 for the 404 page is
// coherent instead (the client's cached copy of it is still good), which is
// why passthrough was chosen over the alternative of stripping the
// conditional headers so every request gets a fresh, full 404.
func TestExtensionlessMiss304PassesThrough(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>index</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "404.html"), []byte("<title>not found here</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(staticHandler(Options{StaticDir: dir}, fstest.MapFS{}))
	defer srv.Close()

	// First request: a genuine 404 with a body, stamped with Last-Modified
	// from 404.html's real mtime on disk (an os.DirFS-backed file server
	// always carries one; no need to fake it).
	resp1, err := http.Get(srv.URL + "/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	body1, err := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp1.StatusCode != http.StatusNotFound || len(body1) == 0 {
		t.Fatalf("first request: status = %d, body len = %d, want 404 with a body", resp1.StatusCode, len(body1))
	}
	lastMod := resp1.Header.Get("Last-Modified")
	if lastMod == "" {
		t.Fatal("first response carried no Last-Modified header to condition the second request on")
	}

	// Second request: conditional on that Last-Modified. The file hasn't
	// changed, so http.FileServerFS wants to answer 304 with no body; that
	// must survive statusOverride rather than being clobbered back to 404.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/nonexistent", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-Modified-Since", lastMod)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body2, err := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("second request: status = %d, want %d (StatusNotModified)", resp2.StatusCode, http.StatusNotModified)
	}
	if len(body2) != 0 {
		t.Fatalf("second request body = %q, want empty (a 304 has no body)", body2)
	}
}
