// Failed rollback, lifecycle audit failure and interrupt handling.

package gateway

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// An interrupt (Ctrl+Break on Windows) stops the gateway through its graceful
// path: the process exits with status 0, both listeners close, the audit file
// ends with a complete record, and the next start serves the same generation
// without finding an incomplete record. Every admission, which waits for its
// durable record, is in the file, and so is every outcome and refusal event,
// queued without acknowledgement, because the stop drains the queue.
func TestInterruptStopsGatewayGracefully(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.interruptible = true
	e.start(binaries.plain)
	generation := e.status().Generation
	for range 3 {
		if code, _ := e.get("/a/x"); code != http.StatusOK {
			t.Fatalf("admitted request: %d", code)
		}
		if code, _ := e.get("/d"); code != http.StatusForbidden {
			t.Fatalf("denied request: %d", code)
		}
	}

	interrupt(t, e.process)
	if code := e.waitExit(); code != 0 {
		t.Fatalf("exit status %d after the interrupt\n%s", code, e.output)
	}
	if dialable(e.dataAddr) || dialable(e.admin) {
		t.Fatal("a listener still accepts connections after the gateway exited")
	}
	data, err := os.ReadFile(e.auditLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("the audit file does not end with a complete record")
	}
	events := e.auditEvents()
	for kind, want := range map[string]int{"request.admitted": 3, "request.forwarded": 3, "request.rejected": 3} {
		if n := countKind(events, kind); n != want {
			t.Fatalf("%d %s records in the audit file, want %d", n, kind, want)
		}
	}

	e.interruptible = false
	e.start(binaries.plain)
	if s := e.status(); s.Generation != generation {
		t.Fatalf("generation %s after the restart, want %s", s.Generation, generation)
	}
	events = e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "controller.reconciled") >= 2 })
	if countKind(events, "controller.reconciled") != 2 || countKind(events, "audit.recovered") != 0 {
		t.Fatalf("restart events: %d reconciled, %d recovered", countKind(events, "controller.reconciled"), countKind(events, "audit.recovered"))
	}
}

// When the base a rollback must restore cannot be compiled, nothing
// is committed and the gateway stops admitting protected requests rather than
// keep serving the provisional policy. A restart reconciles from the
// database. The same holds for the automatic rollback.
func TestFailedRollbackStopsAdmission(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=rollback-target-invalid")
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	e.provisionalFrom(harness, e.cpaCandidate(), 1, 60, "failed-rollback-01")

	code, outcome, body := e.resolve(harness, "rollback", 2, `"r1"`, "failed-rollback-request")
	if code != http.StatusInternalServerError || outcome.Outcome != "rejected" || outcome.Reason != "TARGET_INVALID" {
		t.Fatalf("rollback with an unusable target: %d %s", code, body)
	}
	assertNothingAdmitted := func() {
		t.Helper()
		if health := e.health(); health != "unavailable" {
			t.Fatalf("health %q", health)
		}
		e.resetReceivers()
		bobToken := "Bearer " + issuer.tokenFor(t, "bob")
		for _, probe := range []struct{ method, path string }{{"GET", "/b/x"}, {"POST", "/a/item"}, {"GET", "/a/x"}} {
			if code, _ := e.request(probe.method, probe.path, "application/json", `{}`, "Authorization", bobToken); code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s: %d, want 503", probe.method, probe.path, code)
			}
		}
		e.assertReceipts(0, 0, 0)
	}
	assertNothingAdmitted()
	if reason := e.unavailableReason(); !strings.Contains(reason, "not completed") {
		t.Fatalf("recorded reason %q", reason)
	}
	if code, _, _ := e.resolve(harness, "confirm", 2, `"r1"`, "confirm-while-unavailable"); code != http.StatusServiceUnavailable {
		t.Fatalf("confirmation while unavailable: %d", code)
	}

	// The provisional base is still the stored authority and unexpired.
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.Provisional != 2 {
		t.Fatalf("after restart: %+v", s)
	}
	e.expectEnforced(true, "")
	if code, _, body := e.resolve(harness, "rollback", 2, `"r1"`, "failed-rollback-retry"); code != http.StatusOK {
		t.Fatalf("rollback after restart: %d %s", code, body)
	}
	e.expectEnforced(false, "")

	// The automatic rollback fails the same way.
	e.start(binaries.fault, "GATEWAY_FAULT=rollback-target-invalid")
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	_, expiring := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "failed-rollback-02")
	waitFor(t, expiring.Deadline.Add(5*time.Second), "the failed automatic rollback", func() bool { return e.health() == "unavailable" })
	assertNothingAdmitted()
	e.start(binaries.plain)
	if s := e.status(); s.Revision != 1 || s.Provisional != 0 {
		t.Fatalf("after restart past the deadline: %+v", s)
	}
	e.expectEnforced(false, "")
	e.stop()
	list := storedActivations(t, e.database)
	if len(list) != 2 || list[0].Disposition != dispositionRolledBack || list[1].Disposition != dispositionTimedOut {
		t.Fatalf("stored activations %+v", list)
	}
}

// Lifecycle transitions need the mandatory audit record. Without the
// intent record nothing is committed; when the outcome record fails after the
// commit, the change is reported committed with its outcome pending,
// admission stops, and the restarted gateway records the outcome once.
// Request fields never reach the audit log, so they cannot leak or forge
// records.
func TestLifecycleAuditFailure(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	code, staged, body := e.stage(harness, e.cpaCandidate(), `"r1"`, "stage-audit-failure")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	candidate := staged.Candidate.candidateRecord

	// Fields carrying a token, a key marker and a forged record.
	token := issuer.tokenFor(t, "alice")
	for i, checksum := range []string{token, "-----BEGIN PRIVATE KEY-----", "x\r\n{\"kind\":\"cpa.confirmed\",\"actor\":\"forged\"}"} {
		body := string(mustJSON(map[string]any{"candidate": candidate.ID, "checksum": checksum, "confirm_within_seconds": 60}))
		code, data, _ := e.as(harness, http.MethodPost, "/admin/provisional", body, "If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("hostile-fields-%02d", i))
		if code != http.StatusPreconditionFailed || !strings.Contains(data, `"field":"checksum"`) {
			t.Fatalf("hostile checksum %d: %d %s", i, code, data)
		}
	}

	// No intent record: refused, nothing committed.
	e.start(binaries.fault, "GATEWAY_FAULT=audit-full")
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	if code, outcome, body := e.activate(harness, candidate, 60, `"r1"`, "audit-full-activation"); code != http.StatusServiceUnavailable ||
		outcome.Reason != "AUDIT_UNAVAILABLE" {
		t.Fatalf("activation without audit: %d %s", code, body)
	}
	e.stop()
	if list := storedActivations(t, e.database); len(list) != 0 {
		t.Fatalf("activation committed without its intent record: %+v", list)
	}

	// The outcome record fails after the commit, for an activation and then
	// for a confirmation.
	for _, step := range []struct {
		action, kind, key string
		confirmed         int64
	}{
		{"activate", "cpa.activated", "audit-outcome-activation", 1},
		{"confirm", "cpa.confirmed", "audit-outcome-confirmation", 2},
	} {
		e.start(binaries.fault, "GATEWAY_FAULT=audit-fail-published")
		harness = e.mustLogin(harnessAdmin, harnessPassword)
		var outcome Outcome
		if step.action == "activate" {
			code, outcome, body = e.activate(harness, candidate, 60, `"r1"`, step.key)
		} else {
			code, outcome, body = e.resolve(harness, "confirm", 2, `"r1"`, step.key)
		}
		if code != http.StatusServiceUnavailable || outcome.Outcome != "committed" || outcome.Reason != "OUTCOME_AUDIT_PENDING" {
			t.Fatalf("%s with a failing outcome record: %d %s", step.action, code, body)
		}
		if s := e.status(); !s.AuditFailed || s.Confirmed != step.confirmed {
			t.Fatalf("status after %s: %+v", step.action, s)
		}
		e.resetReceivers()
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+token); code != http.StatusServiceUnavailable {
			t.Fatalf("admission after the failed outcome record: %d", code)
		}
		e.assertReceipts(0, 0, 0)

		e.start(binaries.plain)
		if s := e.status(); s.Confirmed != step.confirmed || s.AuditFailed {
			t.Fatalf("after restart: %+v", s)
		}
		e.expectEnforced(true, "")
		id := "admin:" + harnessAdmin + ":" + step.key
		events := e.eventsWhen(func(events []map[string]any) bool { return findTransition(events, step.kind, id) != nil })
		n := 0
		for _, ev := range events {
			if ev["kind"] == step.kind && ev["transition"] == id {
				n++
				if ev["reason"] != "reconciled" {
					t.Fatalf("outcome event %v", ev)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d %s events, want 1", n, step.kind)
		}
	}

	e.stop()
	for _, ev := range e.auditEvents() {
		if ev["actor"] == "forged" {
			t.Fatalf("forged record %v", ev)
		}
	}
	assertAbsent(t, e, token, "BEGIN PRIVATE KEY", harnessPassword)
}
