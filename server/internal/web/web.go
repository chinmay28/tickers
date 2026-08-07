// Package web serves the client.
//
// The whole client — HTML, CSS, one ES module, the icons and the developer
// badge — is compiled into the binary with go:embed. That is what makes the
// deployable artifact a single file with no runtime dependencies: `go build`
// is the entire front-end toolchain, and a Raspberry Pi install never installs
// Node.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed assets
var embedded embed.FS

// FS returns the embedded client as a filesystem rooted at the asset dir.
func FS() (fs.FS, error) { return fs.Sub(embedded, "assets") }

// Handler serves the client.
//
// If dir is non-empty it is served from disk instead of the embedded copy —
// that is `--web-dist`, which exists so the client can be edited and reloaded
// without recompiling the server.
func Handler(dir string) (http.Handler, error) {
	if dir != "" {
		disk := http.Dir(dir)
		return newHandler(disk.Open, disk), nil
	}
	sub, err := FS()
	if err != nil {
		return nil, err
	}
	fsys := http.FS(sub)
	return newHandler(fsys.Open, fsys), nil
}

type opener func(string) (http.File, error)

// newHandler wraps a filesystem with the two behaviours a single-page client
// needs: cache headers that keep the shell fresh, and an index.html fallback.
func newHandler(open opener, fsys http.FileSystem) http.Handler {
	fileServer := http.FileServer(fsys)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Clean("/" + r.URL.Path)

		// Unknown paths fall back to the shell so a deep link (or a reload on
		// #/settings) renders the app rather than a 404. Only paths that
		// clearly want a file are excluded, so a genuinely missing asset still
		// 404s instead of silently returning HTML.
		if name != "/" && path.Ext(name) == "" {
			if f, err := open(name); err != nil {
				serveShell(w, r, open)
				return
			} else {
				f.Close()
			}
		}

		// The shell and the client code must never be served stale: an upgrade
		// that leaves a cached app.js talking to a new API is the exact
		// failure the non-disruptive upgrade story is meant to prevent. The
		// icons and the badge are content-stable and can be cached.
		switch {
		case name == "/" || strings.HasSuffix(name, ".html") ||
			strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".css") ||
			strings.HasSuffix(name, ".webmanifest"):
			w.Header().Set("Cache-Control", "no-cache")
		default:
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}

		fileServer.ServeHTTP(w, r)
	})
}

func serveShell(w http.ResponseWriter, r *http.Request, open opener) {
	f, err := open("/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", stat.ModTime(), f)
}
