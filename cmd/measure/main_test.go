package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMeasurementArgumentRefusals(t *testing.T) {
	for _, args := range [][]string{{"-samples", "0"}, {"-samples", "51"}, {"-account", "ops-01"}, {"-account", "unknown"}, {"unexpected"}} {
		var output bytes.Buffer
		if run(args, &output) == nil || output.Len() != 0 {
			t.Errorf("invalid measurement emitted a result: %v", args)
		}
	}
}

func TestPairedMeasurementChecksIdentityAndAnswers(t *testing.T) {
	directCount, gatewayCount := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/direct/orders/wallet" {
			directCount++
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Gateway-Subject") != "cust-001" || r.Header.Get("X-Gateway-Roles") != "CUSTOMER" {
				t.Error("direct baseline identity incorrect")
			}
		} else {
			gatewayCount++
			if r.Header.Get("Authorization") != "Bearer synthetic-test-token" || r.Header.Get("X-Gateway-Subject") != "" {
				t.Error("gateway measurement forged identity")
			}
			w.Header().Set("X-Request-Id", "synthetic-reference")
		}
		w.Header().Set("X-Service", "orders")
		w.Write([]byte(`{"balance_sen":10000}`))
	}))
	defer server.Close()
	result, err := measure(server.Client(), server.URL+"/direct", server.URL+"/gateway", "cust-001", "synthetic-test-token", 3)
	if err != nil || result.Samples != 3 || directCount != 6 || gatewayCount != 6 {
		t.Fatalf("paired counts direct=%d gateway=%d: %v", directCount, gatewayCount, err)
	}
	if len(result.DirectSamplesMS) != 3 || len(result.GatewaySamplesMS) != 3 {
		t.Fatal("raw observations missing from the reproducible result")
	}
	if result.Direct.MeanMS <= 0 || result.Gateway.P95MS <= 0 {
		t.Fatal("measurement did not time complete responses")
	}
}

func TestMeasurementRefusesIncompleteOrNonEquivalentAnswers(t *testing.T) {
	for _, body := range []string{`{"balance_sen":9000}`, `{}`, `{"balance_sen":10000,"unexpected":1}`, strings.Repeat("x", 4097)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Service", "orders")
			if strings.HasPrefix(r.URL.Path, "/direct") {
				w.Write([]byte(`{"balance_sen":10000}`))
				return
			}
			w.Header().Set("X-Request-Id", "synthetic-reference")
			w.Write([]byte(body))
		}))
		_, err := measure(server.Client(), server.URL+"/direct", server.URL+"/gateway", "cust-001", "synthetic-test-token", 1)
		server.Close()
		if err == nil {
			t.Error("measurement reported non-equivalent answers")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer server.Close()
	if _, err := measure(server.Client(), server.URL, server.URL, "cust-001", "synthetic-test-token", 1); err == nil {
		t.Fatal("rate refusal emitted a latency result")
	}
}

func TestNearestRankLatencySummary(t *testing.T) {
	s := summarize([]float64{5, 1, 4, 2, 3})
	if s.MeanMS != 3 || s.P50MS != 3 || s.P95MS != 5 {
		t.Fatalf("nearest-rank summary: %+v", s)
	}
}
