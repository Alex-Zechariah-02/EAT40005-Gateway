// Override helpers and request grammar.

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// These tests cover temporary overrides: the request grammar and the
// resolver on their own, then creation, inspection, revocation, expiry and
// rate counters through the real gateway, where every enforcement claim is
// checked at the receivers.

func permitOverride(kind, role, operation string, seconds int64) OverrideRequest {
	return OverrideRequest{Type: kind, Control: store.ControlPermit, Operation: operation, Role: new(role), ExpiresInSeconds: seconds}
}

func rateOverride(kind, operation string, count, seconds int64) OverrideRequest {
	return OverrideRequest{Type: kind, Control: store.ControlRate, Operation: operation, Count: new(count), ExpiresInSeconds: seconds}
}

func operationContainment(operation string, seconds int64) OverrideRequest {
	return OverrideRequest{Type: store.OverrideContainment, Control: store.ControlOperation, Operation: operation, ExpiresInSeconds: seconds}
}

// createOverride asks for a temporary override; request may be an
// OverrideRequest or raw JSON.
func (e *env) createOverride(s adminSession, request any, ifMatch, key string) (int, Outcome, string) {
	e.t.Helper()
	body, ok := request.(string)
	if !ok {
		body = string(mustJSON(request))
	}
	code, data, _ := e.as(s, http.MethodPost, "/admin/overrides", body, "If-Match", ifMatch, "Idempotency-Key", key)
	var outcome Outcome
	json.Unmarshal([]byte(data), &outcome)
	return code, outcome, data
}

// mustOverride creates an override, failing the test unless it is created.
func (e *env) mustOverride(s adminSession, request OverrideRequest, ifMatch, key string) Outcome {
	e.t.Helper()
	code, outcome, body := e.createOverride(s, request, ifMatch, key)
	if code != http.StatusCreated || outcome.Outcome != "committed" || outcome.Override == 0 {
		e.t.Fatalf("creating override %+v: %d %s", request, code, body)
	}
	return outcome
}

func (e *env) revokeOverride(s adminSession, id int64, key string) (int, Outcome, string) {
	e.t.Helper()
	code, data, _ := e.as(s, http.MethodDelete, fmt.Sprintf("/admin/overrides/%d", id), "", "Idempotency-Key", key)
	var outcome Outcome
	json.Unmarshal([]byte(data), &outcome)
	return code, outcome, data
}

type overridesView struct {
	Revision            int64            `json:"revision"`
	Confirmed           int64            `json:"confirmed"`
	Provisional         int64            `json:"provisional"`
	Overrides           []store.Override `json:"overrides"`
	Generation          string           `json:"generation"`
	GenerationOverrides []int64          `json:"generation_overrides"`
}

func (e *env) activeOverrides(s adminSession) overridesView {
	e.t.Helper()
	code, body, _ := e.as(s, http.MethodGet, "/admin/overrides", "")
	if code != http.StatusOK {
		e.t.Fatalf("GET /admin/overrides: %d %s", code, body)
	}
	return fromJSON[overridesView](e.t, body)
}

// storedOverrides reads every override of a stopped gateway.
func storedOverrides(t *testing.T, path string) []store.Override {
	t.Helper()
	db := inspect(t, path)
	defer db.Close()
	s := store.Adopt(db)
	list, err := s.Overrides(t.Context(), `ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// probe sends one request as subject (or anonymously when subject is empty)
// and returns its status.
func (e *env) probe(subject, method, path string) int {
	e.t.Helper()
	var header []string
	if subject != "" {
		header = bearer(e.t, subject)
	}
	body, contentType := "", ""
	if method == http.MethodPost {
		body, contentType = `{"n":1}`, "application/json"
	}
	code, _ := e.request(method, path, contentType, body, header...)
	return code
}

func overrideErrorCode(err error) string {
	if policyErr, ok := errors.AsType[*policy.PolicyError](err); ok {
		return policyErr.Code + "@" + policyErr.Field
	}
	return fmt.Sprint(err)
}

// Only the supported controls and actions can be requested, each with
// exactly the members it takes, well-formed identifiers and a bounded
// lifetime. JWT, validation, logging, upstream or route-allow changes have no
// representation and are refused by name.
func TestOverrideRequestGrammar(t *testing.T) {
	valid := []OverrideRequest{
		permitOverride(store.OverrideException, "READER", "write-a", 60),
		permitOverride(store.OverrideContainment, "WRITER", "read-a", 60),
		rateOverride(store.OverrideException, "read-a", 10, 60),
		rateOverride(store.OverrideContainment, "read-a", 1, 60),
		operationContainment("read-b", 60),
		permitOverride(store.OverrideException, "READER", "write-a", store.MinOverrideSeconds),
		permitOverride(store.OverrideException, "READER", "write-a", store.MaxOverrideSeconds),
	}
	wantTargets := []string{"permit:READER:write-a", "permit:WRITER:read-a", "rate:read-a", "rate:read-a", "operation:read-b",
		"permit:READER:write-a", "permit:READER:write-a"}
	for i, r := range valid {
		o, err := r.check()
		if err != nil || o.Target != wantTargets[i] || o.Status != "" {
			t.Errorf("valid request %d %+v: %+v %v", i, r, o, err)
		}
	}

	withRole := func(r OverrideRequest, role string) OverrideRequest { r.Role = new(role); return r }
	withCount := func(r OverrideRequest, count int64) OverrideRequest { r.Count = new(count); return r }
	withSeconds := func(r OverrideRequest, s int64) OverrideRequest { r.ExpiresInSeconds = s; return r }
	permit := permitOverride(store.OverrideException, "READER", "write-a", 60)
	refused := []struct {
		name    string
		request OverrideRequest
		want    string
	}{
		{"JWT control", OverrideRequest{Type: store.OverrideException, Control: "jwt", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"authentication control", OverrideRequest{Type: store.OverrideException, Control: "auth", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"validation control", OverrideRequest{Type: store.OverrideException, Control: "validation", Operation: "write-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"schema control", OverrideRequest{Type: store.OverrideContainment, Control: "schema", Operation: "write-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"logging control", OverrideRequest{Type: store.OverrideException, Control: "logging", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"audit control", OverrideRequest{Type: store.OverrideContainment, Control: "audit", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"upstream control", OverrideRequest{Type: store.OverrideException, Control: "upstream", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"route control name", OverrideRequest{Type: store.OverrideContainment, Control: "route", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"case variant control", OverrideRequest{Type: store.OverrideException, Control: "Permit", Operation: "write-a", Role: new("READER"), ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"operation allow", OverrideRequest{Type: store.OverrideException, Control: store.ControlOperation, Operation: "denied", ExpiresInSeconds: 60}, "UNSUPPORTED_ACTION@type"},
		{"missing type", OverrideRequest{Control: store.ControlPermit, Operation: "write-a", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@type"},
		{"unknown type", OverrideRequest{Type: "priority", Control: store.ControlPermit, Operation: "write-a", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@type"},
		{"missing control", OverrideRequest{Type: store.OverrideException, Operation: "write-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"missing operation", OverrideRequest{Type: store.OverrideException, Control: store.ControlPermit, Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"path as operation", OverrideRequest{Type: store.OverrideException, Control: store.ControlPermit, Operation: "/a/*", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"method and path as operation", OverrideRequest{Type: store.OverrideException, Control: store.ControlPermit, Operation: "POST /a/item", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"encoded operation", OverrideRequest{Type: store.OverrideException, Control: store.ControlPermit, Operation: "write%2Da", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"oversized operation", OverrideRequest{Type: store.OverrideContainment, Control: store.ControlOperation, Operation: strings.Repeat("a", policy.MaxIDLength+1), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"permit without role", OverrideRequest{Type: store.OverrideException, Control: store.ControlPermit, Operation: "write-a", ExpiresInSeconds: 60}, "INVALID_VALUE@role"},
		{"empty role", withRole(permit, ""), "INVALID_VALUE@role"},
		{"role with a line break", withRole(permit, "READER\r\nX"), "INVALID_VALUE@role"},
		{"role on a rate override", withRole(rateOverride(store.OverrideException, "read-a", 10, 60), "READER"), "INVALID_VALUE@role"},
		{"role on an operation containment", withRole(operationContainment("read-a", 60), "READER"), "INVALID_VALUE@role"},
		{"count on a permit override", withCount(permit, 5), "INVALID_VALUE@count"},
		{"rate without count", OverrideRequest{Type: store.OverrideException, Control: store.ControlRate, Operation: "read-a", ExpiresInSeconds: 60}, "INVALID_VALUE@count"},
		{"zero count", rateOverride(store.OverrideContainment, "read-a", 0, 60), "INVALID_VALUE@count"},
		{"negative count", rateOverride(store.OverrideContainment, "read-a", -1, 60), "INVALID_VALUE@count"},
		{"unlimited count", rateOverride(store.OverrideException, "read-a", compile.MaxRateCount+1, 60), "INVALID_VALUE@count"},
		{"missing expiry", withSeconds(permit, 0), "INVALID_EXPIRY@expires_in_seconds"},
		{"negative expiry", withSeconds(permit, -60), "INVALID_EXPIRY@expires_in_seconds"},
		{"expiry above a day", withSeconds(permit, store.MaxOverrideSeconds+1), "INVALID_EXPIRY@expires_in_seconds"},
		{"expiry past the duration range", withSeconds(permit, 10_000_000_000), "INVALID_EXPIRY@expires_in_seconds"},
	}
	for _, r := range refused {
		if _, err := r.request.check(); overrideErrorCode(err) != r.want {
			t.Errorf("%s: %v, want %s", r.name, err, r.want)
		}
	}
}
