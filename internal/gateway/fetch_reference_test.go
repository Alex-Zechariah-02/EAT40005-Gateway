package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchIngressDiscardsClientIdentityHeaders(t *testing.T) {
	e := newEnv(t)
	_, handler, _ := e.serveEnforcement(e.policy(e.a, true), 0)
	for _, method := range []string{"CONNECT", "POST"} {
		request := httptest.NewRequest(method, "http://gateway/a/x", nil)
		request.RequestURI = "/a/x"
		if method == "POST" {
			request.ContentLength = defaultMaxBodyBytes + 1
		}
		request.Header.Set("X-Request-Id", "client-value")
		request.Header.Set("X-Gateway-Subject", "forged")
		request.Header.Set("X-Gateway-Extra", "forged")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 400 && response.Code != 413 {
			t.Fatalf("ingress status: %d", response.Code)
		}
		for _, name := range []string{"X-Request-Id", "X-Gateway-Subject", "X-Gateway-Extra"} {
			if request.Header.Get(name) != "" {
				t.Errorf("%s retained %s", method, name)
			}
		}
	}
	e.assertReceipts(0, 0, 0)
}

func TestFetchSeparateNetworkAddressRespectsOffset(t *testing.T) {
	t.Setenv("EAT_PORT_OFFSET", "1000")
	if got := wslDataAddress(t, "192.0.2.1"); got != "192.0.2.1:19443" {
		t.Fatalf("separate-network listener: %s", got)
	}
}

// A reference must correlate the response with both audit and upstream receipt,
// even when either the client or an upstream tries to supply its own reference.
func TestFetchResponseReferences(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("X-Request-Id", "upstream-forged")
		w.WriteHeader(http.StatusCreated)
		return true
	}
	e.start(binaries.plain)
	for _, tc := range []struct {
		method, target string
		status         int
	}{
		{http.MethodGet, "/a/x", 201},
		{http.MethodGet, "/d", 403},
		{http.MethodGet, "/unknown", 404},
		{http.MethodDelete, "/a/x", 405},
		{http.MethodGet, "/a/%2f", 400},
	} {
		resp, body := e.exchange(tc.method, tc.target, "", "", "X-Request-Id", "client-forged", "X-Gateway-Subject", "root")
		if resp.StatusCode != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.target, resp.StatusCode, body)
		}
		id := resp.Header.Get("X-Request-Id")
		if len(id) != 32 {
			t.Fatalf("%s has no gateway reference: %q", tc.target, id)
		}
		if tc.status == http.StatusCreated {
			receipts := e.a.receipts()
			if len(receipts) != 1 || receipts[0].RequestID != id || receipts[0].Headers.Get("X-Gateway-Subject") != "" {
				t.Fatalf("response reference disagrees with trusted receipt: %+v", receipts)
			}
		} else if _, bodyID := requestID(resp, body); bodyID != id {
			t.Fatalf("response/body references disagree: %q vs %q", id, bodyID)
		}
		if events := e.requestEvents(id, 1); len(events) == 0 {
			t.Fatalf("reference %s cannot be found in audit", id)
		}
	}
	e.assertReceipts(1, 0, 0)
	e.a.server.Close()
	resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
	if resp.StatusCode != http.StatusBadGateway || len(resp.Header.Get("X-Request-Id")) != 32 {
		t.Fatalf("forwarding failure lost its reference: %d %s headers %v", resp.StatusCode, body, resp.Header)
	}
}
