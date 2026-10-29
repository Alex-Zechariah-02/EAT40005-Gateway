package services

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestInternalKeyPrecedesUntrustedBody(t *testing.T) {
	s, _ := New(Config{Name: "inventory", Secret: testSecret})
	request := httptest.NewRequest("POST", "/internal/reservations", strings.NewReader(strings.Repeat("x", 5000)))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, request)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "service_key_required") {
		t.Fatalf("unauthenticated internal request: %d %s", w.Code, w.Body)
	}
}

func TestOrderContractErrorsAndInventoryOutage(t *testing.T) {
	stockService, orderService := newCommerce(t)
	for _, key := range []string{"short", strings.Repeat("x", 65), "this key has invalid spaces"} {
		w := serviceRequest(orderService, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", key)
		if w.Code != 400 {
			t.Errorf("invalid checkout identity: %d", w.Code)
		}
	}
	for _, body := range []string{strings.Replace(checkoutBody, `"quantity":2`, `"quantity":0`, 1), strings.Replace(checkoutBody, `"payment":"wallet"`, `"payment":"other"`, 1), strings.Replace(checkoutBody, `"postcode":"59100"`, `"postcode":"1"`, 1)} {
		if w := serviceRequest(orderService, "POST", "/orders", body, "cust-003", "CUSTOMER", ""); w.Code != 400 {
			t.Errorf("invalid checkout body: %d %s", w.Code, w.Body)
		}
	}
	stockService.mu.Lock()
	stockService.data.(*inventory).merchants[4].Open = false
	stockService.mu.Unlock()
	w := serviceRequest(orderService, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", "")
	if w.Code != 422 || !strings.Contains(w.Body.String(), "merchant_closed") {
		t.Fatalf("closed merchant: %d %s", w.Code, w.Body)
	}
	closed := httptest.NewServer(nil)
	origin := closed.URL
	closed.Close()
	service, err := New(Config{Name: "orders", Secret: testSecret, InventoryURL: origin})
	if err != nil {
		t.Fatal(err)
	}
	w = serviceRequest(service, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", "inventory-offline-key")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "inventory_unavailable") || service.data.(*orders).wallets["cust-003"] != 10000 || len(service.data.(*orders).list) != len(seedOrders()) {
		t.Fatalf("unavailable Inventory changed checkout: %d %s", w.Code, w.Body)
	}
}

func TestAccountsSigningReloadAndCredentialWork(t *testing.T) {
	s, _, file := accountService(t)
	login := func(email string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"email": email, "password": "wrong-password"})
		return serviceRequest(s, "POST", "/accounts/login", string(body), "", "", "")
	}
	known, unknown := []time.Duration{}, []time.Duration{}
	for range 7 {
		start := time.Now()
		login("customer1@fetch.test")
		known = append(known, time.Since(start))
		start = time.Now()
		login("nobody@fetch.test")
		unknown = append(unknown, time.Since(start))
	}
	slices.Sort(known)
	slices.Sort(unknown)
	if known[3] > 3*unknown[3] || unknown[3] > 3*known[3] {
		t.Fatalf("credential work gap: known median %s, unknown median %s", known[3], unknown[3])
	}
	t.Logf("same password-work paths: known median %s, unknown median %s", known[3], unknown[3])
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte("fetch-test-a"), []byte("fetch-test-b"))
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	w := serviceRequest(s, "POST", "/accounts/login", `{"email":"customer1@fetch.test","password":"FetchDemo2026!"}`, "", "", "")
	var response struct {
		Token string `json:"token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal("signing reload login failed")
	}
	parts := strings.Split(response.Token, ".")
	if len(parts) != 3 {
		t.Fatal("invalid token shape")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var selected struct {
		Kid string `json:"kid"`
	}
	if json.Unmarshal(header, &selected) != nil || selected.Kid != "fetch-test-b" {
		t.Fatal("Accounts did not use the updated signing settings")
	}
	if err = os.WriteFile(file, []byte(`{"kid":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	w = serviceRequest(s, "POST", "/accounts/login", `{"email":"customer1@fetch.test","password":"FetchDemo2026!"}`, "", "", "")
	if w.Code != 503 || strings.Contains(w.Body.String(), file) {
		t.Fatal("invalid signing settings did not fail safely")
	}
}
