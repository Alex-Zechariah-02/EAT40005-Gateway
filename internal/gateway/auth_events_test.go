// Authentication events and unrecorded credentials.

package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

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
