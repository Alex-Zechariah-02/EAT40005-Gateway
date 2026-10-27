package services

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

var testSecret = []byte("01234567890123456789012345678901")

const checkoutBody = `{"merchant":"MER-005","items":[{"item":"ITM-501","quantity":2}],"address":{"line1":"12 Jalan Telawi 3","line2":"","postcode":"59100","city":"Kuala Lumpur","state":"Wilayah Persekutuan Kuala Lumpur"},"payment":"wallet","note":"Please ring the bell"}`

func newCommerce(t *testing.T) (*Service, *Service) {
	t.Helper()
	inventory, err := New(Config{Name: "inventory", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(inventory)
	t.Cleanup(server.Close)
	orders, err := New(Config{Name: "orders", Secret: testSecret, InventoryURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return inventory, orders
}

func serviceRequest(s *Service, method, path, body, subject, roles, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Subject", subject)
	r.Header.Set("X-Gateway-Roles", roles)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestOrderCheckoutAndCancellation(t *testing.T) {
	inventory, orders := newCommerce(t)
	w := serviceRequest(orders, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", "checkout-idempotent-01")
	if w.Code != 201 || w.Header().Get("X-Service") != "orders" {
		t.Fatalf("checkout: %d %s", w.Code, w.Body)
	}
	var created struct {
		Order Order `json:"order"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Order.ID != "ORD-1005" || created.Order.Total != 5380 || created.Order.DeliveryFee != 0 || created.Order.Customer != "cust-003" {
		t.Fatalf("order: %s", w.Body)
	}
	first := w.Body.String()
	w = serviceRequest(orders, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", "checkout-idempotent-01")
	if w.Code != 201 || w.Body.String() != first {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	w = serviceRequest(orders, "POST", "/orders", strings.Replace(checkoutBody, `"quantity":2`, `"quantity":1`, 1), "cust-003", "CUSTOMER", "checkout-idempotent-01")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "idempotency_conflict") {
		t.Fatalf("conflict: %d %s", w.Code, w.Body)
	}
	w = serviceRequest(orders, "GET", "/orders/wallet", "", "cust-003", "CUSTOMER", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"balance_sen":4620`) {
		t.Fatalf("wallet: %d %s", w.Code, w.Body)
	}
	w = serviceRequest(inventory, "GET", "/inventory/stock/ITM-501", "", "ops-01", "OPERATIONS", "")
	if !strings.Contains(w.Body.String(), `"reserved":2`) {
		t.Fatalf("reservation: %s", w.Body)
	}
	w = serviceRequest(orders, "POST", "/orders/cancel/ORD-1005", "", "cust-003", "CUSTOMER", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	w = serviceRequest(orders, "GET", "/orders/wallet", "", "cust-003", "CUSTOMER", "")
	if !strings.Contains(w.Body.String(), `"balance_sen":10000`) {
		t.Fatalf("refund: %s", w.Body)
	}
	w = serviceRequest(inventory, "GET", "/inventory/stock/ITM-501", "", "ops-01", "OPERATIONS", "")
	if !strings.Contains(w.Body.String(), `"reserved":0`) {
		t.Fatalf("release: %s", w.Body)
	}
	w = serviceRequest(orders, "POST", "/orders/cancel/ORD-1005", "", "cust-003", "CUSTOMER", "")
	if w.Code != 409 {
		t.Fatalf("second refund: %d %s", w.Code, w.Body)
	}
}

func TestOrderRoutesAndErrors(t *testing.T) {
	_, orders := newCommerce(t)
	for _, tc := range []struct {
		method, path, body, subject, roles, code string
		status                                   int
	}{
		{"GET", "/orders/quote?merchant=MER-005", "", "cust-001", "CUSTOMER", "", 200},
		{"GET", "/orders/mine?limit=1", "", "cust-001", "CUSTOMER", "", 200},
		{"GET", "/orders/wallet", "", "cust-001", "CUSTOMER", "", 200},
		{"GET", "/orders/detail/ORD-1002", "", "cust-002", "CUSTOMER", "", 200},
		{"GET", "/orders/detail/ORD-1002", "", "cust-001", "CUSTOMER", "not_found", 404},
		{"GET", "/orders/merchant?status=placed", "", "merchant-001", "MERCHANT", "", 200},
		{"GET", "/orders/jobs", "", "rider-01", "RIDER", "", 200},
		{"GET", "/orders?status=placed&limit=5", "", "support-01", "SUPPORT", "", 200},
		{"GET", "/orders?status=unknown", "", "support-01", "SUPPORT", "invalid_query", 400},
		{"POST", "/orders", strings.Replace(checkoutBody, "ITM-501", "ITM-503", 1), "cust-003", "CUSTOMER", "out_of_stock", 409},
		{"POST", "/orders", strings.ReplaceAll(strings.ReplaceAll(checkoutBody, "MER-005", "MER-004"), "ITM-501", "ITM-033"), "cust-003", "CUSTOMER", "item_unavailable", 422},
		{"POST", "/orders", strings.Replace(checkoutBody, "ITM-501", "ITM-001", 1), "cust-003", "CUSTOMER", "mixed_merchants", 422},
		{"POST", "/orders", strings.Replace(checkoutBody, `"quantity":2`, `"quantity":4`, 1), "cust-003", "CUSTOMER", "insufficient_wallet", 409},
		{"POST", "/orders/ready/ORD-1001", "", "merchant-001", "MERCHANT", "invalid_transition", 409},
	} {
		w := serviceRequest(orders, tc.method, tc.path, tc.body, tc.subject, tc.roles, "")
		if w.Code != tc.status || tc.code != "" && !strings.Contains(w.Body.String(), `"error":"`+tc.code+`"`) {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
}

func TestOrderStatusGraph(t *testing.T) {
	_, orders := newCommerce(t)
	for _, tc := range []struct {
		path, subject, roles, status string
		code                         int
	}{
		{"accept", "merchant-001", "MERCHANT", "accepted", 200},
		{"ready", "merchant-001", "MERCHANT", "ready", 200},
		{"pickup", "rider-01", "RIDER", "picked_up", 200},
		{"pickup", "rider-02", "RIDER", "already_assigned", 409},
		{"deliver", "rider-02", "RIDER", "not_found", 404},
		{"deliver", "rider-01", "RIDER", "delivered", 200},
		{"cancel", "cust-001", "CUSTOMER", "invalid_transition", 409},
	} {
		w := serviceRequest(orders, "POST", "/orders/"+tc.path+"/ORD-1001", "", tc.subject, tc.roles, "")
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.status) {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
}
