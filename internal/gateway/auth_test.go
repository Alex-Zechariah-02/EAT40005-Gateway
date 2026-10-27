package gateway

import (
	"bufio"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fixture "github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/services/testfixture"
)

// These tests run bearer authentication and role authorisation through the
// real gateway process. Every refusal is checked at the fixtures or receivers
// themselves: a 401 or 403 alone does not show that nothing was forwarded.

// protectedPolicy is the standard test policy with authentication required on
// /a/*, /a/item and /b/*: READER may read /a/* and /b/*, WRITER may read and
// write /a/*. GET /d stays a denied public operation.
func (e *env) protectedPolicy() Policy {
	p := e.policy(e.a, true)
	for i := range p.Operations {
		if p.Operations[i].ID != "denied" {
			p.Operations[i].Auth = "required"
		}
	}
	p.Roles = []Role{{ID: "READER"}, {ID: "WRITER"}}
	p.Permits = []Permit{
		{Role: "READER", Operation: "read-a"},
		{Role: "WRITER", Operation: "read-a"},
		{Role: "WRITER", Operation: "write-a"},
		{Role: "READER", Operation: "read-b"},
	}
	return p
}

func withoutRole(p Policy, role string) Policy {
	p.Roles = slices.DeleteFunc(p.Roles, func(r Role) bool { return r.ID == role })
	p.Permits = slices.DeleteFunc(p.Permits, func(permit Permit) bool { return permit.Role == role })
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

// Every catalogue subject, and one the policy does not know, calls every
// catalogue operation with a genuine token. The expected decision comes from
// the catalogue's role truth alone: an operation is reached exactly when it
// is public, or the subject is active and holds a role the operation is
// intended for. Each decision is checked at the fixtures.
func TestRoleMatrixAgainstCatalogue(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	c := loadCatalogue(t)
	roles := catalogueRoles(c)
	generation := e.status().Generation

	requests := map[string]struct{ method, path, body string }{
		"orders.list":      {http.MethodGet, "/orders", ""},
		"orders.get":       {http.MethodGet, "/orders/ORD-1001", ""},
		"orders.create":    {http.MethodPost, "/orders", `{"customer":"%s","item":"ITM-001","quantity":1}`},
		"inventory.health": {http.MethodGet, "/inventory/health", ""},
		"inventory.get":    {http.MethodGet, "/inventory/items/ITM-001", ""},
		"inventory.adjust": {http.MethodPatch, "/inventory/items/ITM-001", `{"delta":1}`},
		"reports.sales":    {http.MethodGet, "/reports/sales", ""},
		"reports.summary":  {http.MethodGet, "/reports/summary", ""},
	}
	// ghost-01 holds a valid token but has no membership record.
	subjects := append(slices.Clone(c.Subjects), fixture.Subject{ID: "ghost-01"})

	allowed, denied := 0, 0
	for _, s := range subjects {
		for _, op := range c.Operations {
			intended := op.Auth == "none" || (s.Status == "active" && slices.ContainsFunc(s.Roles, func(r string) bool { return slices.Contains(op.Roles, r) }))
			wantReason := ""
			switch {
			case intended:
			case s.Status == "disabled":
				wantReason = "account_blocked"
			default:
				wantReason = "no_permit"
			}

			r := requests[op.ID]
			body, contentType := "", ""
			if r.body != "" {
				body, contentType = strings.ReplaceAll(r.body, "%s", s.ID), "application/json"
			}
			target := fixtures[op.API]
			before := receiptCounts(fixtures)
			stateBefore := target.state().SHA256
			resp, reply := e.exchange(r.method, r.path, contentType, body, bearer(t, s.ID)...)
			after := receiptCounts(fixtures)
			name := s.ID + " " + op.ID

			if intended {
				allowed++
				receipts := target.receipts()
				last := receipts[len(receipts)-1]
				wantSubject, wantRoles := s.ID, roles[s.ID]
				if op.Auth == "none" {
					wantSubject, wantRoles = "", ""
				}
				if after[op.API] != before[op.API]+1 || sum(after) != sum(before)+1 {
					t.Errorf("%s: receipts %v -> %v, want exactly one at %s", name, before, after, op.API)
				} else if last.Operation != op.ID || last.Subject != wantSubject || last.Roles != wantRoles || last.Generation != generation {
					t.Errorf("%s: receipt %+v", name, last)
				}
				continue
			}

			denied++
			wantCode := "forbidden"
			if s.Status == "disabled" {
				wantCode = "account_blocked"
			}
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(reply, `"`+wantCode+`"`) {
				t.Errorf("%s: status %d %s, want 403", name, resp.StatusCode, reply)
			}
			if !maps.Equal(before, after) || target.state().SHA256 != stateBefore {
				t.Errorf("%s: refused request reached a fixture or changed state (%v -> %v)", name, before, after)
			}
			_, id := requestID(resp, reply)
			events := e.requestEvents(id, 1)
			if len(events) != 1 {
				t.Errorf("%s: events %v", name, events)
				continue
			}
			ev, stages := events[0], stagesOf(events[0])
			if ev["stage"] != "RBAC" || ev["reason"] != wantReason || ev["subject"] != s.ID || ev["generation"] != generation ||
				stages["JWT"] != stagePass || stages["RBAC"] != stageReject || stages["BODY"] != stageNotRun {
				t.Errorf("%s: event %v, want RBAC rejection %s", name, ev, wantReason)
			}
		}
	}
	t.Logf("%d subjects x %d operations: %d reached their fixture, %d refused with no receipt", len(subjects), len(c.Operations), allowed, denied)
	if allowed == 0 || denied == 0 {
		t.Fatal("the matrix does not distinguish allowed from refused calls")
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

// Route RBAC allows a customer to call orders.get, but which orders the
// customer may see is the orders API's own rule. The gateway forwards the
// call and records RBAC as passed; the fixture refuses another customer's
// order and records that it did.
func TestObjectOwnershipStaysWithFixture(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	orders := fixtures["orders"]

	cases := []struct {
		subject string
		status  int
	}{
		{"cust-001", http.StatusNotFound}, // ORD-1002 belongs to cust-002
		{"cust-002", http.StatusOK},
		{"staff-01", http.StatusOK},
	}
	for i, tc := range cases {
		resp, body := e.exchange(http.MethodGet, "/orders/ORD-1002", "", "", bearer(t, tc.subject)...)
		receipts := orders.receipts()
		if resp.StatusCode != tc.status || len(receipts) != i+1 {
			t.Fatalf("%s: status %d (%s) with %d receipts", tc.subject, resp.StatusCode, body, len(receipts))
		}
		last := receipts[i]
		if last.Operation != "orders.get" || last.Status != tc.status || last.Subject != tc.subject {
			t.Fatalf("%s: fixture receipt %+v", tc.subject, last)
		}
		events := e.requestEvents(last.RequestID, 2)
		forwarded := findEvent(events, "request.forwarded", last.RequestID)
		if forwarded == nil || stagesOf(forwarded)["RBAC"] != stagePass || forwarded["status"] != float64(tc.status) {
			t.Fatalf("%s: gateway event %v", tc.subject, forwarded)
		}
	}
}

// Authentication and authorisation events record what actually ran, with the
// verified subject only after verification, and never a credential.
func TestAuthenticationEvents(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	g := e.status().Generation

	routed := []string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS"}
	type expected struct {
		kind, stage, reason, subject, errorCode string
		status                                  int
		stages                                  map[string]any
	}
	aliceToken := issuer.tokenFor(t, "alice")
	cases := []struct {
		name     string
		method   string
		path     string
		body     string
		token    string
		receipts int
		want     []expected
	}{
		{"no token", http.MethodGet, "/a/x", "", "", 0, []expected{
			{"request.rejected", "JWT", "missing_credential", "", "unauthenticated", 401, expectedStages(routed, "JWT", stageReject)},
		}},
		{"expired token", http.MethodGet, "/a/x", "", issuer.token(t, changed(validClaims("alice"), map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})), 0, []expected{
			{"request.rejected", "JWT", "expired", "", "unauthenticated", 401, expectedStages(routed, "JWT", stageReject)},
		}},
		{"unknown subject", http.MethodGet, "/a/x", "", issuer.tokenFor(t, "mallory"), 0, []expected{
			{"request.rejected", "RBAC", "no_permit", "mallory", "forbidden", 403, expectedStages(append(routed, "JWT"), "RBAC", stageReject)},
		}},
		{"no permit", http.MethodPost, "/a/item", `{"n":1}`, issuer.tokenFor(t, "bob"), 0, []expected{
			{"request.rejected", "RBAC", "no_permit", "bob", "forbidden", 403, expectedStages(append(routed, "JWT"), "RBAC", stageReject)},
		}},
		{"permitted", http.MethodPost, "/a/item", `{"n":1}`, aliceToken, 1, []expected{
			{"request.admitted", "AUDIT", "", "alice", "", 0, unrated(expectedStages(append(routed, "JWT", "RBAC", "BODY", "VALIDATION"), "", ""))},
			{"request.forwarded", "FORWARD", "", "alice", "", 200, unrated(expectedStages(append(routed, "JWT", "RBAC", "BODY", "VALIDATION", "AUDIT", "BIND"), "FORWARD", stagePass))},
		}},
		{"denied public operation", http.MethodGet, "/d", "", aliceToken, 0, []expected{
			{"request.rejected", "ACCESS", "", "", "forbidden", 403, expectedStages(routed[:4], "ACCESS", stageReject)},
		}},
	}
	for _, tc := range cases {
		e.resetReceivers()
		var header []string
		if tc.token != "" {
			header = []string{"Authorization", "Bearer " + tc.token}
		}
		resp, body := e.exchange(tc.method, tc.path, "application/json", tc.body, header...)
		id := ""
		if tc.receipts > 0 {
			id = e.a.receipts()[0].RequestID
		} else {
			_, id = requestID(resp, body)
		}
		e.assertReceipts(tc.receipts, 0, 0)

		events := e.requestEvents(id, len(tc.want))
		if len(events) != len(tc.want) {
			t.Fatalf("%s: %d events, want %d: %v", tc.name, len(events), len(tc.want), events)
		}
		for i, want := range tc.want {
			ev := events[i]
			reason, _ := ev["reason"].(string)
			subject, _ := ev["subject"].(string)
			errorCode, _ := ev["error"].(string)
			status, _ := ev["status"].(float64)
			if ev["kind"] != want.kind || ev["stage"] != want.stage || reason != want.reason || subject != want.subject ||
				errorCode != want.errorCode || int(status) != want.status || ev["generation"] != g || ev["schema"] != float64(eventSchema) {
				t.Errorf("%s: event %v, want %+v", tc.name, ev, want)
			}
			if !sameStages(ev["stages"], want.stages) {
				t.Errorf("%s: stages %v, want %v", tc.name, ev["stages"], want.stages)
			}
		}
	}

	// The fixture-facing identity is the verified one.
	e.resetReceivers()
	e.request(http.MethodGet, "/a/x", "", "", "Authorization", "Bearer "+aliceToken, "X-Gateway-Subject", "bob", "X-Gateway-Roles", "ADMIN")
	h := e.a.receipts()[0].Headers
	if h.Get("X-Gateway-Subject") != "alice" || h.Get("X-Gateway-Roles") != "WRITER" || h.Get("Authorization") != "" {
		t.Fatalf("forwarded identity headers %v", h)
	}
}

// No credential, key or token reaches the audit log, the gateway's own
// output or a client error, whatever the request carries.
func TestCredentialsNeverRecorded(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)

	valid := issuer.tokenFor(t, "alice")
	forged := newTestIssuer(2048).tokenFor(t, "alice")
	multiline := issuer.signRaw(`{"alg":"RS256","kid":"test-a"}`, `{"iss":"https://issuer.test","aud":"gateway.test","sub":"alice\n{\"kind\":\"forged\"}","exp":9999999999}`)
	privatePEM := "-----BEGIN PRIVATE KEY-----MIIEvQIBADANBgkqhkiG9w0BAQEFAASC-----END PRIVATE KEY-----"
	requests := [][]string{
		{"Authorization", "Bearer " + valid},
		{"Authorization", "Bearer " + forged},
		{"Authorization", "Bearer " + multiline},
		{"Authorization", "Basic " + privatePEM},
		{"Authorization", privatePEM},
	}
	var replies []string
	for _, header := range requests {
		_, body := e.request(http.MethodGet, "/a/x?token="+valid[:40], "", "", header...)
		replies = append(replies, body)
	}
	e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.rejected") >= 4 })
	e.assertReceipts(1, 0, 0)

	audit, err := os.ReadFile(e.auditLog)
	if err != nil {
		t.Fatal(err)
	}
	e.stop()
	output := e.output.String()
	signatures := []string{}
	for _, token := range []string{valid, forged, multiline} {
		signatures = append(signatures, token[strings.LastIndex(token, ".")+1:])
	}
	secrets := append([]string{"eyJ", "Bearer", "Basic", "PRIVATE KEY", "MIIEvQ", valid[:40]}, signatures...)
	for _, secret := range secrets {
		if strings.Contains(string(audit), secret) {
			t.Errorf("audit log contains %q", secret)
		}
		if strings.Contains(output, secret) {
			t.Errorf("gateway output contains %q", secret)
		}
		for _, reply := range replies {
			if strings.Contains(reply, secret) {
				t.Errorf("client reply %q contains %q", reply, secret)
			}
		}
	}
	for i, line := range strings.Split(strings.TrimSuffix(string(audit), "\n"), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev["kind"] == "forged" {
			t.Fatalf("audit line %d is not one genuine record: %q", i, line)
		}
	}
}

// A role removal published while a request is in flight does not change that
// request: it completes under the generation it captured, with that
// generation's roles. Every later request uses the new generation. The held
// request is paused right after capture, before authentication and
// authorisation run, so both run after the new generation is published.
func TestRoleChangeAppliesToNewRequestsOnly(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	holdFile, release := filepath.Join(e.dir, "hold"), filepath.Join(e.dir, "release")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/item", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	first := e.status().Generation
	aliceToken, bobToken := issuer.tokenFor(t, "alice"), issuer.tokenFor(t, "bob")

	conn, captured := holdRequest(t, e, holdFile, aliceToken)
	defer conn.Close()
	if captured != first {
		t.Fatalf("captured %s, want %s", captured, first)
	}

	// The next generation removes alice's role and grants READER the write.
	next := withoutRole(e.protectedPolicy(), "WRITER")
	next.Permits = append(next.Permits, Permit{Role: "READER", Operation: "write-a"})
	outcome, code := e.publish(Change{ID: "remove-writer", Expected: 1, Policy: mustJSON(next)})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "true}")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("in-flight request: %v %v", resp, err)
	}
	e.assertReceipts(1, 0, 0)
	held := e.a.receipts()[0]
	if held.Generation != first || held.Headers.Get("X-Gateway-Roles") != "WRITER" {
		t.Fatalf("in-flight request reached A under %s with roles %q, want %s and WRITER", held.Generation, held.Headers.Get("X-Gateway-Roles"), first)
	}

	// New requests: alice is refused without reaching A; bob now may write.
	resp, body := e.exchange(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("alice after removal: %d %s", resp.StatusCode, body)
	}
	e.assertReceipts(1, 0, 0)
	_, refusedID := requestID(resp, body)
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":2}`, "Authorization", "Bearer "+bobToken); code != http.StatusOK {
		t.Fatalf("bob after the new permit: %d", code)
	}
	e.assertReceipts(2, 0, 0)
	if g := e.a.receipts()[1].Generation; g != outcome.Generation {
		t.Fatalf("bob's write under %s, want %s", g, outcome.Generation)
	}

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == 2 })
	if ev := findEvent(events, "request.forwarded", held.RequestID); ev == nil || ev["generation"] != first || ev["subject"] != "alice" {
		t.Fatalf("in-flight event %v", ev)
	}
	if ev := findEvent(events, "request.rejected", refusedID); ev == nil || ev["generation"] != outcome.Generation || ev["reason"] != "no_permit" {
		t.Fatalf("refusal event %v", ev)
	}
}

// While membership changes repeatedly under concurrent load, every decision
// matches the generation it was made under: a request reaches the upstream
// only under a generation in which the caller holds a permitted role, and a
// refusal happens only under one in which it does not.
func TestRoleSwapsUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault)
	bobToken := issuer.tokenFor(t, "bob")

	// Whether bob may read /a/* under each generation.
	permitted := map[string]bool{e.status().Generation: true}

	const workers = 8
	const swaps = 100
	requests := make(chan struct{}, workers)
	completed := make(chan struct{}, workers)
	var unexpected atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			for range requests {
				req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", nil)
				req.Header.Set("Authorization", "Bearer "+bobToken)
				resp, err := client.Do(req)
				if err != nil {
					e.t.Logf("role-swap request failed: %v", err)
					unexpected.Add(1)
					completed <- struct{}{}
					continue
				}
				_, readErr := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if readErr != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusForbidden) {
					e.t.Logf("role-swap response: status=%d read=%v", resp.StatusCode, readErr)
					unexpected.Add(1)
				}
				completed <- struct{}{}
			}
		})
	}
	defer func() {
		close(requests)
		wg.Wait()
	}()

	for i := range swaps {
		bobRoles := []string{"READER"}
		if i%2 == 0 {
			bobRoles = nil
		}
		policy := e.protectedPolicy()
		if bobRoles == nil {
			policy = withoutRole(policy, "READER")
		}
		// Keep eight real requests racing every publication, with an exact
		// bound. An unbounded refusal loop exhausts Windows client ports
		// because the gateway deliberately closes refused connections.
		for range workers {
			requests <- struct{}{}
		}
		outcome, code := e.publish(Change{ID: fmt.Sprintf("swap-%d", i), Expected: int64(i + 1), Policy: mustJSON(policy)})
		if code != http.StatusOK {
			t.Fatalf("swap %d: %d %+v", i, code, outcome)
		}
		permitted[outcome.Generation] = bobRoles != nil
		for range workers {
			<-completed
		}
	}
	if unexpected.Load() != 0 {
		t.Fatalf("%d requests failed or got an unexpected status", unexpected.Load())
	}

	forwarded := 0
	for _, r := range e.a.receipts() {
		if !permitted[r.Generation] || r.Headers.Get("X-Gateway-Roles") != "READER" {
			t.Fatalf("receipt under %s with roles %q, where bob holds no permitted role", r.Generation, r.Headers.Get("X-Gateway-Roles"))
		}
		forwarded++
	}
	events := e.eventsWhen(func(events []map[string]any) bool {
		return countKind(events, "request.forwarded")+countKind(events, "request.rejected") >= workers*swaps
	})
	refused := 0
	for _, ev := range events {
		if ev["kind"] != "request.rejected" {
			continue
		}
		generation, _ := ev["generation"].(string)
		if ev["reason"] != "no_permit" || permitted[generation] {
			t.Fatalf("refusal %v under a generation in which bob is permitted", ev)
		}
		refused++
	}
	t.Logf("%d swaps: %d requests reached the upstream, %d refused, each under the right generation", swaps, forwarded, refused)
	if forwarded == 0 || refused == 0 {
		t.Fatal("the load did not exercise both outcomes")
	}
	if forwarded+refused != workers*swaps {
		t.Fatalf("%d receipts plus %d refusals, want %d decisions", forwarded, refused, workers*swaps)
	}
}

// A membership change that is invalid or based on a stale revision is
// refused as a whole; the previous membership keeps applying.
func TestRejectedRoleChangeKeepsMembership(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault)
	before := e.status()
	aliceToken := issuer.tokenFor(t, "alice")

	removal := withoutRole(e.protectedPolicy(), "WRITER")
	invalid := e.protectedPolicy()
	invalid.Permits = append(invalid.Permits, Permit{Role: "ADMIN", Operation: "write-a"})
	duplicate := e.protectedPolicy()
	duplicate.Roles = append(duplicate.Roles, Role{ID: "READER"})
	changes := []struct {
		change Change
		status int
		reason string
	}{
		{Change{ID: "unknown-role", Expected: 1, Policy: mustJSON(invalid)}, 400, "UNKNOWN_REFERENCE"},
		{Change{ID: "duplicate-subject", Expected: 1, Policy: mustJSON(duplicate)}, 400, "DUPLICATE_ID"},
		{Change{ID: "stale-removal", Expected: 0, Policy: mustJSON(removal)}, 409, "STALE"},
	}
	for _, c := range changes {
		outcome, code := e.publish(c.change)
		if code != c.status || outcome.Reason != c.reason {
			t.Errorf("%s: %d %+v, want %d %s", c.change.ID, code, outcome, c.status, c.reason)
		}
		if s := e.status(); s.Generation != before.Generation {
			t.Fatalf("%s changed the generation", c.change.ID)
		}
		if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken); code != http.StatusOK {
			t.Fatalf("alice after rejected %s: %d", c.change.ID, code)
		}
	}

	// The valid removal then applies.
	if _, code := e.publish(Change{ID: "disable-alice", Expected: 1, Policy: mustJSON(removal)}); code != http.StatusOK {
		t.Fatalf("valid removal: %d", code)
	}
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken); code != http.StatusForbidden {
		t.Fatalf("disabled alice: %d", code)
	}
	e.assertReceipts(len(changes), 0, 0)
}

// A crash during a membership publication leaves either the old or the new
// membership in force after restart, never a mixture, and the transition is
// applied exactly once.
func TestRoleChangeSurvivesCrash(t *testing.T) {
	for _, tc := range []struct {
		point     string
		committed bool
	}{
		{"before-commit", false},
		{"after-commit", true},
		{"after-swap", true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+tc.point)
			aliceToken, bobToken := issuer.tokenFor(t, "alice"), issuer.tokenFor(t, "bob")

			// Removes WRITER from policy and grants READER the write.
			next := withoutRole(e.protectedPolicy(), "WRITER")
			next.Permits = append(next.Permits, Permit{Role: "READER", Operation: "write-a"})
			change := Change{ID: "swap-roles", Expected: 1, Policy: mustJSON(next)}
			if _, code := e.publish(change); code != 0 {
				t.Fatalf("publication answered %d instead of crashing", code)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d, want %d\n%s", code, crashExit, e.output)
			}

			e.start(binaries.fault)
			wantAlice, wantBob, wantRevision := http.StatusOK, http.StatusForbidden, int64(1)
			if tc.committed {
				wantAlice, wantBob, wantRevision = http.StatusForbidden, http.StatusOK, 2
			}
			if s := e.status(); s.Revision != wantRevision {
				t.Fatalf("restart serves revision %d, want %d", s.Revision, wantRevision)
			}
			aliceCode, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken)
			bobCode, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":2}`, "Authorization", "Bearer "+bobToken)
			if aliceCode != wantAlice || bobCode != wantBob {
				t.Fatalf("after restart alice %d bob %d, want %d %d", aliceCode, bobCode, wantAlice, wantBob)
			}
			e.assertReceipts(1, 0, 0)

			// Retrying the same transition applies it exactly once.
			if outcome, code := e.publish(change); code != http.StatusOK || outcome.Revision != 2 {
				t.Fatalf("retry: %d %+v", code, outcome)
			}
			if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":3}`, "Authorization", "Bearer "+aliceToken); code != http.StatusForbidden {
				t.Fatalf("alice after retry: %d", code)
			}
		})
	}
}

// The gateway does not start without valid trust material, and changing a
// key file alone changes nothing: keys are replaced by a reload or restart.
func TestTrustMaterialAtStartup(t *testing.T) {
	other := newTestIssuer(2048)
	privateKey := filepath.Join(t.TempDir(), "private.pem")
	der, _ := x509.MarshalPKCS8PrivateKey(other.key)
	os.WriteFile(privateKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)

	trust := func(e *env, changes map[string]any) map[string]any {
		config := map[string]any{"issuer": testIssuerName, "audience": testAudience, "keys": []VerificationKey{{Kid: "test-a", PublicKeyFile: e.publicKey}}}
		return changed(config, changes)
	}
	// cause is the part of the startup error that shows why it was refused.
	// The gateway logs the error as JSON, so quotes inside it appear escaped.
	cases := []struct {
		name    string
		changes func(e *env) any
		cause   string
	}{
		{"no trust configuration", func(e *env) any { return map[string]any{} }, "gateway.jwt.issuer is required"},
		{"missing key file", func(e *env) any {
			return trust(e, map[string]any{"keys": []VerificationKey{{Kid: "test-a", PublicKeyFile: filepath.Join(e.dir, "absent.pem")}}})
		}, "jwt: opening public key"},
		{"private key given as the key", func(e *env) any {
			return trust(e, map[string]any{"keys": []VerificationKey{{Kid: "test-a", PublicKeyFile: privateKey}}})
		}, "exactly one PEM PUBLIC KEY block"},
		{"no issuer", func(e *env) any { return trust(e, map[string]any{"issuer": nil}) }, "gateway.jwt.issuer is required"},
		{"no audience", func(e *env) any { return trust(e, map[string]any{"audience": nil}) }, "gateway.jwt.audience is required"},
		{"several audiences", func(e *env) any { return trust(e, map[string]any{"audience": []string{testAudience, "other"}}) }, "gateway.jwt.audience: a JSON array is not a valid value"},
		{"remote key set", func(e *env) any { return trust(e, map[string]any{"jwks_url": "http://127.0.0.1:1/jwks"}) }, `unknown field \"jwks_url\"`},
		{"second issuer", func(e *env) any { return trust(e, map[string]any{"issuers": []string{"https://other.test"}}) }, `unknown field \"issuers\"`},
		{"clock leeway", func(e *env) any { return trust(e, map[string]any{"leeway": "30s"}) }, `unknown field \"leeway\"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.writeConfig(configOptions{jwt: tc.changes(e)})
			e.launch(binaries.plain, []string{"run", "--config", e.config})
			if code := e.waitExit(); code == 0 {
				t.Fatalf("gateway ran without valid trust material\n%s", e.output)
			}
			if !strings.Contains(e.output.String(), tc.cause) {
				t.Fatalf("refused for another reason, want %q:\n%s", tc.cause, e.output)
			}
			if dialable(e.dataAddr) || dialable(e.admin) {
				t.Fatal("a listener is open after the refused start")
			}
			if strings.Contains(e.output.String(), "PRIVATE KEY") || strings.Contains(e.output.String(), "MII") {
				t.Fatalf("startup output discloses key material:\n%s", e.output)
			}
		})
	}

	t.Run("key replaced by restart", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.protectedPolicy(), seedOptions{})
		e.start(binaries.plain)
		oldToken, newToken := issuer.tokenFor(t, "alice"), other.tokenFor(t, "alice")
		before := e.status().JWTKey
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+oldToken); code != http.StatusOK {
			t.Fatalf("token from the configured key: %d", code)
		}
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+newToken); code != http.StatusUnauthorized {
			t.Fatalf("token from the replacement key before restart: %d", code)
		}

		// Replacing the file changes nothing until the gateway restarts.
		if err := os.WriteFile(e.publicKey, other.publicPEM(), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+newToken); code != http.StatusUnauthorized || e.status().JWTKey != before {
			t.Fatalf("replacement key used without a restart: %d", code)
		}

		e.start(binaries.plain)
		if e.status().JWTKey == before || len(e.status().JWTKey) != 64 {
			t.Fatalf("fingerprint %q after restart, was %q", e.status().JWTKey, before)
		}
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+oldToken); code != http.StatusUnauthorized {
			t.Fatalf("token from the old key after restart: %d", code)
		}
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+newToken); code != http.StatusOK {
			t.Fatalf("token from the new key after restart: %d", code)
		}
		e.assertReceipts(2, 0, 0)
	})
}
