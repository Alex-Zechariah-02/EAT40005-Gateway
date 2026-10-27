package services

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func reserveRequest(s *Service, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Service-Key", string(testSecret))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestReservationConcurrencyAndLatePrepare(t *testing.T) {
	s, _ := New(Config{Name: "inventory", Secret: testSecret})
	body := `{"merchant":"MER-005","items":[{"item":"ITM-502","quantity":4},{"item":"ITM-502","quantity":4}]}`
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			statuses[i] = reserveRequest(s, "POST", "/internal/reservations", body, fmt.Sprintf("reservation-race-%d", i)).Code
		})
	}
	wg.Wait()
	if !(statuses[0] == 200 && statuses[1] == 409 || statuses[0] == 409 && statuses[1] == 200) {
		t.Fatalf("oversell or duplicate-line undercount: %v", statuses)
	}
	if s.data.(*inventory).item("ITM-502").Reserved != 8 {
		t.Fatal("duplicate lines did not reserve their total quantity")
	}
	key := "reservation-late-prepare"
	if w := reserveRequest(s, "DELETE", "/internal/reservations/"+key, "", key); w.Code != 200 {
		t.Fatalf("release before prepare: %d", w.Code)
	}
	if w := reserveRequest(s, "POST", "/internal/reservations", body, key); w.Code != 409 {
		t.Fatalf("late prepare survived the release tombstone: %d", w.Code)
	}
}

func TestConcurrentCheckoutReplayAndScope(t *testing.T) {
	stockService, orderService := newCommerce(t)
	responses := make([]*httptest.ResponseRecorder, 6)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Go(func() {
			responses[i] = serviceRequest(orderService, "POST", "/orders", checkoutBody, "cust-003", "CUSTOMER", "concurrent-same-checkout")
		})
	}
	wg.Wait()
	for _, response := range responses {
		if response.Code != 201 || response.Body.String() != responses[0].Body.String() {
			t.Fatal("concurrent checkout did not replay the frozen first result")
		}
	}
	if stockService.data.(*inventory).item("ITM-501").Reserved != 2 || orderService.data.(*orders).wallets["cust-003"] != 4620 {
		t.Fatal("replay duplicated stock or wallet effects")
	}
	// Another account has an independent checkout identity even with the same key.
	body := strings.Replace(checkoutBody, `"wallet"`, `"cash"`, 1)
	if w := serviceRequest(orderService, "POST", "/orders", body, "cust-001", "CUSTOMER", "concurrent-same-checkout"); w.Code != 201 {
		t.Fatalf("checkout key was shared between accounts: %d", w.Code)
	}
}
