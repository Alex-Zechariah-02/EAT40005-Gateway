package services

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSupportContract(t *testing.T) {
	s, err := New(Config{Name: "support", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body, subject, roles, contains string
		status                                       int
	}{
		{"GET", "/support/mine", "", "cust-002", "CUSTOMER", "TKT-1001", 200},
		{"GET", "/support/queue?status=open", "", "support-01", "SUPPORT", "TKT-1001", 200},
		{"GET", "/support/ticket/TKT-1001", "", "cust-001", "CUSTOMER", "not_found", 404},
		{"GET", "/support/ticket/TKT-1001", "", "cust-002", "CUSTOMER", "Missing item", 200},
		{"POST", "/support/reply/TKT-1001", `{"message":"We are checking this for you."}`, "support-01", "SUPPORT", `"status":"answered"`, 200},
		{"POST", "/support/reply/TKT-1001", `{"message":"Thank you for checking."}`, "cust-002", "CUSTOMER", `"status":"open"`, 200},
		{"POST", "/support/resolve/TKT-1001", "", "support-01", "SUPPORT", `"status":"resolved"`, 200},
		{"POST", "/support/reply/TKT-1001", `{"message":"Another question"}`, "cust-002", "CUSTOMER", "invalid_transition", 409},
		{"POST", "/support/tickets", `{"subject":"Delivery question","message":"Where is my delivery?","order":"ORD-1001","request_ref":"0123456789abcdef"}`, "cust-001", "CUSTOMER", "TKT-1002", 201},
		{"GET", "/support/queue?status=bad", "", "support-01", "SUPPORT", "invalid_query", 400},
		{"GET", "/support/mine?limit=3", "", "cust-001", "CUSTOMER", "invalid_query", 400},
		{"POST", "/support/tickets", `{"subject":"tiny","message":"Hello"}`, "cust-001", "CUSTOMER", "invalid_request", 400},
		{"POST", "/support/reply/TKT-1002", `{"message":""}`, "cust-001", "CUSTOMER", "invalid_request", 400},
	} {
		w := serviceRequest(s, tc.method, tc.path, tc.body, tc.subject, tc.roles, "")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) || w.Header().Get("X-Service") != "support" {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	w := serviceRequest(s, "GET", "/support/ticket/TKT-1002", "", "cust-001", "CUSTOMER", "")
	var body map[string]map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body["ticket"]) != 9 {
		t.Fatalf("ticket fields: %s", w.Body)
	}
	// Staff read names, not account IDs: the ticket and each message carry
	// the seed user's display name.
	var named struct {
		Ticket struct {
			CustomerName string `json:"customer_name"`
			Messages     []struct {
				AuthorName string `json:"author_name"`
			} `json:"messages"`
		} `json:"ticket"`
	}
	if json.Unmarshal(w.Body.Bytes(), &named) != nil || named.Ticket.CustomerName != "Customer 1" || len(named.Ticket.Messages) == 0 || named.Ticket.Messages[0].AuthorName != "Customer 1" {
		t.Fatalf("ticket names: %s", w.Body)
	}
}
