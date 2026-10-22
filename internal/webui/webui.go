// Package webui serves the console, the single-page app in web/. Its build
// (npm run build in web/) lands in dist/ and is embedded in the api binary,
// so the console and the API ship together and share one origin.
//
// Without a build (a plain go build, no Node) the console's paths answer
// with a short page that says how to build it; the API is unaffected.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Routes are the console's client-side routes: each serves the app, which
// then renders the screen for that path. Anything else is not the
// console's, so an unknown path stays a JSON 404 from the API router.
var Routes = []string{"/", "/sign-in", "/endpoints", "/deliveries", "/send"}

// AssetPrefix is where the build puts its hashed files.
const AssetPrefix = "/assets/"

// securityHeaders lock the console down to its own origin: scripts, styles,
// fonts and API calls all come from here, nothing may frame it, and no
// referrer leaks the URL. React sets styles through the DOM (CSSOM), which
// style-src does not restrict, so no 'unsafe-inline' is needed.
func securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

// Handler serves the embedded build.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err) // the directory is embedded: it always exists
	}
	return New(sub)
}

// New serves the console from files: index.html for every client-side
// route, and the hashed assets under /assets/.
func New(files fs.FS) http.Handler {
	index, indexErr := fs.ReadFile(files, "index.html")
	assets := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header())
		if strings.HasPrefix(r.URL.Path, AssetPrefix) {
			// Clean so ".." cannot climb out; FileServerFS also refuses it.
			if p := path.Clean(r.URL.Path); !strings.HasPrefix(p, AssetPrefix) {
				http.NotFound(w, r)
				return
			}
			// Hashed names: a changed file gets a new name, so cache for ever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			assets.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		if indexErr != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(notBuilt))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}

const notBuilt = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Hook Mini</title></head>
<body style="font-family:system-ui,sans-serif;background:#07090d;color:#e8ecf2;padding:48px">
<h1>The console is not built</h1>
<p>This binary was built without the web console. Build it with <code>npm ci &amp;&amp; npm run build</code> in <code>web/</code>, then rebuild the api, or use the Docker image, which does both. The API under <code>/v1</code> works either way.</p>
</body></html>`
