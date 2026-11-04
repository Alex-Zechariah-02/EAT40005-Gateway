// Status with an unreadable stored authority.

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A status read whose stored authority cannot be read says so, instead of
// omitting the confirmed and provisional revisions as though there were
// none; the facts that need no store read are still reported.
func TestStatusReportsUnreadableStoredAuthority(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	c, _ := e.own(e.controllerConfig())
	read := func(ctx context.Context) map[string]any {
		t.Helper()
		reply := httptest.NewRecorder()
		c.handleStatus(reply, httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/status", nil), caller{})
		var status map[string]any
		if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &status) != nil {
			t.Fatalf("status: %d %s", reply.Code, reply.Body)
		}
		return status
	}
	readable := read(t.Context())
	if readable["confirmed"] == nil || readable["stored_authority"] != nil {
		t.Fatalf("readable authority: %v", readable)
	}
	// A cancelled request makes the store read fail without touching the
	// database.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	unreadable := read(cancelled)
	if unreadable["stored_authority"] != "unreadable" || unreadable["confirmed"] != nil || unreadable["provisional"] != nil ||
		unreadable["state"] != readable["state"] || unreadable["admission"] != readable["admission"] || unreadable["audit_failed"] != readable["audit_failed"] {
		t.Fatalf("unreadable authority: %v, readable: %v", unreadable, readable)
	}
}
