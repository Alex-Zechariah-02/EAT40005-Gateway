package gateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFetchMetrics(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	e.get("/a/customer-order-identifier", bearer(t, "alice")...)
	e.get("/a/no-token")
	e.get("/unknown-item-identifier")
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	if code, body, _ := e.as(admin, "PUT", "/admin/blocked-accounts/secret-subject", `{"reason":"test"}`, "Idempotency-Key", "metrics-block-test"); code != 200 {
		t.Fatalf("block: %d %s", code, body)
	}
	response, err := http.Get("http://" + e.metrics + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics: %d %v", response.StatusCode, response.Header)
	}
	for _, text := range []string{`gateway_requests_total{api="a",operation="read-a",decision="allowed",code=""} 1`, `gateway_request_duration_seconds_count{api="a"} 2`, "gateway_policy_revision 1", "gateway_temporary_rules_active 0", "gateway_blocked_accounts 1", "gateway_event_stream_dropped_total 0"} {
		if !strings.Contains(body, text) {
			t.Errorf("missing %s in %s", text, body)
		}
	}
	for _, secret := range []string{"alice", "secret-subject", "customer-order-identifier", "unknown-item-identifier", "127.0.0.1"} {
		if strings.Contains(body, secret) {
			t.Errorf("metric leaks %q", secret)
		}
	}
	for _, path := range []string{"/admin/status", "/", "/metrics/"} {
		response, err := http.Get("http://" + e.metrics + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Errorf("metrics %s: %d", path, response.StatusCode)
		}
	}
	if code, _ := e.get("/metrics"); code != 404 {
		t.Errorf("metrics exposed on data: %d", code)
	}
}

func TestFetchBoundedEventWindow(t *testing.T) {
	o := newObservations()
	viewer := o.subscribe()
	for i := range recentEventLimit + 7 {
		o.record(Event{RequestID: fmt.Sprint(i), Decision: "reject", Stage: "ROUTE"})
	}
	events := o.snapshot()
	if len(events) != 10000 || events[0].RequestID != "10006" || events[9999].RequestID != "7" {
		t.Fatalf("window bounds: %d %s %s", len(events), events[0].RequestID, events[len(events)-1].RequestID)
	}
	if dropped := o.takeDropped(viewer); dropped != recentEventLimit+7-streamQueueSize {
		t.Errorf("dropped %d", dropped)
	}
	if o.takeDropped(viewer) != 0 {
		t.Fatal("drop count was not consumed")
	}
	o.unsubscribe(viewer)
	o.close()
	if o.subscribe() != nil {
		t.Fatal("subscribed after shutdown")
	}
}
