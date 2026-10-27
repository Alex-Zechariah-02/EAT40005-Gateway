package testfixture

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests exercise the fixtures in process, without the gateway, to
// establish what each operation does on its own. The gateway integration
// tests replay the same catalogue through the real proxy.

func loadCatalogue(t *testing.T) Catalogue {
	t.Helper()
	c, err := LoadCatalogue()
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	return c
}

func TestSeedMatchesCatalogue(t *testing.T) {
	c := loadCatalogue(t)
	if len(c.Fixtures) != 3 {
		t.Fatalf("catalogue lists %d fixtures, want exactly 3", len(c.Fixtures))
	}
	for _, f := range c.Fixtures {
		s, err := New(f.Service)
		if err != nil {
			t.Fatal(err)
		}
		state := s.state()
		if state.Version != f.Version || state.SHA256 != f.SeedSHA256 {
			t.Errorf("%s seed: version %s sha256 %s, catalogue %s %s", f.Service, state.Version, state.SHA256, f.Version, f.SeedSHA256)
		}
	}
}

func TestCatalogueOperationsExist(t *testing.T) {
	c := loadCatalogue(t)
	declared := map[string]bool{}
	for _, f := range c.Fixtures {
		s, _ := New(f.Service)
		for _, op := range s.operations {
			declared[op.id] = true
		}
	}
	for _, op := range c.Operations {
		if !declared[op.ID] {
			t.Errorf("catalogue operation %s is not implemented by any fixture", op.ID)
		}
		delete(declared, op.ID)
	}
	for id := range declared {
		t.Errorf("fixture operation %s is missing from the catalogue", id)
	}
}

// TestScenariosInProcess replays the catalogue sequence against fresh
// fixtures and checks each step against its expected outcome.
func TestScenariosInProcess(t *testing.T) {
	c := loadCatalogue(t)
	services := map[string]*Service{}
	apis := map[string]string{}
	for _, op := range c.Operations {
		apis[op.ID] = op.API
	}
	for _, f := range c.Fixtures {
		services[f.Service], _ = New(f.Service)
	}
	roles := map[string]string{}
	for _, s := range c.Subjects {
		roles[s.ID] = strings.Join(s.Roles, ",")
	}

	for _, sc := range c.Scenarios {
		s := services[apis[sc.Operation]]
		before := s.state().SHA256

		req := httptest.NewRequest(sc.Method, sc.Path, bytes.NewReader(sc.Body))
		if len(sc.Body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		// In place of the gateway, set the trusted identity it would forward.
		if sc.Subject != "" {
			req.Header.Set("X-Gateway-Subject", sc.Subject)
			req.Header.Set("X-Gateway-Roles", roles[sc.Subject])
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)

		receipt := s.receipts[len(s.receipts)-1]
		changed := s.state().SHA256 != before
		switch {
		case rec.Code != sc.Status:
			t.Errorf("%s: status %d, want %d (%s)", sc.Name, rec.Code, sc.Status, rec.Body)
		case receipt.Operation != sc.Operation:
			t.Errorf("%s: fixture ran %q, want %q", sc.Name, receipt.Operation, sc.Operation)
		case changed != sc.ChangesState:
			t.Errorf("%s: state changed %v, want %v", sc.Name, changed, sc.ChangesState)
		case receipt.Effect != sc.Effect:
			t.Errorf("%s: effect %q, want %q", sc.Name, receipt.Effect, sc.Effect)
		case len(sc.Response) > 0 && !sameJSON(t, rec.Body.Bytes(), sc.Response):
			t.Errorf("%s: response %s, want %s", sc.Name, rec.Body, sc.Response)
		case rec.Header().Get("X-Service") != s.name:
			t.Errorf("%s: served by %q", sc.Name, rec.Header().Get("X-Service"))
		}
	}
}

// Without a forwarded identity, or with roles that do not cover every
// customer, an order belongs to nobody the caller can act for.
func TestOrderOwnership(t *testing.T) {
	cases := []struct {
		name, method, path, body, subject, roles string
		status                                   int
	}{
		{"no identity reads an order", http.MethodGet, "/orders/ORD-1001", "", "", "", http.StatusNotFound},
		{"owner reads own order", http.MethodGet, "/orders/ORD-1001", "", "cust-001", "CUSTOMER", http.StatusOK},
		{"customer reads another's order", http.MethodGet, "/orders/ORD-1002", "", "cust-001", "CUSTOMER", http.StatusNotFound},
		{"staff reads any order", http.MethodGet, "/orders/ORD-1002", "", "staff-01", "STAFF", http.StatusOK},
		{"role name inside another role", http.MethodGet, "/orders/ORD-1002", "", "cust-001", "CUSTOMER,NOTSTAFF", http.StatusNotFound},
		{"no identity creates an order", http.MethodPost, "/orders", `{"customer":"cust-001","item":"ITM-001","quantity":1}`, "", "", http.StatusForbidden},
		{"customer creates for another", http.MethodPost, "/orders", `{"customer":"cust-002","item":"ITM-001","quantity":1}`, "cust-001", "CUSTOMER", http.StatusForbidden},
	}
	for _, tc := range cases {
		s, _ := New("orders")
		before := s.state().SHA256
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		if tc.subject != "" {
			req.Header.Set("X-Gateway-Subject", tc.subject)
			req.Header.Set("X-Gateway-Roles", tc.roles)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.status, rec.Body)
		}
		if rec.Code != http.StatusOK && s.state().SHA256 != before {
			t.Errorf("%s: refused request changed state", tc.name)
		}
	}
}

func TestEveryArrivalIsRecorded(t *testing.T) {
	s, _ := New("orders")
	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/unknown", nil),
		httptest.NewRequest(http.MethodDelete, "/orders", nil),
		httptest.NewRequest(http.MethodGet, "/orders/ORD-1001/extra", nil),
		httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(strings.Repeat("x", maxBodyBytes+1))),
	}
	for _, req := range requests {
		req.Header.Set("Authorization", "Bearer must-not-be-recorded")
		s.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(s.receipts) != len(requests) {
		t.Fatalf("%d receipts for %d requests", len(s.receipts), len(requests))
	}
	for i, r := range s.receipts[:3] {
		if r.Operation != "" || r.Status != http.StatusNotFound {
			t.Errorf("receipt %d: %+v, want an unmatched 404", i, r)
		}
	}
	if s.receipts[3].Status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %+v", s.receipts[3])
	}
	recorded, _ := json.Marshal(s.receipts)
	if bytes.Contains(recorded, []byte("must-not-be-recorded")) || !s.receipts[0].Authorization {
		t.Errorf("receipts must note only the presence of a credential: %s", recorded)
	}
}

func TestFullReceiptLogRefusesRequests(t *testing.T) {
	s, _ := New("inventory")
	s.receipts = make([]Receipt, maxReceipts)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/inventory/items/ITM-001", strings.NewReader(`{"delta":1}`)))
	if rec.Code != http.StatusServiceUnavailable || len(s.receipts) != maxReceipts {
		t.Fatalf("status %d with %d receipts", rec.Code, len(s.receipts))
	}
	if s.data.(*inventory).items[0].Stock != 40 {
		t.Fatal("state changed although the request was refused")
	}
}

func TestControlRequiresSecret(t *testing.T) {
	secret := []byte(strings.Repeat("s", minSecretBytes))
	s, _ := New("orders")
	control := httptest.NewServer(s.Control(secret))
	defer control.Close()

	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders", nil))
	for _, token := range []string{"", "wrong", string(secret) + "x"} {
		req, _ := http.NewRequest(http.MethodPost, control.URL+"/reset", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("reset with token %q: %d", token, resp.StatusCode)
		}
		if resp.Header.Get("X-Service") != "orders" {
			t.Error("control refusal has no service identity")
		}
	}
	if len(s.receipts) != 1 {
		t.Fatal("an unauthenticated reset cleared the receipts")
	}

	req, _ := http.NewRequest(http.MethodPost, control.URL+"/reset", nil)
	req.Header.Set("Authorization", "Bearer "+string(secret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(s.receipts) != 0 {
		t.Fatalf("authorised reset: %d %s", resp.StatusCode, body)
	}
}

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, path, id string
		ok                bool
	}{
		{"/orders", "/orders", "", true},
		{"/orders", "/orders/", "", false},
		{"/orders/{id}", "/orders/ORD-1", "ORD-1", true},
		{"/orders/{id}", "/orders/", "", false},
		{"/orders/{id}", "/orders/a/b", "", false},
		{"/orders/{id}", "/Orders/a", "", false},
	}
	for _, c := range cases {
		id, ok := matchPattern(c.pattern, c.path)
		if id != c.id || ok != c.ok {
			t.Errorf("%s ~ %s: %q %v, want %q %v", c.pattern, c.path, id, ok, c.id, c.ok)
		}
	}
}

func sameJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal(want, &b) != nil {
		return false
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
