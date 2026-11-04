// Audit decision classes and unrecorded bodies.

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// auditRecords reads every record of the rotated audit files, oldest first,
// then of the active file, failing on any line that is not one JSON record.
func (e *env) auditRecords() []map[string]any {
	e.t.Helper()
	files, _ := filepath.Glob(e.auditLog + ".*")
	// Numeric order of the rotation suffix: shorter names first.
	slices.SortFunc(files, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	files = append(files, e.auditLog)
	var records []map[string]any
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			e.t.Fatal(err)
		}
		for line := range bytes.SplitSeq(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				e.t.Fatalf("%s holds a line that is not one record: %q", file, line)
			}
			records = append(records, record)
		}
	}
	return records
}

// Every decision class produces its own event with trusted attribution:
// a server timestamp, the captured generation, the operation, the verified
// subject only after verification, and the stages that actually ran.
// Publication events carry the transition and revision, and no event kind
// of an unimplemented component appears.
func TestEventsCoverEveryDecisionClass(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	e.start(binaries.fault) // publication below uses the test publication hook
	started := time.Now().UTC()

	tight := withRate(fixturePolicy(fixtures), rate(1, 60))
	published, code := e.publish(Change{ID: "tight", Expected: 1, Policy: mustJSON(tight)})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, published)
	}
	if rejected, code := e.publish(Change{ID: "broken", Expected: published.Revision, Policy: mustJSON(withRate(tight, rate(0, 60)))}); code != http.StatusBadRequest || rejected.Reason != "INVALID_VALUE" {
		t.Fatalf("invalid publish: %d %+v", code, rejected)
	}
	g := e.status().Generation

	staff := bearer(t, "staff-01")
	cases := []struct {
		name, method, path, body string
		header                   []string
		status                   int
		stage, reason, subject   string
		evaluated                []string // stages expected PASS before the terminal one
	}{
		{"authentication", "GET", "/orders", "", nil, 401, "JWT", "missing_credential", "", nil},
		{"authorisation", "GET", "/reports/sales", "", staff, 403, "RBAC", "no_permit", "staff-01", []string{"JWT"}},
		{"admitted under quota", "GET", "/orders", "", staff, 200, "", "", "staff-01", nil},
		{"rate", "GET", "/orders", "", staff, 429, "RATE", "quota_exceeded", "staff-01", []string{"JWT", "RBAC"}},
		{"validation", "PATCH", "/inventory/items/ITM-001", `{"delta":"x"}`, staff, 400, "VALIDATION", "schema_mismatch", "staff-01", []string{"JWT", "RBAC", "RATE", "BODY"}},
		{"routing", "GET", "/billing", "", staff, 404, "ROUTE", "", "", nil},
	}
	for _, tc := range cases {
		contentType := ""
		if tc.body != "" {
			contentType = "application/json"
		}
		resp, body := e.exchange(tc.method, tc.path, contentType, tc.body, tc.header...)
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: %d %s", tc.name, resp.StatusCode, body)
		}
		if tc.status == 200 {
			continue
		}
		_, id := requestID(resp, body)
		events := e.requestEvents(id, 1)
		if len(events) != 1 {
			t.Fatalf("%s: %d events", tc.name, len(events))
		}
		ev := events[0]
		subject, _ := ev["subject"].(string)
		reason, _ := ev["reason"].(string)
		if ev["kind"] != "request.rejected" || ev["stage"] != tc.stage || reason != tc.reason || subject != tc.subject {
			t.Errorf("%s: event %v", tc.name, ev)
		}
		if tc.stage != "ROUTE" && ev["generation"] != g {
			t.Errorf("%s: generation %v, want %s", tc.name, ev["generation"], g)
		}
		stamp, err := time.Parse(time.RFC3339Nano, ev["time"].(string))
		if err != nil || !strings.HasSuffix(ev["time"].(string), "Z") || stamp.Before(started.Add(-time.Second)) || stamp.After(time.Now().Add(time.Second)) {
			t.Errorf("%s: time %v is not a current server UTC time", tc.name, ev["time"])
		}
		// Stages after the terminal one were not evaluated.
		stages := stagesOf(ev)
		after := false
		for _, name := range stageNames {
			if name == tc.stage {
				after = true
				continue
			}
			if after && stages[name] != stageNotRun {
				t.Errorf("%s: stage %s after the terminal stage reported %v", tc.name, name, stages[name])
			}
		}
		for _, name := range tc.evaluated {
			if stages[name] != stagePass {
				t.Errorf("%s: stage %s %v, want PASS", tc.name, name, stages[name])
			}
		}
	}

	events := e.auditEvents()
	kinds := map[string]bool{}
	var ids []string
	for _, ev := range events {
		kinds[ev["kind"].(string)] = true
		ids = append(ids, ev["event_id"].(string))
	}
	if len(slices.Compact(slices.Sorted(slices.Values(ids)))) != len(ids) {
		t.Fatal("event identities repeat")
	}
	for kind := range kinds {
		known := slices.Contains([]string{"request.admitted", "request.forwarded", "request.rejected", "controller.reconciled",
			"transition.intent", "transition.published", "transition.rejected", "account.block", "admin.authenticated", "admin.read"}, kind)
		if !known {
			t.Errorf("unexpected event kind %s", kind)
		}
	}
	intent := findTransition(events, "transition.intent", "tight")
	done := findTransition(events, "transition.published", "tight")
	failed := findTransition(events, "transition.rejected", "broken")
	if intent == nil || done == nil || done["revision"] != float64(published.Revision) || done["generation"] != published.Generation ||
		done["actor"] != harnessAdmin || failed == nil || failed["reason"] != "INVALID_VALUE" {
		t.Fatalf("publication events: intent %v published %v rejected %v", intent, done, failed)
	}
}

func withRate(p Policy, r *RateRule) Policy {
	p.Operations = slices.Clone(p.Operations)
	for i := range p.Operations {
		if p.Operations[i].ID == "orders.list" {
			p.Operations[i].Rate = r
		}
	}
	return p
}

func findTransition(events []map[string]any, kind, id string) map[string]any {
	for _, ev := range events {
		if ev["kind"] == kind && ev["transition"] == id {
			return ev
		}
	}
	return nil
}

// Request bodies, header values and validator messages never reach the
// audit log, whether the request was refused or forwarded.
func TestBodiesAndHeaderValuesNeverRecorded(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.keywordPolicy(), seedOptions{})
	e.start(binaries.plain)

	secrets := []string{"BODY-SECRET-1", "BODY-SECRET-2", "HEADER-SECRET-3", "hunter2"}
	key := "Idempotency-Key: HEADER-SECRET-3\r\n"
	item := func(headers, body string) string {
		return fmt.Sprintf("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\nX-Api-Key: hunter2\r\n%sContent-Length: %d\r\n\r\n%s", headers, len(body), body)
	}
	for _, request := range []string{
		item(key, `{"kind":"alpha","count":1,"note":"BODY-SE"}`),               // forwarded
		item(key, `{"kind":"alpha","count":1,"password":"BODY-SECRET-1"}`),     // unknown field
		item(key, `{"kind":"BODY-SECRET-2","count":1}`),                        // enumeration
		item(key, `{"kind":"alpha","count":1,"note":"BODY-SECRET-1\n{"`),       // malformed
		item("Idempotency-Key: short\r\n", `{"kind":"alpha","count":1}`),       // header schema
		item(key, `{"kind":"alpha","count":1,"meta":{"tag":"BODY-SECRET-2"}}`), // nested
	} {
		e.rawExchange(request, false)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.rejected") == 5 })
	if countKind(events, "request.rejected") != 5 || countKind(events, "request.forwarded") != 1 {
		t.Fatalf("events %v", events)
	}
	raw, _ := os.ReadFile(e.auditLog)
	for _, secret := range append(secrets, "BODY-SE", "password", "jsonschema", "minLength", "enum") {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("audit log contains %q", secret)
		}
	}
	e.assertReceipts(1, 0, 0)
}
