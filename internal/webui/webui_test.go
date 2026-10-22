package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var built = fstest.MapFS{
	"index.html":           {Data: []byte("<!doctype html><title>console</title>")},
	"assets/app-1a2b3c.js": {Data: []byte("console.log(1)")},
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestEveryRouteServesTheAppWithItsSecurityHeaders(t *testing.T) {
	h := New(built)
	for _, p := range Routes {
		rec := get(h, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>console</title>") {
			t.Fatalf("%s: %d %q, want the app", p, rec.Code, rec.Body.String())
		}
		csp := rec.Header().Get("Content-Security-Policy")
		directives := parseCSP(csp)
		// Each directive exactly: a substring check would accept
		// "font-src 'self' data:", which is how inlined fonts slip in.
		for name, want := range map[string]string{
			"default-src":     "'self'",
			"script-src":      "'self'",
			"style-src":       "'self'",
			"font-src":        "'self'",
			"connect-src":     "'self'",
			"frame-ancestors": "'none'",
			"base-uri":        "'none'",
			"object-src":      "'none'",
		} {
			if got, ok := directives[name]; !ok || got != want {
				t.Errorf("%s: CSP %s = %q, want %q (policy %q)", p, name, got, want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows inline or eval: %q", p, csp)
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing hardening headers: %v", p, rec.Header())
		}
		if rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: the app shell must be revalidated, Cache-Control %q", p, rec.Header().Get("Cache-Control"))
		}
	}
}

func TestAssetsAreCachedForEverAndCannotEscape(t *testing.T) {
	h := New(built)
	rec := get(h, "/assets/app-1a2b3c.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Fatalf("asset: %d %q", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("hashed asset Cache-Control = %q, want immutable", cc)
	}
	if rec := get(h, "/assets/missing.js"); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset: %d, want 404", rec.Code)
	}
	// Refused by the console itself: a 404, and no year-long cache on it.
	if rec := get(h, "/assets/../index.html"); rec.Code != http.StatusNotFound || strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("a path climbing out of /assets/: %d, Cache-Control %q; want 404, not cached", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestWithoutABuildTheConsoleSaysHowToBuildIt(t *testing.T) {
	rec := get(New(fstest.MapFS{".gitkeep": {}}), "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not built") || !strings.Contains(rec.Body.String(), "npm run build") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("the placeholder page has no CSP")
	}
}

// The embedded directory always exists (a placeholder is committed), so the
// real handler can always be built.
func TestEmbeddedHandlerBuilds(t *testing.T) {
	if rec := get(Handler(), "/"); rec.Code != http.StatusOK {
		t.Fatalf("GET / from the embedded build: %d", rec.Code)
	}
}

// parseCSP maps each directive of a policy to its source list.
func parseCSP(policy string) map[string]string {
	out := map[string]string{}
	for _, d := range strings.Split(policy, ";") {
		fields := strings.Fields(d)
		if len(fields) > 0 {
			out[fields[0]] = strings.Join(fields[1:], " ")
		}
	}
	return out
}
