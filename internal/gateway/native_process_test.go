package gateway

import (
	"bytes"
	"net/http"
	"os"
	"testing"
)

// Observe an actual committed fixture mutation independently of a lost
// acknowledgement, and prove that neither build silently replays it.
func TestNativeFixtureWriteThenDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		binary string
		status int
	}{
		{"normal excludes fault", binaries.fixture, http.StatusCreated},
		{"fault build loses acknowledgement", binaries.fixtureFault, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary, err := os.ReadFile(tc.binary)
			if err != nil {
				t.Fatal(err)
			}
			if included := bytes.Contains(binary, []byte("FIXTURE_FAULT")); included != (tc.binary == binaries.fixtureFault) {
				t.Fatalf("fixture fault control included = %v", included)
			}
			fixtures := startFixtureSet(t, tc.binary, "FIXTURE_FAULT=drop-after-write")
			e := newEnv(t)
			seed(t, e.database, fixturePolicy(fixtures), seedOptions{})
			e.start(binaries.plain)
			before := fixtures["orders"].state()
			code, body := e.request(http.MethodPost, "/orders", "application/json", `{"customer":"cust-001","item":"ITM-001","quantity":1}`, bearer(t, "cust-001")...)
			if code != tc.status {
				t.Fatalf("write: %d %s, want %d", code, body, tc.status)
			}
			receipts := fixtures["orders"].receipts()
			if len(receipts) != 1 || receipts[0].Status != http.StatusCreated || receipts[0].Effect == "" || before.SHA256 == fixtures["orders"].state().SHA256 {
				t.Fatalf("expected exactly one real mutation: %+v", receipts)
			}
			if len(fixtures["inventory"].receipts()) != 0 || len(fixtures["reports"].receipts()) != 0 {
				t.Fatal("write reached another fixture")
			}
			if tc.status == http.StatusBadGateway {
				events := e.requestEvents(receipts[0].RequestID, 2)
				outcome := findEvent(events, "request.forwarded", receipts[0].RequestID)
				if outcome == nil || outcome["decision"] != "upstream_failure" || outcome["reason"] != "outcome_unknown" {
					t.Fatalf("lost acknowledgement was not reported honestly: %v", events)
				}
			}
			t.Logf("client status %d; exactly one orders receipt, status 201, effect %s; independent state changed; other fixtures untouched", code, receipts[0].Effect)
		})
	}
}
