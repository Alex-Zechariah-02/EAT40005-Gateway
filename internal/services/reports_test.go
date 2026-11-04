package services

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
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

// A caller that cancels is recorded as a cancellation, never as an Orders
// outage, while a real Orders failure is still reported as one.
func TestReportsRecordCallerCancellation(t *testing.T) {
	_, orderService := newCommerce(t)
	server := httptest.NewServer(orderService)
	reportService, err := New(Config{Name: "reports", Secret: testSecret, OrdersURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer reportService.data.(*reports).close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest("GET", "/reports/summary", nil).WithContext(ctx)
	request.Header.Set("X-Gateway-Subject", "ops-01")
	request.Header.Set("X-Gateway-Roles", "OPERATIONS")
	reportService.ServeHTTP(httptest.NewRecorder(), request)
	server.Close()
	outage := serviceRequest(reportService, "GET", "/reports/summary", "", "ops-01", "OPERATIONS", "")
	if outage.Code != 503 || !strings.Contains(outage.Body.String(), "orders_unavailable") {
		t.Fatalf("Orders outage: %d %s", outage.Code, outage.Body)
	}
	statuses := []int{}
	for _, receipt := range reportService.receipts {
		statuses = append(statuses, receipt.Status)
	}
	if !slices.Equal(statuses, []int{499, 503}) {
		t.Fatalf("receipt statuses %v, want cancellation 499 then outage 503", statuses)
	}
}

func TestPlatformReportsHonorGatewayPermission(t *testing.T) {
	_, orderService := newCommerce(t)
	server := httptest.NewServer(orderService)
	defer server.Close()
	reportService, err := New(Config{Name: "reports", Secret: testSecret, OrdersURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer reportService.data.(*reports).close()
	for _, caller := range []struct{ subject, roles string }{{"cust-001", "CUSTOMER"}, {"rider-01", "RIDER"}, {"merchant-001", "MERCHANT"}, {"support-01", "SUPPORT"}, {"ops-01", "OPERATIONS"}} {
		for _, path := range []string{"/reports/summary", "/reports/sales", "/reports/top-items"} {
			response := serviceRequest(reportService, "GET", path, "", caller.subject, caller.roles, "")
			if response.Code != 200 {
				t.Errorf("gateway-permitted %s %s: %d", caller.roles, path, response.Code)
			}
		}
	}
	if response := serviceRequest(reportService, "GET", "/reports/summary", "", "", "OPERATIONS", ""); response.Code != 404 {
		t.Fatal("Reports accepted missing forwarded identity")
	}
	if response := serviceRequest(reportService, "GET", "/reports/merchant", "", "cust-001", "CUSTOMER", ""); response.Code != 404 {
		t.Fatal("customer acquired an unowned merchant report")
	}
	for _, owner := range []struct{ subject, merchant string }{{"merchant-001", "MER-001"}, {"merchant-005", "MER-005"}} {
		response := serviceRequest(reportService, "GET", "/reports/merchant", "", owner.subject, "MERCHANT", "")
		var view struct {
			Merchant string `json:"merchant"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Merchant != owner.merchant {
			t.Fatal("merchant report did not retain store ownership")
		}
	}
}
