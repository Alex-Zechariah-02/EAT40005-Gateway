// Overrides with fixtures, routing and upstreams.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Overrides through the three fixture APIs, whose own receipts and state
// show what was executed, then the reset to the stable state for
// subsequent requests: one confirmed base, no provisional base, no active
// override, empty rate counters and fixtures at their seeds.
func TestOverridesWithFixtures(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	initial := e.status()
	seedHash := map[string]string{}
	for _, f := range loadCatalogue(t).Fixtures {
		seedHash[f.Service] = f.SeedSHA256
	}
	resetFixtures := func() {
		for _, f := range fixtures {
			f.reset()
		}
	}

	// Exception: staff may read the sales report.
	resetFixtures()
	if code, _ := e.get("/reports/sales", bearer(t, "staff-01")...); code != http.StatusForbidden || receiptCounts(fixtures)["reports"] != 0 {
		t.Fatalf("sales report under the base: %d", code)
	}
	sales := e.mustOverride(harness, permitOverride(overrideException, "STAFF", "reports.sales", 600), `"r1"`, "fixtures-sales-00001")
	if code, _ := e.get("/reports/sales", bearer(t, "staff-01")...); code != http.StatusOK || receiptCounts(fixtures)["reports"] != 1 {
		t.Fatalf("sales report under the exception: %d %v", code, receiptCounts(fixtures))
	}
	// Still a bearer token is required.
	if code, _ := e.get("/reports/sales"); code != http.StatusUnauthorized || receiptCounts(fixtures)["reports"] != 1 {
		t.Fatalf("anonymous sales report: %d", code)
	}

	// Containment: no orders may be created.
	order := `{"customer":"cust-001","item":"ITM-001","quantity":1}`
	block := e.mustOverride(harness, operationContainment("orders.create", 600), `"r1"`, "fixtures-block-00001")
	resetFixtures()
	if code, _ := e.request(http.MethodPost, "/orders", "application/json", order, bearer(t, "cust-001")...); code != http.StatusForbidden ||
		receiptCounts(fixtures)["orders"] != 0 || fixtures["orders"].state().SHA256 != seedHash["orders"] {
		t.Fatalf("order creation under the containment: %d", code)
	}
	// Rate containment: staff may list orders once per window.
	limit := e.mustOverride(harness, rateOverride(overrideContainment, "orders.list", 1, 600), `"r1"`, "fixtures-limit-00001")
	codes := []int{}
	for range 2 {
		code, _ := e.get("/orders", bearer(t, "staff-01")...)
		codes = append(codes, code)
	}
	if !slices.Equal(codes, []int{200, 429}) || receiptCounts(fixtures)["orders"] != 1 {
		t.Fatalf("order lists under the rate containment: %v %v", codes, receiptCounts(fixtures))
	}
	// Ending the containment restores creation, still schema-validated.
	if code, _, body := e.revokeOverride(harness, block.Override, "fixtures-block-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	resetFixtures()
	if code, _ := e.request(http.MethodPost, "/orders", "application/json", `{"customer":"cust-001"}`, bearer(t, "cust-001")...); code != http.StatusBadRequest ||
		receiptCounts(fixtures)["orders"] != 0 {
		t.Fatalf("invalid order: %d", code)
	}
	if code, _ := e.request(http.MethodPost, "/orders", "application/json", order, bearer(t, "cust-001")...); code != http.StatusCreated ||
		receiptCounts(fixtures)["orders"] != 1 || fixtures["orders"].state().SHA256 == seedHash["orders"] {
		t.Fatalf("order creation after the containment: %d", code)
	}

	// Reset: end every override, then restart (rate counters are process
	// memory) and reset the fixtures.
	for i, id := range []int64{sales.Override, limit.Override} {
		if code, _, body := e.revokeOverride(harness, id, fmt.Sprintf("fixtures-reset-%02d-xx", i)); code != http.StatusOK {
			t.Fatalf("revocation: %d %s", code, body)
		}
	}
	e.stop()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	resetFixtures()
	s := e.status()
	view := e.activeOverrides(harness)
	if s.Provisional != 0 || s.Confirmed != initial.Confirmed || s.Generation != initial.Generation || len(s.Overrides) != 0 || len(view.Overrides) != 0 {
		t.Fatalf("state after the reset %+v, %+v; initial %+v", s, view, initial)
	}
	if code, _, _ := e.as(harness, http.MethodGet, "/admin/provisional", ""); code != http.StatusNotFound {
		t.Fatalf("provisional view after the reset: %d", code)
	}
	for name, f := range fixtures {
		if state := f.state(); state.SHA256 != seedHash[name] || len(f.receipts()) != 0 {
			t.Fatalf("fixture %s after the reset: %s", name, state.SHA256)
		}
	}
	// The counter the containment used is empty again: staff list orders.
	if code, _ := e.get("/orders", bearer(t, "staff-01")...); code != http.StatusOK {
		t.Fatalf("order list after the reset: %d", code)
	}
	fixtures["orders"].reset()
}

// With overrides active, routing and forwarding are unchanged: encoded,
// dot-segment and case variants are refused before routing, an unknown
// method is not allowed, a contained operation reaches nothing, and a failed
// upstream is reported without any other destination being tried.
func TestOverridesKeepRoutingAndUpstreams(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "routing-close-00001")
	e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "routing-grant-00001")

	e.resetReceivers()
	rows := []struct {
		request string
		status  int
	}{
		{"GET /b/x HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusForbidden},
		{"GET /b/%2e%2e/a/x HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusBadRequest},
		{"GET /a/..%2Fb/x HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusBadRequest},
		{"GET /B/x HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusNotFound},
		{"DELETE /a/item HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusMethodNotAllowed},
		{"GET /d HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusForbidden},
	}
	for _, r := range rows {
		if code, _ := e.raw(r.request); code != r.status {
			t.Errorf("%q: %d, want %d", strings.SplitN(r.request, "\r\n", 2)[0], code, r.status)
		}
	}
	e.assertReceipts(0, 0, 0)

	// Upstream A stops: the granted write fails as an upstream failure and
	// nothing else is reached.
	e.a.server.Close()
	code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, bearer(t, "bob")...)
	if code != http.StatusBadGateway {
		t.Fatalf("granted write with A down: %d", code)
	}
	e.assertReceipts(0, 0, 0)
	events := e.eventsWhen(func(events []map[string]any) bool {
		for _, ev := range events {
			if ev["kind"] == "request.forwarded" && ev["decision"] == "upstream_failure" {
				return true
			}
		}
		return false
	})
	found := false
	for _, ev := range events {
		if ev["kind"] == "request.forwarded" && ev["decision"] == "upstream_failure" && ev["upstream"] == "A" && strings.Contains(fmt.Sprint(ev["generation"]), "+o1.2-") {
			found = true
		}
	}
	if !found {
		t.Fatal("no upstream failure event under the override generation")
	}
}
