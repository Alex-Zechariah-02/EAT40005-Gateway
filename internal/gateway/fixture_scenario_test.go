// Catalogue scenarios through the gateway.

package gateway

import (
	"bytes"
	"encoding/json"
	"maps"
	"testing"

	fixture "github.com/Alex-Zechariah-02/apiward/internal/services/testfixture"
)

// stepResult is what one catalogue step produced, as seen by the client, the
// fixture and the audit log.
type stepResult struct {
	Status    int
	Operation string // operation the fixture ran
	Effect    string
}

// runScenarios replays the catalogue sequence through the gateway, each step
// with a genuine token for its subject, and checks every step against the
// catalogue's expected fixture outcome, the fixture's own receipt and the
// gateway's event for the same request.
func runScenarios(t *testing.T, e *env, fixtures map[string]*fixtureProcess, c fixture.Catalogue) []stepResult {
	t.Helper()
	apiOf := map[string]string{}
	for _, op := range c.Operations {
		apiOf[op.ID] = op.API
	}
	roles := catalogueRoles(c)
	generation := e.status().Generation

	var results []stepResult
	for _, sc := range c.Scenarios {
		target := fixtures[apiOf[sc.Operation]]
		before := receiptCounts(fixtures)
		stateBefore := target.state().SHA256

		contentType := ""
		if len(sc.Body) > 0 {
			contentType = "application/json"
		}
		var header []string
		if sc.Subject != "" {
			header = bearer(t, sc.Subject)
		}
		resp, body := e.exchange(sc.Method, sc.Path, contentType, string(sc.Body), header...)

		after := receiptCounts(fixtures)
		if sc.GatewayStage != "" {
			checkGatewayRefusal(t, e, sc, resp.StatusCode, body, before, after, target.state().SHA256 != stateBefore)
			results = append(results, stepResult{Status: resp.StatusCode})
			continue
		}
		for name := range after {
			want := before[name]
			if name == target.name {
				want++
			}
			if after[name] != want {
				t.Fatalf("%s: receipts %v -> %v, want exactly one new receipt at %s", sc.Name, before, after, target.name)
			}
		}
		receipt := target.receipts()[after[target.name]-1]
		changed := target.state().SHA256 != stateBefore

		switch {
		case resp.StatusCode != sc.Status:
			t.Errorf("%s: status %d, want %d (%s)", sc.Name, resp.StatusCode, sc.Status, body)
		case resp.Header.Get("X-Service") != target.name:
			t.Errorf("%s: answered by %q, want %s", sc.Name, resp.Header.Get("X-Service"), target.name)
		case receipt.Operation != sc.Operation:
			t.Errorf("%s: fixture ran %q, want %q", sc.Name, receipt.Operation, sc.Operation)
		case receipt.Generation != generation || receipt.RequestID == "":
			t.Errorf("%s: receipt generation %q request %q, want generation %s", sc.Name, receipt.Generation, receipt.RequestID, generation)
		case receipt.Authorization:
			t.Errorf("%s: a credential reached the fixture", sc.Name)
		case receipt.Subject != sc.Subject || receipt.Roles != roles[sc.Subject]:
			t.Errorf("%s: fixture saw subject %q roles %q, want %q %q", sc.Name, receipt.Subject, receipt.Roles, sc.Subject, roles[sc.Subject])
		case changed != sc.ChangesState || receipt.Effect != sc.Effect:
			t.Errorf("%s: changed %v effect %q, want %v %q", sc.Name, changed, receipt.Effect, sc.ChangesState, sc.Effect)
		case len(sc.Response) > 0 && !sameJSON(body, sc.Response):
			t.Errorf("%s: response %s, want %s", sc.Name, body, sc.Response)
		}

		// The gateway must have selected the same operation the fixture ran.
		events := e.eventsWhen(func(events []map[string]any) bool {
			return findEvent(events, "request.forwarded", receipt.RequestID) != nil
		})
		forwarded := findEvent(events, "request.forwarded", receipt.RequestID)
		subject, _ := forwarded["subject"].(string)
		if forwarded == nil || forwarded["operation"] != receipt.Operation || forwarded["api"] != apiOf[sc.Operation] ||
			forwarded["generation"] != generation || subject != receipt.Subject {
			t.Errorf("%s: gateway event %v does not match fixture receipt %+v", sc.Name, forwarded, receipt)
		}
		results = append(results, stepResult{resp.StatusCode, receipt.Operation, receipt.Effect})
	}
	return results
}

// checkGatewayRefusal checks a step the gateway must refuse: the expected
// status, no receipt at any fixture, no state change, and a rejection event
// at the expected stage for the request the client was told about.
func checkGatewayRefusal(t *testing.T, e *env, sc fixture.Scenario, status int, body string, before, after map[string]int, changed bool) {
	t.Helper()
	if status != sc.GatewayStatus {
		t.Errorf("%s: status %d, want %d from the gateway (%s)", sc.Name, status, sc.GatewayStatus, body)
	}
	if !maps.Equal(before, after) || changed {
		t.Errorf("%s: receipts %v -> %v, state changed %v; the gateway must refuse it first", sc.Name, before, after, changed)
	}
	var reply struct {
		RequestID string `json:"request_id"`
	}
	json.Unmarshal([]byte(body), &reply)
	events := e.eventsWhen(func(events []map[string]any) bool {
		return findEvent(events, "request.rejected", reply.RequestID) != nil
	})
	rejected := findEvent(events, "request.rejected", reply.RequestID)
	if rejected == nil || rejected["stage"] != sc.GatewayStage || rejected["subject"] != sc.Subject {
		t.Errorf("%s: rejection event %v, want stage %s for subject %s", sc.Name, rejected, sc.GatewayStage, sc.Subject)
	}
}

func findEvent(events []map[string]any, kind, requestID string) map[string]any {
	for _, ev := range events {
		if ev["kind"] == kind && ev["request_id"] == requestID {
			return ev
		}
	}
	return nil
}

func sameJSON(got string, want []byte) bool {
	var a, b any
	if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal(want, &b) != nil {
		return false
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// Every catalogue operation runs
// through the real gateway and proxy on the intended fixture, with the
// expected business outcome and state change.
func TestCatalogueThroughGateway(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	c := loadCatalogue(t)

	instances := map[string]bool{}
	for _, f := range fixtures {
		instances[f.state().Instance] = true
	}
	if len(instances) != 3 {
		t.Fatalf("fixture instances %v, want three distinct processes", instances)
	}

	results := runScenarios(t, e, fixtures, c)
	if len(results) != len(c.Scenarios) {
		t.Fatalf("%d of %d steps ran", len(results), len(c.Scenarios))
	}
	ran := map[string]bool{}
	for _, r := range results {
		ran[r.Operation] = true
	}
	for _, op := range c.Operations {
		if !ran[op.ID] {
			t.Errorf("catalogue operation %s was never exercised", op.ID)
		}
	}
	e.assertReceipts(0, 0, 0)
}
