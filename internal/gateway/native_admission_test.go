// Native control enforcement and admission status.

package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// The five controls run in the native process as in the shared enforcement
// pipeline: authentication against the one trusted key, roles held by the
// policy, the rate limit, body and header schemas under the body ceiling, the
// validated-header agreement and the gateway's own identity headers. Every
// refusal is recorded at its stage and reaches no upstream.
func TestNativeGatewayEnforcesControls(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	for i := range p.Operations {
		switch p.Operations[i].ID {
		case "write-a":
			p.Operations[i].Body = jsonBody(64, `{"type":"object","additionalProperties":false,"required":["n"],"properties":{"n":{"type":"integer"}}}`)
			p.Operations[i].Headers = json.RawMessage(`{"type":"object","additionalProperties":true,"required":["X-Mode"],"properties":{"X-Mode":{"type":"string","enum":["fast","safe"]}}}`)
		case "read-b":
			p.Operations[i].Rate = &policy.RateRule{Algorithm: "sliding-window", Count: 2, WindowSeconds: 60, Key: "subject"}
		}
	}
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	generation := e.status().Generation

	token := func(claims map[string]any) []string {
		return []string{"Authorization", "Bearer " + issuer.token(t, claims)}
	}
	expired, foreign, claimed := validClaims("alice"), validClaims("alice"), validClaims("mallory")
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	foreign["iss"] = "another-issuer"
	claimed["roles"] = []string{"UNKNOWN"}
	altered := issuer.tokenFor(t, "alice")
	altered = altered[:len(altered)-6] + "AAAAAA"
	asAlice := append(bearer(t, "alice"), "X-Mode", "fast")

	refusals := []struct {
		name, method, path, contentType, body string
		header                                []string
		status                                int
		stage                                 string
	}{
		{"no token", http.MethodGet, "/a/x", "", "", nil, http.StatusUnauthorized, "JWT"},
		{"expired token", http.MethodGet, "/a/x", "", "", token(expired), http.StatusUnauthorized, "JWT"},
		{"token of another issuer", http.MethodGet, "/a/x", "", "", token(foreign), http.StatusUnauthorized, "JWT"},
		{"altered signature", http.MethodGet, "/a/x", "", "", []string{"Authorization", "Bearer " + altered}, http.StatusUnauthorized, "JWT"},
		{"unknown role claimed in the verified token", http.MethodGet, "/a/x", "", "", token(claimed), http.StatusForbidden, "RBAC"},
		{"role without the permit", http.MethodPost, "/a/item", "application/json", `{"n":1}`, append(bearer(t, "bob"), "X-Mode", "fast"), http.StatusForbidden, "RBAC"},
		{"body outside its schema", http.MethodPost, "/a/item", "application/json", `{"n":"one"}`, asAlice, http.StatusBadRequest, "VALIDATION"},
		{"missing validated header", http.MethodPost, "/a/item", "application/json", `{"n":1}`, bearer(t, "alice"), http.StatusBadRequest, "VALIDATION"},
		{"validated header named in Connection", http.MethodPost, "/a/item", "application/json", `{"n":1}`, append(asAlice, "Connection", "X-Mode"), http.StatusBadRequest, "VALIDATION"},
		{"body above the operation ceiling", http.MethodPost, "/a/item", "application/json", `{"n":` + strings.Repeat("1", 64) + `}`, asAlice, http.StatusRequestEntityTooLarge, "BODY"},
		{"another media type", http.MethodPost, "/a/item", "text/plain", `{"n":1}`, asAlice, http.StatusUnsupportedMediaType, "BODY"},
		{"operation outside the policy", http.MethodGet, "/c/x", "", "", bearer(t, "alice"), http.StatusNotFound, "ROUTE"},
		{"denied operation", http.MethodGet, "/d", "", "", nil, http.StatusForbidden, "ACCESS"},
	}
	for _, r := range refusals {
		resp, body := e.exchange(r.method, r.path, r.contentType, r.body, r.header...)
		_, id := requestID(resp, body)
		events := e.requestEvents(id, 1)
		if resp.StatusCode != r.status || len(events) != 1 || events[0]["stage"] != r.stage || events[0]["kind"] != "request.rejected" {
			t.Errorf("%s: %d %s, events %v; want %d at %s", r.name, resp.StatusCode, body, events, r.status, r.stage)
		}
	}
	e.assertReceipts(0, 0, 0)

	// Admitted: the upstream receives the gateway's identity headers, not the
	// client's, with the validated header and body unchanged and no
	// credential.
	forged := append(asAlice, "X-Gateway-Subject", "bob", "X-Gateway-Roles", "ADMIN")
	if code, body := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, forged...); code != http.StatusOK || body != "A ok" {
		t.Fatalf("admitted write: %d %q", code, body)
	}
	r := e.a.receipts()[0]
	if r.Headers.Get("X-Gateway-Subject") != "alice" || r.Headers.Get("X-Gateway-Roles") != "WRITER" || r.Headers.Get("X-Mode") != "fast" ||
		r.Authorization || r.Body != `{"n":1}` || r.Generation != generation {
		t.Fatalf("upstream received %+v", r)
	}
	statuses := []int{}
	for range 3 {
		code, _ := e.get("/b/x", bearer(t, "bob")...)
		statuses = append(statuses, code)
	}
	if fmt.Sprint(statuses) != "[200 200 429]" {
		t.Fatalf("rate limit 2 per minute: %v", statuses)
	}
	e.assertReceipts(1, 2, 0)
	raw, _ := os.ReadFile(e.auditLog)
	if strings.Contains(string(raw), "Bearer") || strings.Contains(string(raw), altered[:40]) {
		t.Fatal("the audit log holds a credential")
	}
}

// Listening is not admission. The startup output and the protected
// status say whether protected admission can run: not in bootstrap or
// recovery, without usable authority, with a failed audit log or a
// generation past its deadline, and not once the stop has begun. A confirmed
// deny-all base is ready, though it admits nothing. The unauthenticated
// health check stays a liveness and mode report.
func TestNativeAdmissionStatus(t *testing.T) {
	// unavailableAtCapture checks that a protected request is refused before
	// any policy decision and reaches no upstream.
	unavailableAtCapture := func(e *env) {
		e.t.Helper()
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		_, id := requestID(resp, body)
		if events := e.requestEvents(id, 1); resp.StatusCode != http.StatusServiceUnavailable || len(events) != 1 || events[0]["stage"] != "CAPTURE" {
			e.t.Fatalf("request: %d %s, events %v", resp.StatusCode, body, events)
		}
		e.assertReceipts(0, 0, 0)
	}

	t.Run("bootstrap", func(t *testing.T) {
		e := newEnv(t)
		if out, code := e.runOperator(binaries.plain, nil, "install", "--admin", "root"); code != 0 {
			t.Fatalf("install: %d %s", code, out)
		}
		e.start(binaries.plain)
		e.waitOutput(`"msg":"protected admission","admission":"bootstrap_locked"`)
		if health := e.health(); health != modeBootstrap {
			t.Fatalf("health %q", health)
		}
		unavailableAtCapture(e)
	})

	t.Run("recovery required", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{mode: modeRecovery})
		e.start(binaries.plain)
		e.waitOutput(`"admission":"recovery_required"`)
		if s := e.status(); s.Admission != modeRecovery || s.State != modeRecovery || e.health() != modeRecovery {
			t.Fatalf("status %+v", s)
		}
		unavailableAtCapture(e)
	})

	for _, tc := range []struct {
		name    string
		prepare func(e *env)
		reason  string
	}{
		{"missing database", func(e *env) {}, "missing or empty"},
		{"earlier schema version", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{version: databaseVersion - 1})
		}, "run its migrate command"},
		{"stored revision altered", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{})
			db := inspect(e.t, e.database)
			if _, err := db.Exec(`UPDATE revision SET document = ? WHERE id = 1`, []byte(`{}`)); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "checksum mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.prepare(e)
			e.start(binaries.plain)
			e.waitOutput(`"msg":"protected admission","admission":"unavailable"`)
			if health := e.health(); health != "unavailable" {
				t.Fatalf("health %q", health)
			}
			if reason := e.unavailableReason(); !strings.Contains(reason, tc.reason) {
				t.Fatalf("recorded reason %q, want %q", reason, tc.reason)
			}
			unavailableAtCapture(e)
		})
	}

	t.Run("confirmed deny-all base", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.denyAllPolicy(), seedOptions{})
		e.start(binaries.plain)
		if s := e.status(); s.Admission != "ready" {
			t.Fatalf("status %+v", s)
		}
		for path, want := range map[string]int{"/a/x": http.StatusForbidden, "/b/x": http.StatusForbidden, "/zzz": http.StatusNotFound} {
			if code, _ := e.get(path); code != want {
				t.Errorf("%s: %d, want %d", path, code, want)
			}
		}
		e.assertReceipts(0, 0, 0)
	})

	t.Run("audit log failed while serving", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.fault, "GATEWAY_FAULT=audit-fail-outcome")
		if s := e.status(); s.Admission != "ready" {
			t.Fatalf("before the failure %+v", s)
		}
		if code, _ := e.get("/a/x"); code != http.StatusOK {
			t.Fatalf("first request: %d", code)
		}
		waitFor(t, time.Now().Add(10*time.Second), "the audit failure", func() bool { return e.status().AuditFailed })
		if s := e.status(); s.Admission != "audit_failed" || s.State != modeNormal || e.health() != modeNormal {
			t.Fatalf("after the failure %+v, health %q", s, e.health())
		}
		if code, _ := e.get("/a/x"); code != http.StatusServiceUnavailable {
			t.Fatalf("request with the failed audit log: %d", code)
		}
		e.assertReceipts(1, 0, 0)
	})

	t.Run("generation past its deadline", func(t *testing.T) {
		e := newEnv(t)
		provisional := e.policy(e.a, true)
		deadline := time.UnixMilli(time.Now().Add(1500 * time.Millisecond).UnixMilli())
		seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: deadline})
		e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
		if s := e.status(); s.Admission != "ready" || !s.NotAfter.Equal(deadline) {
			t.Fatalf("provisional %+v", s)
		}
		time.Sleep(time.Until(deadline.Add(200 * time.Millisecond)))
		if s := e.status(); s.Admission != "generation_expired" || s.Provisional != 2 {
			t.Fatalf("past the deadline %+v", s)
		}
		unavailableAtCapture(e)
		// The refused request ends the provisional base; the restored one is
		// ready.
		waitFor(t, time.Now().Add(10*time.Second), "the rollback", func() bool { s := e.status(); return s.Admission == "ready" && s.Revision == 1 })
		if code, _ := e.get("/b/x"); code != http.StatusNotFound {
			t.Fatalf("operation of the rolled-back revision: %d", code)
		}
	})
}
