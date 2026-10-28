package services

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInternalServiceOrigins(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:9101", "http://[::1]:9101", "HTTP://127.0.0.1:9101"} {
		client, err := newServiceClient(origin, []byte("local-service-key"))
		if err != nil {
			t.Errorf("valid origin %q: %v", origin, err)
			continue
		}
		client.close()
	}
	for _, origin := range []string{
		"http://127.0.0.1:9101?", "http://127.0.0.1:9101#",
		"http://127.0.0.1:9101?x=1", "http://127.0.0.1:9101#fragment",
		"http://127.0.0.1:9101/", "http://127.0.0.1:9101/internal",
		"http://user@127.0.0.1:9101", "http://localhost:9101", "http://192.0.2.1:9101",
		"http://127.0.0.1:0", "http://127.0.0.1", "https://127.0.0.1:9101",
	} {
		client, err := newServiceClient(origin, []byte("local-service-key"))
		if err == nil {
			client.close()
			t.Errorf("invalid origin %q was accepted", origin)
		}
	}
}

func TestInternalServiceRequestTarget(t *testing.T) {
	var arrived int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived++
		if r.RequestURI != "/internal/merchants/MER-001" || r.Method != http.MethodGet || r.Header.Get("X-Service-Key") != "local-service-key" {
			t.Errorf("internal request: %s %s, credential matches %v", r.Method, r.RequestURI, r.Header.Get("X-Service-Key") == "local-service-key")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := newServiceClient(server.URL, []byte("local-service-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	code, _, err := client.call(t.Context(), http.MethodGet, "/internal/merchants/MER-001", nil, "")
	if err != nil || code != http.StatusNoContent || arrived != 1 {
		t.Fatalf("internal call: status %d, arrived %d, error %v", code, arrived, err)
	}
}
