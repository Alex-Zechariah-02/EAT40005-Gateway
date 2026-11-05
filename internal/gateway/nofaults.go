//go:build !faultinject

// Production builds register no test routes.

package gateway

import "net/http"

// registerTestRoutes adds nothing: production binaries have no test hooks.
func registerTestRoutes(*Controller, *http.ServeMux) {}
