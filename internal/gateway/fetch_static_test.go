package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestFetchStaticDiscardsClientIdentityHeaders(t *testing.T) {
	request := httptest.NewRequest("GET", "http://gateway/", nil)
	request.RequestURI = "/"
	request.Header.Set("X-Request-Id", "client-value")
	request.Header.Set("X-Gateway-Subject", "forged")
	request.Header.Set("X-Gateway-Extra", "forged")
	files := fstest.MapFS{"index.html": {Data: []byte("Fetch")}}
	dataPlane(http.NotFoundHandler(), files).ServeHTTP(httptest.NewRecorder(), request)
	for _, name := range []string{"X-Request-Id", "X-Gateway-Subject", "X-Gateway-Extra"} {
		if request.Header.Get(name) != "" {
			t.Errorf("static request retained %s", name)
		}
	}
}

// Catch accidental SPA fallback over an API or acceptance of a decoded target.
func TestFetchWebsiteTargets(t *testing.T) {
	pages := []string{
		"/", "/food", "/mart", "/store/MER-001", "/item/ITM-001", "/cart", "/checkout",
		"/my-orders", "/my-orders/ORD-1001", "/help", "/help/TKT-1001", "/signin",
		"/merchant", "/rider", "/support", "/ops", "/lab",
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, target := range pages {
			t.Run(method+target, func(t *testing.T) {
				r := httptest.NewRequest(method, target+"?from=link", nil)
				name, contentType, ok := websiteFile(r)
				if !ok || name != "index.html" || contentType != "text/html; charset=utf-8" {
					t.Fatalf("page target returned %q %q %t", name, contentType, ok)
				}
			})
		}
		for file, contentType := range map[string]string{
			"app-1.js": "text/javascript", "app.css": "text/css", "logo.svg": "image/svg+xml", "MER-001.webp": "image/webp",
		} {
			name, gotType, ok := websiteFile(httptest.NewRequest(method, "/assets/"+file, nil))
			if !ok || name != "assets/"+file || gotType != contentType {
				t.Errorf("asset %s: %q %q %t", file, name, gotType, ok)
			}
		}
	}
	for _, target := range []string{
		"/orders", "/orders/ORD-1001", "/orders/mine", "/orders/quote", "/support/tickets", "/internal/stock",
		"/inventory/search", "/accounts/login", "/admin/status", "/demo/", "/demo/assets/app.js",
		"/food/", "/store", "/store/", "/store/a/b", "/my-orders/", "/help/a/b", "/signin/extra",
		"/assets/", "/assets/app.js.map", "/assets/x.png", "/assets/x.jpg", "/assets/x.JS", "/assets/../app.js",
		"/assets/sub/app.js", "/assets/%61pp.js", "/assets/%2e%2e%2fapp.js", "/%66ood", "/store/%2f",
		"/store/.", "/item/..", "//food", "/food//", "/assets/..\\app.js",
	} {
		if _, _, ok := websiteFile(httptest.NewRequest(http.MethodGet, target, nil)); ok {
			t.Errorf("unapproved target served as website: %s", target)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		for _, target := range []string{"/", "/food", "/assets/app.js"} {
			if _, _, ok := websiteFile(httptest.NewRequest(method, target, nil)); ok {
				t.Errorf("unapproved method served as website: %s %s", method, target)
			}
		}
	}
}

// A page, an asset and a missing asset all retain a gateway-issued reference.
func TestFetchWebsiteResponseHeaders(t *testing.T) {
	files := fstest.MapFS{
		"index.html":          {Data: []byte("test page")},
		"assets/MER-001.webp": {Data: []byte("test image")},
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	handler := dataPlane(fallback, files)
	for _, target := range []string{"/", "/food", "/assets/MER-001.webp", "/assets/missing.js"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("X-Request-Id", "forged")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if id := w.Header().Get("X-Request-Id"); len(id) != 32 || id == "forged" {
			t.Errorf("%s has no generated reference: %q", target, id)
		}
		if strings.Contains(target, "missing") {
			if w.Code != http.StatusNotFound {
				t.Errorf("missing asset: %d", w.Code)
			}
			continue
		}
		if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") != uiPolicy || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: status %d headers %v", target, w.Code, w.Header())
		}
	}
}
