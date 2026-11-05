//go:build faultinject

// Test routes for fault-injection builds.

package gateway

import (
	"net/http"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// registerTestRoutes adds the test publication hook: a whole-policy
// publication for regression tests of the publisher. Production binaries have
// no such route, because whole-policy activation belongs to the confirmed
// activation lifecycle. The hook still needs an administrator session with
// manage_policy, the admin origin and the CSRF token.
func registerTestRoutes(c *Controller, mux *http.ServeMux) {
	c.route(mux, "POST /test/publish", permManagePolicy, func(w http.ResponseWriter, r *http.Request, who caller) {
		var change Change
		if !decodeBody(w, r, policy.MaxPolicyBytes, &change) {
			return
		}
		outcome, status := c.Publish(r.Context(), who.admin.Name, change)
		writeJSON(w, status, outcome)
	})
}
