package services

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReportsContractAndLiveOrders(t *testing.T) {
	_, orderService := newCommerce(t)
	server := httptest.NewServer(orderService)
	defer server.Close()
	reportService, err := New(Config{Name: "reports", Secret: testSecret, OrdersURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, subject, roles string }{
		{"/reports/merchant?from=2026-08-01&to=2026-10-31", "merchant-001", "MERCHANT"},
		{"/reports/summary?from=2026-08-01&to=2026-10-31", "ops-01", "OPERATIONS"},
		{"/reports/sales?from=2026-08-01&to=2026-10-31", "ops-01", "OPERATIONS"},
		{"/reports/top-items?from=2026-08-01&to=2026-10-31&limit=3", "ops-01", "OPERATIONS"},
	} {
		w := serviceRequest(reportService, "GET", tc.path, "", tc.subject, tc.roles, "")
		if w.Code != 200 || w.Header().Get("X-Service") != "reports" {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/reports/summary?from=2026-02-30", "/reports/summary?from=2026-10-31&to=2026-10-01", "/reports/top-items?limit=101", "/reports/sales?from=x&from=y"} {
		w := serviceRequest(reportService, "GET", path, "", "ops-01", "OPERATIONS", "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_query") {
			t.Errorf("query %s: %d %s", path, w.Code, w.Body)
		}
	}
	read := func() (int, int64) {
		w := serviceRequest(reportService, "GET", "/reports/summary?from=2026-08-01&to=2026-12-31", "", "ops-01", "OPERATIONS", "")
		var result struct {
			Orders  int
			Revenue int64 `json:"revenue_sen"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("summary: %d %s", w.Code, w.Body)
		}
		return result.Orders, result.Revenue
	}
	before, revenue := read()
	w := serviceRequest(orderService, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", "reports-live-checkout")
	if w.Code != 201 {
		t.Fatalf("live order: %d %s", w.Code, w.Body)
	}
	after, total := read()
	if after != before+1 || total != revenue+5380 {
		t.Fatalf("live aggregation: count %d -> %d, revenue %d -> %d", before, after, revenue, total)
	}
}
