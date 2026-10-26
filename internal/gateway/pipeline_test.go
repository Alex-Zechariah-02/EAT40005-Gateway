package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// allStages is the stage map of a request that passed every control and was
// forwarded.
func allStages() map[string]any {
	stages := map[string]any{}
	for _, name := range stageNames {
		stages[name] = stagePass
	}
	return stages
}

// A valid request passes all five controls on one request path, under one
// generation, and the intended fixture performs the operation: the event
// chain, the receipt and the state change agree.
func TestFiveControlsAdmitValidRequest(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	g := e.status().Generation
	orders, inventory := fixtures["orders"], fixtures["inventory"]

	requests := []struct {
		subject, method, path, body, fixture, effect string
		status                                       int
	}{
		{"cust-002", "POST", "/orders", `{"customer":"cust-002","item":"ITM-003","quantity":4}`, "orders", "created ORD-1004", 201},
		{"staff-01", "PATCH", "/inventory/items/ITM-002", `{"delta":5}`, "inventory", "ITM-002 stock 15->20", 200},
	}
	for _, r := range requests {
		target := fixtures[r.fixture]
		before := target.state().SHA256
		resp, body := e.exchange(r.method, r.path, "application/json", r.body, bearer(t, r.subject)...)
		if resp.StatusCode != r.status || resp.Header.Get("X-Fixture-Service") != r.fixture {
			t.Fatalf("%s %s: %d from %q (%s)", r.method, r.path, resp.StatusCode, resp.Header.Get("X-Fixture-Service"), body)
		}
		receipts := target.receipts()
		receipt := receipts[len(receipts)-1]
		if receipt.Effect != r.effect || target.state().SHA256 == before || receipt.Generation != g ||
			receipt.Subject != r.subject || receipt.Authorization || receipt.BodySHA256 != sha256Hex(r.body) {
			t.Fatalf("%s: receipt %+v", r.path, receipt)
		}
		events := e.requestEvents(receipt.RequestID, 2)
		admitted, forwarded := findEvent(events, "request.admitted", receipt.RequestID), findEvent(events, "request.forwarded", receipt.RequestID)
		wantAdmitted := allStages()
		wantAdmitted["AUDIT"], wantAdmitted["BIND"], wantAdmitted["FORWARD"] = stageNotRun, stageNotRun, stageNotRun
		if admitted == nil || forwarded == nil || !sameStages(admitted["stages"], wantAdmitted) || !sameStages(forwarded["stages"], allStages()) {
			t.Fatalf("%s: events %v", r.path, events)
		}
		for _, ev := range events {
			if ev["generation"] != g || ev["subject"] != r.subject || ev["operation"] != receipt.Operation {
				t.Fatalf("%s: event %v disagrees with the receipt %+v", r.path, ev, receipt)
			}
		}
	}
	if n := len(fixtures["reports"].receipts()); n != 0 || len(orders.receipts()) != 1 || len(inventory.receipts()) != 1 {
		t.Fatalf("receipts: orders %d inventory %d reports %d", len(orders.receipts()), len(inventory.receipts()), n)
	}
}

// fixtureWithRate is the fixture policy with another rate rule on
// orders.create and, when schema is set, another body schema.
func fixtureWithRate(fixtures map[string]*fixtureProcess, r *RateRule, schema string) Policy {
	p := fixturePolicy(fixtures)
	for i := range p.Operations {
		if p.Operations[i].ID == "orders.create" {
			p.Operations[i].Rate = r
			if schema != "" {
				p.Operations[i].Body = jsonBody(1024, schema)
			}
		}
	}
	return p
}

// An early refusal stops the path: a JWT or RBAC refusal consumes no quota
// and evaluates no body, and a rate refusal of an otherwise valid request
// evaluates no body; none reaches a fixture.
func TestEarlyRefusalsStopThePath(t *testing.T) {
	fixtures := startFixtures(t)
	e := newEnv(t)
	seed(t, e.database, fixtureWithRate(fixtures, rate(2, 60), ""), seedOptions{})
	clock := newControlledClock(t, base)
	e.start(binaries.fault, clock.env())

	valid := `{"customer":"cust-001","item":"ITM-001","quantity":1}`
	routed := []string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS"}
	cases := []struct {
		name, body string
		header     []string
		status     int
		want       map[string]any
	}{
		{"forged token", valid, []string{"Authorization", "Bearer " + newTestIssuer(2048).tokenFor(t, "cust-001")}, 401, expectedStages(routed, "JWT", stageReject)},
		{"wrong role", valid, bearer(t, "staff-01"), 403, expectedStages(append(slices.Clone(routed), "JWT"), "RBAC", stageReject)},
		{"wrong role with an invalid body", `{"x":`, bearer(t, "mgr-01"), 403, expectedStages(append(slices.Clone(routed), "JWT"), "RBAC", stageReject)},
		{"first admitted", valid, bearer(t, "cust-001"), 201, nil},
		{"second admitted", valid, bearer(t, "cust-001"), 201, nil},
		{"first excess with a valid body", valid, bearer(t, "cust-001"), 429, expectedStages(append(slices.Clone(routed), "JWT", "RBAC"), "RATE", stageReject)},
		{"excess with an invalid body", `{"x":`, bearer(t, "cust-001"), 429, expectedStages(append(slices.Clone(routed), "JWT", "RBAC"), "RATE", stageReject)},
	}
	for _, tc := range cases {
		resp, body := e.exchange(http.MethodPost, "/orders", "application/json", tc.body, tc.header...)
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: %d %s", tc.name, resp.StatusCode, body)
		}
		if tc.want == nil {
			continue
		}
		_, id := requestID(resp, body)
		events := e.requestEvents(id, 1)
		if len(events) != 1 || !sameStages(events[0]["stages"], tc.want) {
			t.Fatalf("%s: events %v, want stages %v", tc.name, events, tc.want)
		}
	}
	if counts := receiptCounts(fixtures); counts["orders"] != 2 || counts["inventory"] != 0 || counts["reports"] != 0 {
		t.Fatalf("receipts %v, want the two admitted orders only", counts)
	}
}

// A request refused by validation while another generation is published is
// decided entirely by the generation it captured: its schema refuses it, its
// rate rule counted it, and the event names that generation. Nothing is
// forwarded.
func TestValidationRefusalDuringPublication(t *testing.T) {
	fixtures := startFixtures(t)
	e := newEnv(t)
	strict := `{"type":"object","additionalProperties":false,"required":["customer","item","quantity"],"properties":{` +
		`"customer":{"type":"string"},"item":{"type":"string"},"quantity":{"type":"integer","maximum":1}}}`
	seed(t, e.database, fixtureWithRate(fixtures, rate(2, 60), strict), seedOptions{})
	clock := newControlledClock(t, base)
	dir := t.TempDir()
	holdFile, release := filepath.Join(dir, "captured"), filepath.Join(dir, "release")
	e.start(binaries.fault, clock.env(), "GATEWAY_HOLD_PATH=/orders", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	first := e.status().Generation

	// Under the first generation a quantity of 2 is refused; under the
	// second, the fixture schema, it is allowed. The held request carries it.
	extra := `{"customer":"cust-001","item":"ITM-001","quantity":2}`
	type reply struct {
		status int
		body   string
	}
	result := make(chan reply, 1)
	go func() {
		status, body := e.request(http.MethodPost, "/orders", "application/json", extra, bearer(t, "cust-001")...)
		result <- reply{status, body}
	}()
	waitForFile(t, holdFile)
	outcome, code := e.publish(Change{ID: "fixture-schema", Expected: 1, Policy: mustJSON(fixtureWithRate(fixtures, rate(2, 60), ordersCreateSchema))})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}
	os.WriteFile(release, nil, 0o600)
	held := <-result
	if held.status != http.StatusBadRequest {
		t.Fatalf("held request: %d %s", held.status, held.body)
	}
	_, id := requestID(&http.Response{}, held.body)
	ev := e.requestEvents(id, 1)
	if len(ev) != 1 || ev[0]["stage"] != "VALIDATION" || ev[0]["generation"] != first || stagesOf(ev[0])["RATE"] != stagePass {
		t.Fatalf("held request event %v, want a VALIDATION refusal under %s after RATE passed", ev, first)
	}
	// The refused request counted: one more admission fits, then 429.
	statuses := e.sendAs("cust-001", http.MethodPost, "/orders", extra, 2)
	if !statusesAre(statuses, 201, 429) {
		t.Fatalf("after the refused request: %v, want 201 429", statuses)
	}
	receipts := fixtures["orders"].receipts()
	if len(receipts) != 1 || receipts[0].Generation != outcome.Generation {
		t.Fatalf("receipts %+v, want one under %s", receipts, outcome.Generation)
	}
}

// Client-supplied stage, policy, rate and audit headers never skip a control.
// The refused invalid body still counts against the quota of 2, since it was
// admitted by the rate stage.
func TestForgedControlHeadersSkipNothing(t *testing.T) {
	fixtures := startFixtures(t)
	e := newEnv(t)
	seed(t, e.database, fixtureWithRate(fixtures, rate(2, 60), ""), seedOptions{})
	e.start(binaries.plain)

	forged := []string{
		"X-Gateway-Generation", "r1-forged", "X-Gateway-Validated", "true", "X-Gateway-Stage", "FORWARD",
		"X-Gateway-Rate", "exempt", "X-Gateway-Audit", "durable", "X-Request-Id", "forged", "X-Gateway-Subject", "staff-01",
		"X-Gateway-Roles", "STAFF,MANAGER", "X-Forwarded-For", "127.0.0.1",
	}
	invalid := `{"customer":"cust-001","item":"ITM-001","quantity":"many"}`
	if code, _ := e.request(http.MethodPost, "/orders", "application/json", invalid, append(bearer(t, "cust-001"), forged...)...); code != http.StatusBadRequest {
		t.Fatalf("invalid body with forged headers: %d", code)
	}
	valid := `{"customer":"cust-001","item":"ITM-001","quantity":1}`
	if got := e.sendAs("cust-001", http.MethodPost, "/orders", valid, 2, forged...); !statusesAre(got, 201, 429) {
		t.Fatalf("quota with forged headers: %v", got)
	}
	if code, _ := e.request(http.MethodGet, "/reports/sales", "", "", append(bearer(t, "cust-001"), forged...)...); code != http.StatusForbidden {
		t.Fatalf("role escalation by header: %d", code)
	}
	if code, _ := e.request(http.MethodGet, "/orders", "", "", forged...); code != http.StatusUnauthorized {
		t.Fatalf("no token with forged headers: %d", code)
	}
	receipts := fixtures["orders"].receipts()
	if len(receipts) != 1 || receipts[0].Subject != "cust-001" || receipts[0].Roles != "CUSTOMER" || receipts[0].RequestID == "forged" ||
		receipts[0].Generation != e.status().Generation {
		t.Fatalf("receipts %+v", receipts)
	}
}

// With every control in place, an unavailable upstream answers a safe error
// for concurrent valid requests, reaches no other destination, and records
// each outcome; rate and validation still apply.
func TestUpstreamFailureUnderFiveControls(t *testing.T) {
	e := newEnv(t)
	p := e.ratePolicy(rate(30, 60), nil)
	p.Upstreams = append(p.Upstreams, Upstream{ID: "Z", Dial: freeAddr(t)})
	p.APIs = append(p.APIs, API{ID: "z", Upstream: "Z"})
	p.Operations = append(p.Operations, Operation{ID: "write-z", API: "z", Method: "POST", Target: "/z", Access: "allow", Auth: "required",
		Body: jsonBody(1024, `{"type":"object","additionalProperties":false,"required":["n"],"properties":{"n":{"type":"integer"}}}`), Rate: rate(20, 60)})
	p.Permits = append(p.Permits, Permit{Role: "WRITER", Operation: "write-z"})
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)

	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}
	token := issuer.tokenFor(t, "alice")
	for i := range 25 {
		wg.Go(func() {
			body := `{"n":1}`
			if i%5 == 0 {
				body = `{"n":"x"}`
			}
			req, _ := http.NewRequest(http.MethodPost, "http://"+e.dataAddr+"/z", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			mu.Lock()
			statuses[resp.StatusCode]++
			mu.Unlock()
		})
	}
	wg.Wait()
	// 25 requests, quota 20: 20 pass RATE (some with invalid bodies), 5 are
	// refused 429. Of those 20, the invalid ones are refused 400 and the
	// valid ones meet the closed port: 502.
	if statuses[http.StatusTooManyRequests] != 5 || statuses[http.StatusBadRequest]+statuses[http.StatusBadGateway] != 20 || statuses[http.StatusOK] != 0 {
		t.Fatalf("statuses %v", statuses)
	}
	e.assertReceipts(0, 0, 0)
	events := e.eventsWhen(func(events []map[string]any) bool {
		return countKind(events, "request.forwarded") == statuses[http.StatusBadGateway]
	})
	failed := 0
	for _, ev := range events {
		if ev["kind"] == "request.forwarded" {
			if ev["decision"] != "upstream_failure" || ev["error"] != "upstream_unavailable" || ev["upstream"] != "Z" || ev["subject"] != "alice" {
				t.Fatalf("outcome %v", ev)
			}
			failed++
		}
	}
	if failed != statuses[http.StatusBadGateway] || failed == 0 {
		t.Fatalf("%d upstream failure events for %d 502 replies", failed, statuses[http.StatusBadGateway])
	}
	t.Logf("statuses %v", statuses)
}
