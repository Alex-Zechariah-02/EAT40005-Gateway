// Dashboard file serving on the Admin API listener.

package gateway

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
)

// assetTypes are the only asset types served, each with a fixed content type
// rather than one guessed from the host's registry. The only page is the
// index.
var assetTypes = map[string]string{
	".js":  "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8",
	".svg": "image/svg+xml",
}

// uiPolicy lets a frontend, the dashboard or the website, load only its own
// scripts and styles and call only its own origin, and forbids framing, so
// injected markup cannot run script or exfiltrate data.
const uiPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// serveUI serves one dashboard file. The files hold no data, so no session is
// needed; everything the dashboard shows comes from the Admin API.
func (c *Controller) serveUI(w http.ResponseWriter, r *http.Request) {
	name, contentType := "index.html", "text/html; charset=utf-8"
	if file := r.PathValue("file"); file != "" {
		name, contentType = "assets/"+file, assetTypes[path.Ext(file)]
	}
	if contentType == "" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	serveFrontendFile(w, r, c.ui, name, contentType)
}

// serveFrontendFile answers with one file of a built frontend under the
// frontends' content policy, or 404 when it cannot be read.
func serveFrontendFile(w http.ResponseWriter, r *http.Request, files fs.FS, name, contentType string) {
	data, err := fs.ReadFile(files, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", uiPolicy)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(data)
}

// frontendFiles returns the built frontend in dir, which must hold its
// index.html; setting names the configuration field in the error.
func frontendFiles(setting, dir string) (fs.FS, error) {
	files := os.DirFS(dir)
	if info, err := fs.Stat(files, "index.html"); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %q must be a directory holding index.html", setting, dir)
	}
	return files, nil
}
