package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServiceJSONBoundaries(t *testing.T) {
	s, _ := New(Config{Name: "support", Secret: testSecret})
	for _, body := range []string{`null`, `{"subject":"A valid title","message":"\ud800"}`, `{"subject":"A valid title","message":"\udc00"}`, "{\"subject\":\"A valid title\",\"message\":\"" + string([]byte{255}) + "\"}", `{"Subject":"A valid title","message":"text"}`, `{"subject":"A valid title","message":"text","message":"again"}`} {
		w := serviceRequest(s, "POST", "/support/tickets", body, "cust-001", "CUSTOMER", "")
		if w.Code != 400 {
			t.Errorf("ambiguous JSON accepted: %d %s", w.Code, w.Body)
		}
	}
	w := serviceRequest(s, "POST", "/support/tickets", `{"subject":"A valid title","message":"The literal \\ud800 is printed."}`, "cust-001", "CUSTOMER", "")
	if w.Code != 201 {
		t.Fatalf("literal escape refused: %d %s", w.Code, w.Body)
	}
}

func TestServiceRequestFieldsAreNotNullable(t *testing.T) {
	s, _ := New(Config{Name: "support", Secret: testSecret})
	for _, member := range []string{`"order":null`, `"request_ref":null`} {
		body := `{"subject":"Delivery question","message":"Please help",` + member + `}`
		w := serviceRequest(s, "POST", "/support/tickets", body, "cust-001", "CUSTOMER", "")
		if w.Code != 400 {
			t.Errorf("nullable request member accepted: %d", w.Code)
		}
	}
}

func TestServiceWriteQueryAndMediaBoundaries(t *testing.T) {
	s, _ := New(Config{Name: "support", Secret: testSecret})
	body := `{"subject":"Delivery question","message":"Where is my order?"}`
	if w := serviceRequest(s, "POST", "/support/tickets?unknown=x", body, "cust-001", "CUSTOMER", ""); w.Code != 400 {
		t.Errorf("write query accepted: %d", w.Code)
	}
	for _, types := range [][]string{{"application/json", "text/plain"}, {"application/json; charset=latin1"}, {"application/json; unknown=x"}} {
		r := httptest.NewRequest("POST", "/support/tickets", strings.NewReader(body))
		r.Header.Set("X-Gateway-Subject", "cust-001")
		r.Header.Set("X-Gateway-Roles", "CUSTOMER")
		for _, kind := range types {
			r.Header.Add("Content-Type", kind)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 415 {
			t.Errorf("media %v accepted: %d", types, w.Code)
		}
	}
}

func TestServiceBodyLimitsAndResetIsolation(t *testing.T) {
	inventory, orders := newCommerce(t)
	support, _ := New(Config{Name: "support", Secret: testSecret})
	for _, tc := range []struct {
		s                    *Service
		path, subject, roles string
		limit                int
	}{
		{inventory, "/inventory/items/ITM-501", "merchant-005", "MERCHANT", 512},
		{inventory, "/inventory/stock/ITM-501", "merchant-005", "MERCHANT", 256},
		{orders, "/orders", "cust-001", "CUSTOMER", 4096},
		{support, "/support/tickets", "cust-001", "CUSTOMER", 4096},
		{support, "/support/reply/TKT-1001", "cust-002", "CUSTOMER", 4096},
	} {
		method := "POST"
		if strings.HasPrefix(tc.path, "/inventory/") {
			method = "PATCH"
		}
		w := serviceRequest(tc.s, method, tc.path, strings.Repeat(" ", tc.limit)+`{}`, tc.subject, tc.roles, "")
		if w.Code != 413 {
			t.Errorf("%s limit: %d %s", tc.path, w.Code, w.Body)
		}
	}
	before := orders.state().SHA256
	serviceRequest(inventory, "PATCH", "/inventory/stock/ITM-501", `{"delta":1}`, "ops-01", "OPERATIONS", "")
	reset := httptest.NewRequest("POST", "/reset", nil)
	reset.Header.Set("Authorization", "Bearer "+string(testSecret))
	inventory.Control(testSecret).ServeHTTP(httptest.NewRecorder(), reset)
	if orders.state().SHA256 != before {
		t.Fatal("Inventory reset changed Orders")
	}
}

func TestCancellationReportsUnconfirmedStockRelease(t *testing.T) {
	inventory, _ := New(Config{Name: "inventory", Secret: testSecret})
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() && r.Method == "DELETE" {
			w.WriteHeader(503)
			return
		}
		inventory.ServeHTTP(w, r)
	}))
	defer server.Close()
	service, err := New(Config{Name: "orders", Secret: testSecret, InventoryURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if w := serviceRequest(service, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", "cancel-uncertain-key"); w.Code != 201 {
		t.Fatalf("checkout: %d %s", w.Code, w.Body)
	}
	fail.Store(true)
	w := serviceRequest(service, "POST", "/orders/cancel/ORD-1005", "", "cust-003", "CUSTOMER", "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "inventory_unavailable") {
		t.Errorf("unconfirmed release claimed success: %d %s", w.Code, w.Body)
	}
	order := service.data.(*orders).find("ORD-1005")
	if order.Status != "cancelled" || service.data.(*orders).wallets["cust-003"] != 10000 {
		t.Fatal("cancelled order/refund did not remain authoritative during cleanup")
	}
	fail.Store(false)
	serviceRequest(service, "GET", "/orders/wallet", "", "cust-003", "CUSTOMER", "")
	w = serviceRequest(inventory, "GET", "/inventory/stock/ITM-501", "", "ops-01", "OPERATIONS", "")
	var stock struct {
		Reserved int `json:"reserved"`
	}
	if json.Unmarshal(w.Body.Bytes(), &stock) != nil || stock.Reserved != 0 {
		t.Fatalf("pending release not settled: %s", w.Body)
	}
}

func TestServiceControlKeyBound(t *testing.T) {
	s, _ := New(Config{Name: "inventory", Secret: testSecret})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, s, "127.0.0.1:0", "127.0.0.1:0", nil); err == nil {
		t.Fatal("an empty control credential was accepted")
	}
}

func TestEveryServiceResetReproducesSeed(t *testing.T) {
	stockService, orderService := newCommerce(t)
	orderServer := httptest.NewServer(orderService)
	t.Cleanup(orderServer.Close)
	reportService, err := New(Config{Name: "reports", Secret: testSecret, OrdersURL: orderServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer reportService.data.(*reports).close()
	supportService, _ := New(Config{Name: "support", Secret: testSecret})
	accountsService, _, _ := accountService(t)
	services := []*Service{stockService, orderService, reportService, supportService, accountsService}
	before := make([]string, len(services))
	for i, service := range services {
		before[i] = service.state().SHA256
	}
	serviceRequest(orderService, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", "reset-checkout-identity")
	serviceRequest(supportService, "POST", "/support/tickets", `{"subject":"Delivery question","message":"Please help"}`, "cust-001", "CUSTOMER", "")
	serviceRequest(reportService, "GET", "/reports/summary", "", "ops-01", "OPERATIONS", "")
	serviceRequest(accountsService, "GET", "/accounts/me", "", "cust-001", "CUSTOMER", "")
	for i, service := range services {
		if len(service.receipts) == 0 {
			t.Fatalf("%s arrival was not recorded", service.name)
		}
		request := httptest.NewRequest("POST", "/reset", nil)
		request.Header.Set("Authorization", "Bearer "+string(testSecret))
		response := httptest.NewRecorder()
		service.Control(testSecret).ServeHTTP(response, request)
		if response.Code != 200 || service.state().SHA256 != before[i] || len(service.receipts) != 0 {
			t.Fatalf("%s did not reproduce its seed and clear receipts", service.name)
		}
	}
}
