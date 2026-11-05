// Authentication helpers, authentication and forged roles.

package gateway

import (
	"encoding/base64"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// These tests run bearer authentication and role authorisation through the
// real gateway process. Every refusal is checked at the fixtures or receivers
// themselves: a 401 or 403 alone does not show that nothing was forwarded.

// protectedPolicy is the standard test policy with authentication required on
// /a/*, /a/item and /b/*: READER may read /a/* and /b/*, WRITER may read and
// write /a/*. GET /d stays a denied public operation.
func (e *env) protectedPolicy() policy.Policy {
	p := e.policy(e.a, true)
	for i := range p.Operations {
		if p.Operations[i].ID != "denied" {
			p.Operations[i].Auth = "required"
		}
	}
	p.Roles = []policy.Role{{ID: "READER"}, {ID: "WRITER"}}
	p.Permits = []policy.Permit{
		{Role: "READER", Operation: "read-a"},
		{Role: "WRITER", Operation: "read-a"},
		{Role: "WRITER", Operation: "write-a"},
		{Role: "READER", Operation: "read-b"},
	}
	return p
}

func withoutRole(p policy.Policy, role string) policy.Policy {
	p.Roles = slices.DeleteFunc(p.Roles, func(r policy.Role) bool { return r.ID == role })
	p.Permits = slices.DeleteFunc(p.Permits, func(permit policy.Permit) bool { return permit.Role == role })
	return p
}

// requestEvents waits for and returns the events of one request.
func (e *env) requestEvents(requestID string, want int) []map[string]any {
	e.t.Helper()
	events := e.eventsWhen(func(events []map[string]any) bool {
		n := 0
		for _, ev := range events {
			if ev["request_id"] == requestID {
				n++
			}
		}
		return n >= want
	})
	var mine []map[string]any
	for _, ev := range events {
		if ev["request_id"] == requestID {
			mine = append(mine, ev)
		}
	}
	return mine
}

func stagesOf(ev map[string]any) map[string]any {
	stages, _ := ev["stages"].(map[string]any)
	return stages
}

func sum(counts map[string]int) int {
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}

// Every credential problem is refused at the authentication stage with 401,
// before authorisation, and nothing reaches any fixture. A token is accepted
// only with a genuine RS256 signature from the trusted key and the profile's
// claims.
func TestAuthenticationThroughGateway(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	states := map[string]string{}
	for name, f := range fixtures {
		states[name] = f.state().SHA256
	}

	// A listener that a token header could name as a key location. The
	// gateway must never contact it.
	keyServer := newReceiver(t, "keys")

	now := time.Now().Unix()
	claims := func(changes map[string]any) string {
		return issuer.token(t, changed(validClaims("staff-01"), changes))
	}
	valid := issuer.tokenFor(t, "staff-01")
	segments := strings.Split(valid, ".")
	signature, _ := base64.RawURLEncoding.DecodeString(segments[2])
	signature[0] ^= 0x80
	tampered := segments[0] + "." + segments[1] + "." + base64.RawURLEncoding.EncodeToString(signature)
	elevated := segments[0] + "." + encodeSegment(claimsJSON(validClaims("mgr-01"))) + "." + segments[2]
	payload := claimsJSON(validClaims("staff-01"))

	type vector struct {
		name    string
		header  []string // name/value pairs
		query   string
		status  int
		reason  string
		subject string // expected subject when accepted
	}
	vectors := []vector{
		{"valid token", []string{"Authorization", "Bearer " + valid}, "", 200, "", "staff-01"},
		{"missing required issuer and audience", []string{"Authorization", "Bearer " + claims(map[string]any{"iss": nil, "aud": nil})}, "", 401, "invalid_claims", ""},
		{"aud array naming this gateway", []string{"Authorization", "Bearer " + claims(map[string]any{"aud": []string{"other", testAudience}})}, "", 200, "", "staff-01"},

		{"no credential", nil, "", 401, "missing_credential", ""},
		{"token in the query string", nil, "?access_token=" + valid, 401, "missing_credential", ""},
		{"token in a cookie", []string{"Cookie", "access_token=" + valid}, "", 401, "missing_credential", ""},
		{"token in a custom header", []string{"X-Access-Token", valid}, "", 401, "missing_credential", ""},
		{"forged internal identity headers", []string{"X-Gateway-Subject", "staff-01", "X-Gateway-Roles", "STAFF", "X-Gateway-Authenticated", "true"}, "", 401, "missing_credential", ""},
		{"empty bearer", []string{"Authorization", "Bearer"}, "", 401, "malformed_credential", ""},
		{"basic credentials", []string{"Authorization", "Basic c3RhZmYtMDE6cGFzc3dvcmQ="}, "", 401, "malformed_credential", ""},
		{"truncated token", []string{"Authorization", "Bearer " + segments[0] + "." + segments[1]}, "", 401, "malformed_credential", ""},
		{"oversize token", []string{"Authorization", "Bearer " + valid + strings.Repeat("A", maxTokenBytes)}, "", 401, "malformed_credential", ""},
		{"alg none without signature", []string{"Authorization", "Bearer " + encodeSegment(`{"alg":"none"}`) + "." + encodeSegment(payload) + "."}, "", 401, "malformed_credential", ""},
		{"alg none with a fake signature", []string{"Authorization", "Bearer " + encodeSegment(`{"alg":"none"}`) + "." + encodeSegment(payload) + ".e30"}, "", 401, "unsupported_header", ""},
		{"HS256 keyed with the public key", []string{"Authorization", "Bearer " + hmacToken(issuer.publicPEM(), payload)}, "", 401, "unsupported_header", ""},
		{"key URL in jku", []string{"Authorization", "Bearer " + issuer.signRaw(`{"alg":"RS256","kid":"test-a","jku":"http://`+keyServer.addr+`/jwks"}`, payload)}, "", 401, "unsupported_header", ""},
		{"certificate URL in x5u", []string{"Authorization", "Bearer " + issuer.signRaw(`{"alg":"RS256","kid":"test-a","x5u":"http://`+keyServer.addr+`/cert"}`, payload)}, "", 401, "unsupported_header", ""},
		{"signature bit flipped", []string{"Authorization", "Bearer " + tampered}, "", 401, "invalid_signature", ""},
		{"payload swapped for another subject", []string{"Authorization", "Bearer " + elevated}, "", 401, "invalid_signature", ""},
		{"untrusted RSA key", []string{"Authorization", "Bearer " + newTestIssuer(2048).tokenFor(t, "staff-01")}, "", 401, "invalid_signature", ""},
		{"invalid token with forged internal headers", []string{"Authorization", "Bearer " + tampered, "X-Gateway-Subject", "staff-01", "X-Gateway-Generation", "forged"}, "", 401, "invalid_signature", ""},
		{"expired", []string{"Authorization", "Bearer " + claims(map[string]any{"exp": now - 60})}, "", 401, "expired", ""},
		{"not yet valid", []string{"Authorization", "Bearer " + claims(map[string]any{"nbf": now + 600})}, "", 401, "not_yet_valid", ""},
		{"wrong issuer", []string{"Authorization", "Bearer " + claims(map[string]any{"iss": "https://other.test"})}, "", 401, "wrong_issuer", ""},
		{"token for another recipient", []string{"Authorization", "Bearer " + claims(map[string]any{"aud": "other-gateway"})}, "", 401, "wrong_audience", ""},
		{"no sub", []string{"Authorization", "Bearer " + claims(map[string]any{"sub": nil})}, "", 401, "invalid_claims", ""},
		{"no exp", []string{"Authorization", "Bearer " + claims(map[string]any{"exp": nil})}, "", 401, "invalid_claims", ""},
		{"exp as a string", []string{"Authorization", "Bearer " + claims(map[string]any{"exp": "9999999999"})}, "", 401, "invalid_claims", ""},
	}

	for _, v := range vectors {
		before := receiptCounts(fixtures)
		resp, body := e.exchange(http.MethodGet, "/orders"+v.query, "", "", v.header...)
		after := receiptCounts(fixtures)
		_, id := requestID(resp, body)

		if resp.StatusCode != v.status {
			t.Errorf("%s: status %d, want %d (%s)", v.name, resp.StatusCode, v.status, body)
			continue
		}
		if v.status == http.StatusOK {
			receipts := fixtures["orders"].receipts()
			if after["orders"] != before["orders"]+1 || receipts[len(receipts)-1].Subject != v.subject {
				t.Errorf("%s: receipts %v -> %v, want one at orders for %s", v.name, before, after, v.subject)
			}
			continue
		}

		// Refused: nothing reached any fixture, and the event shows exactly
		// where and why, with no identity.
		if !maps.Equal(before, after) {
			t.Errorf("%s: receipts changed %v -> %v", v.name, before, after)
		}
		if resp.Header.Get("WWW-Authenticate") != "Bearer" || !strings.Contains(body, `"unauthenticated"`) {
			t.Errorf("%s: response %v %s", v.name, resp.Header, body)
		}
		events := e.requestEvents(id, 1)
		if len(events) != 1 {
			t.Errorf("%s: events %v", v.name, events)
			continue
		}
		ev, stages := events[0], stagesOf(events[0])
		_, hasSubject := ev["subject"]
		if ev["stage"] != "JWT" || ev["reason"] != v.reason || ev["operation"] != "orders.list" || hasSubject ||
			stages["JWT"] != stageReject || stages["RBAC"] != stageNotRun || stages["BODY"] != stageNotRun || stages["AUDIT"] != stageNotRun {
			t.Errorf("%s: event %v, want JWT rejection %s", v.name, ev, v.reason)
		}
	}

	// A repeated Authorization field is ambiguous even when both copies are
	// the same valid token.
	before := receiptCounts(fixtures)
	code, _ := e.raw("GET /orders HTTP/1.1\r\nHost: gw\r\nAuthorization: Bearer " + valid + "\r\nAuthorization: Bearer " + valid + "\r\nConnection: close\r\n\r\n")
	if code != http.StatusUnauthorized || !maps.Equal(before, receiptCounts(fixtures)) {
		t.Errorf("repeated Authorization field: %d", code)
	}

	if keyServer.count() != 0 {
		t.Fatalf("the gateway contacted a key location named by a token: %d requests", keyServer.count())
	}
	for name, f := range fixtures {
		if f.state().SHA256 != states[name] {
			t.Errorf("fixture %s state changed", name)
		}
	}
}

// Role claims from an untrusted issuer, and role or identity values anywhere
// else in the request, never add a role: only verified token roles that the
// policy defines count.
func TestForgedRolesGrantNothing(t *testing.T) {
	e, fixtures := newFixtureEnv(t)

	forgedClaims := validClaims("cust-001")
	forgedClaims["roles"] = []string{"MANAGER", "ADMIN"}
	forgedClaims["role"] = "ADMIN"
	forgedClaims["groups"] = []string{"MANAGER"}
	forgedClaims["scope"] = "reports.sales admin"
	forgedToken := newTestIssuer(2048).token(t, forgedClaims)

	attempts := []struct {
		name   string
		method string
		path   string
		body   string
		header []string
		status int
	}{
		{"untrusted token claiming MANAGER and ADMIN", http.MethodGet, "/reports/sales", "", []string{"Authorization", "Bearer " + forgedToken}, 401},
		{"untrusted zero-role subject claiming MANAGER", http.MethodGet, "/orders", "", []string{"Authorization", "Bearer " + newTestIssuer(2048).token(t, changed(validClaims("nobody-01"), map[string]any{"roles": []string{"STAFF", "MANAGER"}}))}, 401},
		{"forged identity headers beside a valid token", http.MethodGet, "/reports/sales", "", append(bearer(t, "cust-001"),
			"X-Gateway-Subject", "mgr-01", "X-Gateway-Roles", "MANAGER", "X-Roles", "MANAGER", "X-User", "mgr-01"), 403},
		{"role in the query string", http.MethodGet, "/reports/sales?role=MANAGER&roles=ADMIN", "", bearer(t, "cust-001"), 403},
		{"role in the body", http.MethodPost, "/orders", `{"customer":"cust-001","item":"ITM-001","quantity":1,"role":"CUSTOMER"}`, bearer(t, "staff-01"), 403},
		{"a role name as the subject", http.MethodGet, "/reports/sales", "", bearer(t, "MANAGER"), 403},
		{"a subject in another case", http.MethodGet, "/reports/sales", "", bearer(t, "MGR-01"), 403},
		{"the manager's subject from an untrusted key", http.MethodGet, "/reports/sales", "", []string{"Authorization", "Bearer " + newTestIssuer(2048).tokenFor(t, "mgr-01")}, 401},
		{"no administrator bypass", http.MethodGet, "/reports/sales", "", bearer(t, "admin"), 403},
	}
	for _, a := range attempts {
		contentType := ""
		if a.body != "" {
			contentType = "application/json"
		}
		before := receiptCounts(fixtures)
		code, _ := e.request(a.method, a.path, contentType, a.body, a.header...)
		if code != a.status || !maps.Equal(before, receiptCounts(fixtures)) {
			t.Errorf("%s: status %d, receipts %v -> %v; want %d and none", a.name, code, before, receiptCounts(fixtures), a.status)
		}
	}

	// Positive control: the same request succeeds for the subject that holds
	// the role in the policy.
	if code, _ := e.request(http.MethodGet, "/reports/sales", "", "", bearer(t, "mgr-01")...); code != http.StatusOK {
		t.Fatalf("manager control: %d", code)
	}
	if n := len(fixtures["reports"].receipts()); n != 1 {
		t.Fatalf("reports receipts %d, want only the control", n)
	}
}
