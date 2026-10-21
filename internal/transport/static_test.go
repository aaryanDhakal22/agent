package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// staticMux is the real route table on a bare Server: the static + UI routes
// need no dependencies.
func staticMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	(&Server{}).register(mux)
	return mux
}

func get(t *testing.T, mux http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestStaticAssets(t *testing.T) {
	mux := staticMux(t)
	for _, tc := range []struct {
		path, ctype, cache string
	}{
		{"/static/app.js", "text/javascript; charset=utf-8", "no-cache"},
		{"/static/alpine.min.js", "text/javascript; charset=utf-8", "no-cache"},
		{"/static/app.css", "text/css; charset=utf-8", "no-cache"},
		{"/static/fonts/inter-latin-400-normal.woff2", "font/woff2", "public, max-age=31536000, immutable"},
		{"/static/fonts/jetbrains-mono-latin-800-normal.woff2", "font/woff2", "public, max-age=31536000, immutable"},
	} {
		rec := get(t, mux, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", tc.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != tc.ctype {
			t.Errorf("%s: Content-Type %q, want %q", tc.path, got, tc.ctype)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("%s: Cache-Control %q, want %q", tc.path, got, tc.cache)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", tc.path)
		}
		// The ETag makes the no-cache revalidation a 304.
		tag := rec.Header().Get("ETag")
		if tag == "" {
			t.Errorf("%s: no ETag", tc.path)
			continue
		}
		if again := get(t, mux, tc.path, "If-None-Match", tag); again.Code != http.StatusNotModified {
			t.Errorf("%s: revalidation status %d, want 304", tc.path, again.Code)
		}
	}
}

// A missing asset or a directory is a 404 — never the SPA page, never a listing.
func TestStaticMissingAndDirectories404(t *testing.T) {
	mux := staticMux(t)
	for _, p := range []string{"/static/", "/static/fonts", "/static/fonts/", "/static/missing.js", "/static/fonts/nope.woff2"} {
		rec := get(t, mux, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "<html") || strings.Contains(body, "<a href") {
			t.Errorf("%s: served a page or a listing: %.80q", p, body)
		}
	}
}

// The SPA catch-all still serves index.html, which loads the assets by path.
func TestIndexServedAndReferencesAssets(t *testing.T) {
	mux := staticMux(t)
	rec := get(t, mux, "/some/client/route")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("index: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{"Quicc POS", `src="/static/app.js"`, `src="/static/alpine.min.js"`, `href="/static/app.css"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	// app.js must register its component before Alpine boots: both deferred,
	// app.js first.
	if strings.Index(body, "/static/app.js") > strings.Index(body, "/static/alpine.min.js") {
		t.Error("app.js must load before alpine.min.js")
	}
	if rec := get(t, mux, "/api/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unmatched /api GET: %d, want 404", rec.Code)
	}
}
