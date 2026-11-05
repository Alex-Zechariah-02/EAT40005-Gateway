package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// expectedStages builds the full stage map for a request event: the listed
// stages passed, the terminal stage has the given result, and every other
// stage was not evaluated.
func expectedStages(passed []string, terminal, result string) map[string]any {
	stages := map[string]any{}
	for _, name := range stageNames {
		stages[name] = stageNotRun
	}
	for _, name := range passed {
		stages[name] = stagePass
	}
	if terminal != "" {
		stages[terminal] = result
	}
	return stages
}

// public marks the authentication and rate stages of a public operation as
// not applicable: it reads no token and cannot carry a rate rule.
func public(stages map[string]any) map[string]any {
	stages["JWT"] = stageNotApplicable
	stages["RBAC"] = stageNotApplicable
	stages["RATE"] = stageNotApplicable
	return stages
}

// unrated marks rate limiting as not applicable, for an operation without a
// rate rule.
func unrated(stages map[string]any) map[string]any {
	stages["RATE"] = stageNotApplicable
	return stages
}

// unvalidated marks schema validation as not applicable, for an operation
// that declares neither a body nor a header schema.
func unvalidated(stages map[string]any) map[string]any {
	stages["VALIDATION"] = stageNotApplicable
	return stages
}

// Each request event names its terminal stage, the generation and operation
// it was decided under, and only the stages that actually ran. These
// operations are public, so authentication, authorisation and rate limiting
// are reported as not applicable once the operation is known; a stage after
// the terminal one is never reported as passed.
func TestEventEnvelopeReflectsStagesReached(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policyWithUpstream(freeAddr(t)), seedOptions{})
	e.start(binaries.plain)
	g := e.status().Generation

	beforeRoute := []string{"INGRESS", "CAPTURE", "CANONICAL"}
	routed := append(beforeRoute, "ROUTE")
	checked := append(routed, "ACCESS")
	admitted := append(checked, "BODY")
	forwarded := append(admitted, "AUDIT", "BIND")
	validated := append(slices.Clone(admitted), "VALIDATION")
	validatedForwarded := append(slices.Clone(validated), "AUDIT", "BIND")

	type expected struct {
		kind, stage, decision, errorCode, operation string
		status                                      int
		stages                                      map[string]any
	}
	cases := []struct {
		name    string
		request func() (int, string) // returns status and the request ID
		want    []expected
	}{
		{"admitted and forwarded", func() (int, string) {
			code, _ := e.get("/a/x")
			return code, e.a.receipts()[len(e.a.receipts())-1].RequestID
		}, []expected{
			{"request.admitted", "AUDIT", "admit", "", "read-a", 0, unvalidated(public(expectedStages(admitted, "", "")))},
			{"request.forwarded", "FORWARD", "forwarded", "", "read-a", 200, unvalidated(public(expectedStages(forwarded, "FORWARD", stagePass)))},
		}},
		{"validated body forwarded", func() (int, string) {
			code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`)
			return code, e.a.receipts()[len(e.a.receipts())-1].RequestID
		}, []expected{
			{"request.admitted", "AUDIT", "admit", "", "write-a", 0, public(expectedStages(validated, "", ""))},
			{"request.forwarded", "FORWARD", "forwarded", "", "write-a", 200, public(expectedStages(validatedForwarded, "FORWARD", stagePass))},
		}},
		{"upstream refused", func() (int, string) { return requestID(e.exchange(http.MethodGet, "/z", "", "")) }, []expected{
			{"request.admitted", "AUDIT", "admit", "", "read-z", 0, unvalidated(public(expectedStages(admitted, "", "")))},
			{"request.forwarded", "FORWARD", "upstream_failure", "upstream_unavailable", "read-z", 502, unvalidated(public(expectedStages(forwarded, "FORWARD", stageFail)))},
		}},
		{"protocol upgrade", func() (int, string) {
			return requestID(e.exchange(http.MethodGet, "/a/x", "", "", "Connection", "Upgrade", "Upgrade", "websocket"))
		}, []expected{
			{"request.rejected", "INGRESS", "reject", "bad_request", "", 400, expectedStages(nil, "INGRESS", stageReject)},
		}},
		{"non-canonical path", func() (int, string) { return requestID(e.exchange(http.MethodGet, "/a/%2e%2e/d", "", "")) }, []expected{
			{"request.rejected", "CANONICAL", "reject", "bad_request", "", 400, expectedStages([]string{"INGRESS", "CAPTURE"}, "CANONICAL", stageReject)},
		}},
		{"unmatched", func() (int, string) { return requestID(e.exchange(http.MethodGet, "/nothing", "", "")) }, []expected{
			{"request.rejected", "ROUTE", "reject", "not_found", "", 404, expectedStages(beforeRoute, "ROUTE", stageReject)},
		}},
		{"wrong method", func() (int, string) { return requestID(e.exchange(http.MethodDelete, "/a/x", "", "")) }, []expected{
			{"request.rejected", "ROUTE", "reject", "method_not_allowed", "", 405, expectedStages(beforeRoute, "ROUTE", stageReject)},
		}},
		{"denied", func() (int, string) { return requestID(e.exchange(http.MethodGet, "/d", "", "")) }, []expected{
			{"request.rejected", "ACCESS", "reject", "forbidden", "denied", 403, expectedStages(routed, "ACCESS", stageReject)},
		}},
		{"wrong media type", func() (int, string) { return requestID(e.exchange(http.MethodPost, "/a/item", "text/plain", "{}")) }, []expected{
			{"request.rejected", "BODY", "reject", "unsupported_media_type", "write-a", 415, public(expectedStages(checked, "BODY", stageReject))},
		}},
		{"body against its schema", func() (int, string) {
			return requestID(e.exchange(http.MethodPost, "/a/item", "application/json", "[]"))
		}, []expected{
			{"request.rejected", "VALIDATION", "reject", "bad_request", "write-a", 400, public(expectedStages(admitted, "VALIDATION", stageReject))},
		}},
	}

	seenIDs := map[string]bool{}
	for _, tc := range cases {
		_, requestID := tc.request()
		if requestID == "" {
			t.Fatalf("%s: no request ID", tc.name)
		}
		events := e.eventsWhen(func(events []map[string]any) bool {
			n := 0
			for _, ev := range events {
				if ev["request_id"] == requestID {
					n++
				}
			}
			return n == len(tc.want)
		})
		var got []map[string]any
		for _, ev := range events {
			if ev["request_id"] == requestID {
				got = append(got, ev)
			}
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %d events, want %d: %v", tc.name, len(got), len(tc.want), got)
		}
		for i, want := range tc.want {
			ev := got[i]
			id, _ := ev["event_id"].(string)
			if ev["schema"] != float64(eventSchema) || id == "" || seenIDs[id] {
				t.Errorf("%s: envelope schema %v event_id %q (repeated %v)", tc.name, ev["schema"], id, seenIDs[id])
			}
			seenIDs[id] = true
			if ev["kind"] != want.kind || ev["stage"] != want.stage || ev["decision"] != want.decision {
				t.Errorf("%s: event %v, want kind %s stage %s decision %s", tc.name, ev, want.kind, want.stage, want.decision)
			}
			if code, _ := ev["error"].(string); code != want.errorCode {
				t.Errorf("%s: error %q, want %q", tc.name, code, want.errorCode)
			}
			if status, _ := ev["status"].(float64); int(status) != want.status {
				t.Errorf("%s: status %v, want %d", tc.name, ev["status"], want.status)
			}
			if op, _ := ev["operation"].(string); op != want.operation {
				t.Errorf("%s: operation %q, want %q", tc.name, op, want.operation)
			}
			if want.stage != "INGRESS" && (ev["generation"] != g || ev["profile"] != policy.ProfileCore) {
				t.Errorf("%s: generation %v profile %v", tc.name, ev["generation"], ev["profile"])
			}
			if !sameStages(ev["stages"], want.stages) {
				t.Errorf("%s: stages %v, want %v", tc.name, ev["stages"], want.stages)
			}
			if _, present := ev["subject"]; present {
				t.Errorf("%s: a public request carries subject %v", tc.name, ev["subject"])
			}
		}
	}
}

func requestID(resp *http.Response, body string) (int, string) {
	var reply struct {
		RequestID string `json:"request_id"`
	}
	json.Unmarshal([]byte(body), &reply)
	return resp.StatusCode, reply.RequestID
}

func sameStages(got any, want map[string]any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return bytes.Equal(a, b)
}

// Client-controlled values cannot inject records, leak credentials
// or grow a record without bound.
func TestEventsResistInjectionAndLeakage(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)

	token := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJTRUNSRVQifQ.c2lnbmF0dXJl"
	long := "/a/" + strings.Repeat("x", 6000)
	requests := []string{
		"GET /a/x?access_token=" + token + " HTTP/1.1\r\nHost: gw\r\nAuthorization: Bearer " + token + "\r\nCookie: session=COOKIE-SECRET\r\n",
		"GET /a/%0A%7B%22kind%22:%22forged%22%7D HTTP/1.1\r\nHost: gw\r\n",
		"GET /a/%0D%0A%1B[31m%07 HTTP/1.1\r\nHost: gw\r\n",
		"GET /zzz/%22%5C%22 HTTP/1.1\r\nHost: gw\r\nAuthorization: Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ==\r\n",
		"GET " + long + " HTTP/1.1\r\nHost: gw\r\n",
		"GET /d HTTP/1.1\r\nHost: gw\r\nX-Request-Id: forged\r\nX-Gateway-Generation: forged\r\n",
	}
	for _, r := range requests {
		e.raw(r + "Connection: close\r\n\r\n")
	}
	events := e.eventsWhen(func(events []map[string]any) bool {
		return countKind(events, "request.rejected") >= 3 && countKind(events, "request.forwarded") == 2
	})

	raw, err := os.ReadFile(e.auditLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, "eyJ", "COOKIE-SECRET", "QWxhZGRpbjpvcGVuIHNlc2FtZQ", "Bearer", "Basic", "access_token"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("audit log contains %q", secret)
		}
	}
	for i, line := range bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) {
		for _, c := range line {
			if c < 0x20 || c == 0x7f {
				t.Fatalf("line %d holds raw control byte %#x", i, c)
			}
		}
		if len(line) > 2048 {
			t.Errorf("line %d is %d bytes", i, len(line))
		}
	}

	truncated := false
	for _, ev := range events {
		if ev["kind"] == "forged" {
			t.Fatal("a request created a forged record")
		}
		if ev["request_id"] == "forged" || ev["generation"] == "forged" {
			t.Fatalf("client header copied into the event: %v", ev)
		}
		if path, _ := ev["path"].(string); ev["path_truncated"] == true {
			truncated = len(path) == maxLoggedPath
		}
	}
	if !truncated {
		t.Error("the long path was not truncated to the bound")
	}
	// The request with the token and the long path were admitted, so the
	// checks above cover forwarded requests too.
	e.assertReceipts(2, 0, 0)
}

// With the fixtures behind the gateway, an audit sink that cannot make a
// record durable still stops every admission before forwarding, even for a
// request that passed authentication and authorisation.
func TestAuditFailureBlocksFixtureForwarding(t *testing.T) {
	fixtures := startFixtures(t)
	e := newEnv(t)
	seed(t, e.database, fixturePolicy(fixtures), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=audit-sync")

	c := loadCatalogue(t)
	for _, sc := range c.Scenarios {
		contentType := ""
		if len(sc.Body) > 0 {
			contentType = "application/json"
		}
		var header []string
		if sc.Subject != "" {
			header = bearer(t, sc.Subject)
		}
		// A step refused by validation never reaches the audit gate, so it
		// keeps its own refusal.
		want := http.StatusServiceUnavailable
		if sc.GatewayStage != "" {
			want = sc.GatewayStatus
		}
		if code, _ := e.request(sc.Method, sc.Path, contentType, string(sc.Body), header...); code != want {
			t.Errorf("%s with a failed audit sink: %d, want %d", sc.Name, code, want)
		}
	}
	for name, n := range receiptCounts(fixtures) {
		if n != 0 {
			t.Errorf("fixture %s received %d requests while the audit sink was failing", name, n)
		}
	}
	if s := e.status(); !s.AuditFailed {
		t.Fatalf("status %+v does not report the audit failure", s)
	}
}
