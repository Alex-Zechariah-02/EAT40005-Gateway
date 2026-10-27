package services

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInventoryPublicContract(t *testing.T) {
	s, err := New(Config{Name: "inventory", Secret: []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/inventory/categories", "/inventory/merchants", "/inventory/merchants/MER-001", "/inventory/search?q=nasi", "/inventory/items/ITM-001"} {
		response := httptest.NewRecorder()
		s.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || response.Header().Get("X-Service") != "inventory" {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatalf("invalid JSON: %s", response.Body)
		}
	}
}

func TestInventoryStateResetAndLimits(t *testing.T) {
	s, _ := New(Config{Name: "inventory", Secret: []byte("01234567890123456789012345678901")})
	before := s.state().SHA256
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Gateway-Subject", "merchant-005")
		r.Header.Set("X-Gateway-Roles", "MERCHANT")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := call("PATCH", "/inventory/stock/ITM-501", `{"delta":5}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"stock":45`) {
		t.Fatalf("adjust: %d %s", w.Code, w.Body)
	}
	if w := call("PATCH", "/inventory/items/ITM-501", `{"price_sen":3000,"available":false}`); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if s.state().SHA256 == before {
		t.Fatal("mutations did not change state")
	}
	for _, tc := range []struct {
		path, body, code string
		status           int
	}{
		{"/inventory/items/ITM-501", `{"Price_sen":100}`, "invalid_request", 400},
		{"/inventory/stock/ITM-501", `{"Delta":1}`, "invalid_request", 400},
		{"/inventory/items/ITM-501", `{"available":null}`, "invalid_request", 400},
		{"/inventory/items/ITM-501", strings.Repeat(" ", 512) + `{}`, "payload_too_large", 413},
		{"/inventory/stock/ITM-501", strings.Repeat(" ", 256) + `{}`, "payload_too_large", 413},
	} {
		w := call("PATCH", tc.path, tc.body)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"error":"`+tc.code+`"`) {
			t.Errorf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	if w := call("GET", "/inventory/items/ITM-501", `{}`); w.Code != 400 {
		t.Errorf("GET body: %d", w.Code)
	}
	control := s.Control(s.secret)
	r := httptest.NewRequest("POST", "/reset", nil)
	r.Header.Set("Authorization", "Bearer "+string(s.secret))
	w := httptest.NewRecorder()
	control.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("X-Service") != "inventory" || s.state().SHA256 != before || len(s.receipts) != 0 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	for _, item := range s.data.(*inventory).items {
		view := publicItem(item)
		if view.Description == "" {
			t.Errorf("missing description for %s", view.ID)
		}
		data, _ := json.Marshal(view)
		if bytes.Contains(data, []byte(`"stock":`)) || bytes.Contains(data, []byte(`"reserved":`)) {
			t.Fatalf("public stock leak: %s", data)
		}
	}
}

func TestInventoryReservationTransactions(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	s, _ := New(Config{Name: "inventory", Secret: secret})
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Service-Key", string(secret))
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	body := `{"merchant":"MER-005","items":[{"item":"ITM-501","quantity":2}]}`
	w := call("POST", "/internal/reservations", body, "reservation-first-01")
	if w.Code != 200 {
		t.Fatalf("reserve: %d %s", w.Code, w.Body)
	}
	first := w.Body.String()
	if w = call("POST", "/internal/reservations", body, "reservation-first-01"); w.Code != 200 || w.Body.String() != first {
		t.Fatalf("duplicate reservation: %d %s", w.Code, w.Body)
	}
	if s.data.(*inventory).item("ITM-501").Reserved != 2 {
		t.Fatal("duplicate reservation applied twice")
	}
	if w = call("POST", "/internal/reservations", strings.ReplaceAll(body, `"quantity":2`, `"quantity":3`), "reservation-first-01"); w.Code != 409 {
		t.Fatalf("conflict: %d %s", w.Code, w.Body)
	}
	if w = call("POST", "/internal/reservations", `{"merchant":"MER-005","items":[{"item":"ITM-501","quantity":1},{"item":"ITM-503","quantity":1}]}`, "reservation-out-of-stock"); w.Code != 409 || !strings.Contains(w.Body.String(), `"field":"items[1].item"`) {
		t.Fatalf("out of stock: %d %s", w.Code, w.Body)
	}
	if s.data.(*inventory).item("ITM-501").Reserved != 2 {
		t.Fatal("failed multi-item reservation half-applied")
	}
	for range 2 {
		if w = call("DELETE", "/internal/reservations/reservation-first-01", "", "reservation-first-01"); w.Code != 200 {
			t.Fatalf("release: %d %s", w.Code, w.Body)
		}
	}
	if s.data.(*inventory).item("ITM-501").Reserved != 0 {
		t.Fatal("release did not restore stock")
	}
}

func TestServiceRunStopsOwnedListeners(t *testing.T) {
	// Real listener lifecycle, with addresses allocated independently of the
	// frontend's fixed ports.
	s, _ := New(Config{Name: "inventory", Secret: []byte("01234567890123456789012345678901")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, s, "127.0.0.1:0", "127.0.0.1:0", s.secret); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), s, "0.0.0.0:0", "127.0.0.1:0", s.secret); err == nil {
		t.Fatal("wildcard service listener accepted")
	}
}

func TestInventoryRefusalsAndOwnership(t *testing.T) {
	s, err := New(Config{Name: "inventory", Secret: []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body, subject, roles, code string
		status                                   int
	}{
		{"GET", "/inventory/search?q=x", "", "", "", "invalid_query", 400},
		{"GET", "/inventory/search?q=nasi&section=other", "", "", "", "invalid_query", 400},
		{"GET", "/inventory/merchants?limit=0", "", "", "", "invalid_query", 400},
		{"GET", "/inventory/merchants?unknown=x", "", "", "", "invalid_query", 400},
		{"GET", "/inventory/items/ITM-999", "", "", "", "not_found", 404},
		{"PATCH", "/inventory/items/ITM-501", `{"price_sen":500}`, "merchant-001", "MERCHANT", "not_found", 404},
		{"PATCH", "/inventory/items/ITM-501", `{}`, "merchant-005", "MERCHANT", "invalid_change", 422},
		{"PATCH", "/inventory/items/ITM-501", `{"price_sen":49}`, "merchant-005", "MERCHANT", "invalid_change", 422},
		{"PATCH", "/inventory/stock/ITM-501", `{"delta":0}`, "merchant-005", "MERCHANT", "invalid_change", 422},
		{"PATCH", "/inventory/stock/ITM-501", `{"delta":-41}`, "merchant-005", "MERCHANT", "insufficient_stock", 409},
		{"PATCH", "/inventory/stock/ITM-501", `{"delta":1,"delta":2}`, "merchant-005", "MERCHANT", "invalid_request", 400},
		{"GET", "/inventory/stock/ITM-501", "", "merchant-001", "MERCHANT", "not_found", 404},
		{"POST", "/internal/reservations", `{}`, "", "", "service_key_required", 401},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Gateway-Subject", tc.subject)
		r.Header.Set("X-Gateway-Roles", tc.roles)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var body struct{ Error, Message string }
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || w.Code != tc.status || body.Error != tc.code || body.Message == "" || w.Header().Get("X-Service") != "inventory" {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
}

func TestInventoryStockHonorsGatewayPermissionAndMerchantOwnership(t *testing.T) {
	s, err := New(Config{Name: "inventory", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	// The service receives an already permitted request from the gateway.
	// Stock ownership is restricted for merchants, not a second role policy.
	for _, tc := range []struct {
		method, body, subject, roles string
		status                       int
	}{
		{"GET", "", "cust-001", "CUSTOMER", 200},
		{"PATCH", `{"delta":1}`, "cust-001", "CUSTOMER", 200},
		{"GET", "", "merchant-001", "MERCHANT", 404},
		{"PATCH", `{"delta":1}`, "merchant-001", "MERCHANT", 404},
		{"GET", "", "merchant-005", "MERCHANT", 200},
		{"GET", "", "ops-01", "OPERATIONS", 200},
		{"GET", "", "", "", 404},
	} {
		response := serviceRequest(s, tc.method, "/inventory/stock/ITM-501", tc.body, tc.subject, tc.roles, "")
		if response.Code != tc.status {
			t.Errorf("%s role=%s stock: %d want %d", tc.method, tc.roles, response.Code, tc.status)
		}
	}
}
