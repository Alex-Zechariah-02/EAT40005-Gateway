// Refused override requests.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// A refused or malformed override request changes nothing: no row, no
// generation, no receipt. Refusals with an identity are recorded once;
// malformed requests are answered 400 without reaching the controller.
func TestOverrideRefusals(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	p.Operations[0].Rate = rate(4, 60) // read-a
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	_, readerPassword := e.addAdmin(binaries.plain, "reader", "read,read_history")
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	reader := e.mustLogin("reader", readerPassword)
	before := e.status()
	apiToken := issuer.tokenFor(t, "alice")

	refusals := []struct {
		name    string
		request any
		ifMatch string
		status  int
		reason  string
		field   string
	}{
		{"JWT bypass", `{"type":"exception","control":"jwt","operation":"read-a","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_CONTROL", "control"},
		{"validation bypass", `{"type":"exception","control":"validation","operation":"write-a","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_CONTROL", "control"},
		{"logging bypass", `{"type":"containment","control":"logging","operation":"read-a","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_CONTROL", "control"},
		{"upstream change", `{"type":"exception","control":"upstream","operation":"read-a","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_CONTROL", "control"},
		{"route allow", operationContainment("denied", 60), `"r1"`, 400, "INVALID_VALUE", "operation"},
		{"route exception", `{"type":"exception","control":"operation","operation":"denied","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_ACTION", "type"},
		{"unknown operation", operationContainment("delete-everything", 60), `"r1"`, 400, "UNKNOWN_REFERENCE", "operation"},
		{"unknown role", permitOverride(overrideException, "ADMIN", "write-a", 60), `"r1"`, 400, "UNKNOWN_REFERENCE", "role"},
		{"token as role", permitOverride(overrideException, apiToken, "write-a", 60), `"r1"`, 400, "INVALID_VALUE", "role"},
		{"record injection in a role", permitOverride(overrideException, "READER\r\n{\"kind\":\"tpo.created\",\"forged\":true}", "write-a", 60), `"r1"`, 400, "INVALID_VALUE", "role"},
		{"record injection in an operation", operationContainment("read-b\n{\"forged\":true}", 60), `"r1"`, 400, "INVALID_VALUE", "operation"},
		{"already granted", permitOverride(overrideException, "WRITER", "write-a", 60), `"r1"`, 400, "INVALID_VALUE", "role"},
		{"rate exception lowering", rateOverride(overrideException, "read-a", 2, 60), `"r1"`, 400, "INVALID_VALUE", "count"},
		{"rate on an unrated operation", rateOverride(overrideContainment, "write-a", 1, 60), `"r1"`, 400, "INVALID_VALUE", "operation"},
		{"unlimited rate", rateOverride(overrideException, "read-a", maxRateCount+1, 60), `"r1"`, 400, "INVALID_VALUE", "count"},
		{"no expiry", `{"type":"exception","control":"permit","operation":"write-a","role":"READER"}`, `"r1"`, 400, "INVALID_EXPIRY", "expires_in_seconds"},
		{"expired already", permitOverride(overrideException, "READER", "write-a", -1), `"r1"`, 400, "INVALID_EXPIRY", "expires_in_seconds"},
		{"expiry above a day", permitOverride(overrideException, "READER", "write-a", 86401), `"r1"`, 400, "INVALID_EXPIRY", "expires_in_seconds"},
		{"stale precondition", permitOverride(overrideException, "READER", "write-a", 60), `"r0"`, 412, "PRECONDITION_FAILED", ""},
	}
	for i, r := range refusals {
		code, outcome, body := e.createOverride(harness, r.request, r.ifMatch, fmt.Sprintf("refusal-%02d-xxxxxxxx", i))
		if code != r.status || outcome.Outcome != "rejected" || outcome.Reason != r.reason || outcome.Field != r.field {
			t.Errorf("%s: %d %s, want %d %s %s", r.name, code, body, r.status, r.reason, r.field)
		}
	}
	// Malformed requests, including fields that would change the key, window
	// or algorithm of a rate rule or a destination: refused before the
	// controller, nothing recorded.
	malformed := []string{
		`{"type":"exception","control":"rate","operation":"read-a","count":9,"window_seconds":1,"expires_in_seconds":60}`,
		`{"type":"exception","control":"rate","operation":"read-a","count":9,"key":"client_ip","expires_in_seconds":60}`,
		`{"type":"exception","control":"rate","operation":"read-a","count":9,"algorithm":"none","expires_in_seconds":60}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","upstream":"10.0.0.1:80","expires_in_seconds":60}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":60,"priority":1}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","role":"WRITER","expires_in_seconds":60}`,
		`{"Type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":60}`,
		`{"type":"exception","control":"rate","operation":"read-a","count":9.5,"expires_in_seconds":60}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":"60"}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":1e30}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":60}}`,
		`[]`,
	}
	for i, body := range malformed {
		if code, _, data := e.createOverride(harness, body, `"r1"`, fmt.Sprintf("malformed-%02d-xxxxxxx", i)); code != http.StatusBadRequest || !strings.Contains(data, "bad_request") {
			t.Errorf("malformed %s: %d %s", body, code, data)
		}
	}
	good := string(mustJSON(permitOverride(overrideException, "READER", "write-a", 60)))
	if code, _, _ := e.as(harness, http.MethodPost, "/admin/overrides", good, "Idempotency-Key", "no-if-match-0001"); code != http.StatusPreconditionRequired {
		t.Errorf("without If-Match: %d", code)
	}
	if code, _, _ := e.as(harness, http.MethodPost, "/admin/overrides", good, "If-Match", `"r1"`); code != http.StatusBadRequest {
		t.Errorf("without Idempotency-Key: %d", code)
	}
	if code, _, _ := e.as(harness, http.MethodPut, "/admin/overrides/1", good); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT of an override: %d", code)
	}
	if code, _, _ := e.as(harness, http.MethodPatch, "/admin/overrides/1", good); code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH of an override: %d", code)
	}

	// Callers without the right credential or permission.
	type attempt struct {
		name   string
		header []string
		status int
	}
	attempts := []attempt{
		{"no session", []string{"Origin", e.adminOrigin()}, http.StatusUnauthorized},
		{"API client token", []string{"Origin", e.adminOrigin(), "Authorization", "Bearer " + apiToken}, http.StatusUnauthorized},
		{"forged session cookie", []string{"Cookie", sessionCookie + "=" + strings.Repeat("A", 43), "Origin", e.adminOrigin()}, http.StatusUnauthorized},
		{"read-only administrator", reader.headers(e), http.StatusForbidden},
		{"no CSRF token", []string{"Cookie", sessionCookie + "=" + harness.cookie, "Origin", e.adminOrigin()}, http.StatusForbidden},
		{"another origin", []string{"Cookie", sessionCookie + "=" + harness.cookie, csrfHeader, harness.csrf, "Origin", "http://evil.example"}, http.StatusForbidden},
		{"another host", append(harness.headers(e), "Host", "localhost"), http.StatusBadRequest},
	}
	for i, a := range attempts {
		for _, call := range []struct{ method, path, body string }{
			{http.MethodPost, "/admin/overrides", good},
			{http.MethodPost, "/admin/overrides/validate", good},
			{http.MethodDelete, "/admin/overrides/1", ""},
		} {
			header := append(slices.Clone(a.header), "If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("unauthorised-%02d-xxxx", i))
			if code, body, _ := e.adminSend(call.method, call.path, call.body, header...); code != a.status {
				t.Errorf("%s %s, %s: %d %s, want %d", call.method, call.path, a.name, code, body, a.status)
			}
		}
	}
	if code, _, _ := e.adminSend(http.MethodGet, "/admin/overrides", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous overrides view: %d", code)
	}
	if code, _, _ := e.as(reader, http.MethodGet, "/admin/overrides", ""); code != http.StatusOK {
		t.Errorf("reader overrides view: %d", code)
	}
	// The native Caddy administration endpoint does not exist.
	if dialable("127.0.0.1:2019") {
		t.Error("something listens on the native Caddy administration port")
	}

	// Validation answers without creating anything.
	code, body, _ := e.as(harness, http.MethodPost, "/admin/overrides/validate", good, "If-Match", `"r1"`)
	if valid := fromJSON[Outcome](t, body); code != http.StatusOK || valid.Outcome != "valid" || valid.Target != "permit:READER:write-a" || valid.Expires.IsZero() {
		t.Fatalf("validation: %d %s", code, body)
	}
	bad := string(mustJSON(permitOverride(overrideException, "ADMIN", "write-a", 60)))
	if code, body, _ := e.as(harness, http.MethodPost, "/admin/overrides/validate", bad, "If-Match", `"r1"`); code != http.StatusBadRequest ||
		!strings.Contains(body, "UNKNOWN_REFERENCE") {
		t.Fatalf("validation of an unknown role: %d %s", code, body)
	}

	// The longest lifetime is accepted. The same key with another body is
	// refused; the first is kept.
	before24h := time.Now()
	first := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", maxOverrideSeconds), `"r1"`, "reused-key-000001")
	if first.Expires.Before(before24h.Add(24*time.Hour).Add(-time.Millisecond)) || first.Expires.After(time.Now().Add(24*time.Hour)) {
		t.Fatalf("expiry %v is not a day after the creation", first.Expires)
	}
	if code, outcome, body := e.createOverride(harness, permitOverride(overrideException, "READER", "write-a", 120), `"r1"`, "reused-key-000001"); code != http.StatusUnprocessableEntity ||
		outcome.Reason != "TRANSITION_ID_REUSED" {
		t.Fatalf("reused key: %d %s", code, body)
	}
	if code, _, body := e.revokeOverride(harness, first.Override, "revoke-refusals-01"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}

	// Nothing else changed; only the one created override was ever stored.
	after := e.status()
	if after.Revision != before.Revision || after.Confirmed != 1 || !strings.HasPrefix(after.Generation, "r1-") || after.Generation != before.Generation {
		t.Fatalf("status after the refusals %+v, before %+v", after, before)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden || e.probe("bob", http.MethodGet, "/b/x") != http.StatusOK {
		t.Fatal("enforcement changed")
	}
	e.assertReceipts(0, 1, 0)
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.rejected") >= len(refusals)+1 })
	if n := countKind(events, "tpo.rejected"); n < len(refusals)+1 {
		t.Fatalf("%d refusal events", n)
	}
	if countKind(events, "tpo.validated") != 1 || countKind(events, "tpo.created") != 1 {
		t.Fatal("validation or creation events missing")
	}
	assertAbsent(t, e, apiToken, harness.cookie, harness.csrf, reader.cookie, `"forged"`)
	e.stop()
	if list := storedOverrides(t, e.database); len(list) != 1 {
		t.Fatalf("stored overrides %+v", list)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'tpo.create' AND outcome = 'rejected'`); n != len(refusals) {
		t.Fatalf("%d recorded refusals, want %d", n, len(refusals))
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id LIKE '%unauthorised%' OR id LIKE '%malformed%'`); n != 0 {
		t.Fatalf("%d unauthorised or malformed requests reached the history", n)
	}
}
