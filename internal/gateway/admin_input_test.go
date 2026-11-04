// Admin API input bounds and allowed operations.

package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Bodies are bounded and strictly decoded, and
// refused input never reaches a reply or an event unescaped.
func TestAdminInputBounds(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	before := e.status()
	key := 0
	put := func(path, body string, header ...string) int {
		key++
		header = append([]string{"If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("input-bounds-%04d", key)}, header...)
		method := http.MethodPut
		if path == "/admin/candidates" {
			method = http.MethodPost
		}
		code, _, _ := e.harnessCall(method, path, body, header...)
		return code
	}
	cases := []struct {
		name, path, body string
		header           []string
		want             int
	}{
		{"oversized", "/admin/blocked-accounts/x", `{"reason":"` + strings.Repeat("R", 5000) + `"}`, nil, http.StatusRequestEntityTooLarge},
		{"text body", "/admin/blocked-accounts/x", `{"reason":"test containment"}`, []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType},
		{"repeated member", "/admin/blocked-accounts/x", `{"reason":"x","reason":"y"}`, nil, http.StatusBadRequest},
		{"case variant", "/admin/blocked-accounts/x", `{"Reason":"x"}`, nil, http.StatusBadRequest},
		{"unknown field", "/admin/blocked-accounts/x", `{"status":"active","roles":[],"admin":true}`, nil, http.StatusBadRequest},
		{"trailing data", "/admin/blocked-accounts/x", `{"reason":"test containment"} {}`, nil, http.StatusBadRequest},
		{"wrong type", "/admin/blocked-accounts/x", `{"reason":["WRITER"]}`, nil, http.StatusBadRequest},
		{"control character in id", "/admin/blocked-accounts/a%0Ab", `{"reason":"test containment"}`, nil, http.StatusBadRequest},
		{"overlong id", "/admin/blocked-accounts/" + strings.Repeat("s", 129), `{"reason":"test containment"}`, nil, http.StatusBadRequest},
		{"malformed If-Match", "/admin/candidates", `{"policy":{}}`, []string{"If-Match", `W/"r1"`}, http.StatusBadRequest},
		{"short idempotency key", "/admin/blocked-accounts/x", `{"reason":"test containment"}`, []string{"Idempotency-Key", "short"}, http.StatusBadRequest},
		{"invalid idempotency key", "/admin/blocked-accounts/x", `{"reason":"test containment"}`, []string{"Idempotency-Key", "has spaces in it ok?"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if code := put(tc.path, tc.body, tc.header...); code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, code, tc.want)
		}
	}
	code, _, _ := e.harnessCall(http.MethodPost, "/admin/candidates", `{"policy":{}}`, "Idempotency-Key", "input-bounds-no-if-match")
	if code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match: %d", code)
	}

	// Secret-looking input is neither echoed nor recorded.
	privateKey := "-----BEGIN PRIVATE KEY-----MIIEvQIBADANBgkqhkiG9w0BAQEFAASC-----END PRIVATE KEY-----"
	token := issuer.tokenFor(t, "alice")
	if code := put("/admin/blocked-accounts/x", `{"status":"active","roles":[],"key":"`+privateKey+`"}`); code != http.StatusBadRequest {
		t.Errorf("unknown field holding a key: %d", code)
	}
	e.login(harnessAdmin, token)
	e.adminSend(http.MethodGet, "/admin/blocked-accounts/x%0A%7B%22kind%22%3A%22forged%22%7D", "")
	if after := e.status(); after.Revision != before.Revision || after.Generation != before.Generation {
		t.Fatalf("refused input changed the state: %+v", after)
	}
	for _, ev := range e.auditEvents() { // fails on any line that is not one JSON record
		if ev["kind"] == "forged" {
			t.Fatal("an injected record appeared in the audit log")
		}
	}
	assertAbsent(t, e, "BEGIN PRIVATE KEY", token)
}

// Only the implemented administration operations exist. No route
// other than the confirmed-activation lifecycle activates a policy: the
// override route takes typed override requests only, and refuses a whole
// policy. No recommendation route answers as if it worked.
func TestOnlyVersionOperationsExist(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	before := e.status()
	probes := []struct{ method, path string }{
		{http.MethodPost, "/admin/candidates/1/activate"}, {http.MethodPost, "/admin/candidates/1/confirm"},
		{http.MethodPut, "/admin/candidates/1"}, {http.MethodPost, "/admin/confirm"},
		{http.MethodPost, "/admin/rollback"}, {http.MethodPost, "/admin/cpa/confirm"}, {http.MethodPost, "/admin/tpo"},
		{http.MethodPut, "/admin/overrides/1"}, {http.MethodPatch, "/admin/overrides/1"}, {http.MethodGet, "/admin/spa"},
		{http.MethodPost, "/admin/recommendations"}, {http.MethodPost, "/admin/backup"}, {http.MethodPost, "/admin/restore"},
		{http.MethodPut, "/admin/policy"}, {http.MethodPost, "/admin/policy"}, {http.MethodPost, "/admin/revisions"},
		{http.MethodPut, "/admin/roles/READER"}, {http.MethodPost, "/admin/permits"}, {http.MethodPost, "/admin/admins"},
		{http.MethodPost, "/admin/install"}, {http.MethodPost, "/publish"}, {http.MethodPost, "/test/publish"},
		{http.MethodPost, "/load"}, {http.MethodGet, "/config/"},
	}
	policy := string(mustJSON(map[string]any{"policy": e.protectedPolicy()}))
	for _, probe := range probes {
		code, _, _ := e.harnessCall(probe.method, probe.path, policy, "If-Match", `"r1"`, "Idempotency-Key", "future-route-probe-1")
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d", probe.method, probe.path, code)
		}
	}
	if code, body, _ := e.harnessCall(http.MethodPost, "/admin/overrides", policy, "If-Match", `"r1"`, "Idempotency-Key", "future-route-probe-2"); code != http.StatusBadRequest ||
		!strings.Contains(body, "bad_request") {
		t.Errorf("a whole policy sent as an override: %d %s", code, body)
	}
	if after := e.status(); after.Generation != before.Generation {
		t.Fatalf("a probe changed the generation: %+v", after)
	}
}
