// Role matrix and object ownership.

package gateway

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	fixture "github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/services/testfixture"
)

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
