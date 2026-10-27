package gateway

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const crashExit = 86

func TestCrashDuringPublication(t *testing.T) {
	cases := []struct {
		point     string
		committed bool   // whether the transition survives the crash
		receipt   string // publication receipt recorded after restart
	}{
		{"before-commit", false, ""},
		{"after-commit", true, "reconciliation"},
		{"after-swap", true, "reconciliation"},
		{"after-receipt", true, "publisher"},
	}
	for _, tc := range cases {
		t.Run(tc.point, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+tc.point)

			change := Change{ID: "add-b", Expected: 1, Policy: mustJSON(e.policy(e.a, true))}
			if _, code := e.publish(change); code != 0 {
				t.Fatalf("publication answered %d instead of crashing", code)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d, want %d\n%s", code, crashExit, e.output)
			}

			wantRevisions, wantCommitted := 1, 0
			if tc.committed {
				wantRevisions, wantCommitted = 2, 1
			}
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != wantRevisions {
				t.Fatalf("after crash: %d revisions, want %d", n, wantRevisions)
			}
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE outcome = 'committed'`); n != wantCommitted {
				t.Fatalf("after crash: %d committed transitions, want %d", n, wantCommitted)
			}

			e.start(binaries.fault)
			s := e.status()
			if s.Revision != int64(wantRevisions) {
				t.Fatalf("restart serves revision %d, want %d", s.Revision, wantRevisions)
			}
			want := http.StatusNotFound
			if tc.committed {
				want = http.StatusOK
			}
			if code, _ := e.get("/b/x"); code != want {
				t.Fatalf("GET /b/x after restart: %d, want %d", code, want)
			}

			outcome, code := e.publish(change)
			if code != http.StatusOK || outcome.Revision != 2 {
				t.Fatalf("retry with the same identity: %d %+v", code, outcome)
			}
			e.stop()
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 2 {
				t.Fatalf("after retry: %d revisions, want 2 (applied exactly once)", n)
			}
			if tc.committed {
				var by string
				db := inspect(t, e.database)
				db.QueryRow(`SELECT published_by FROM transition WHERE id = 'add-b'`).Scan(&by)
				db.Close()
				if by != tc.receipt {
					t.Fatalf("published_by %q, want %q", by, tc.receipt)
				}
			}
		})
	}
}

func TestExpiredProvisionalNeverServes(t *testing.T) {
	t.Run("expired while stopped", func(t *testing.T) {
		e := newEnv(t)
		provisional := e.policy(e.a, true)
		seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: time.Now().Add(-time.Second)})
		e.start(binaries.plain)
		if s := e.status(); s.Revision != 1 {
			t.Fatalf("serving revision %d, want confirmed revision 1", s.Revision)
		}
		if code, _ := e.get("/b/x"); code != http.StatusNotFound {
			t.Fatalf("expired grant: %d", code)
		}
		e.assertReceipts(0, 0, 0)
		e.stop()
		if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id = 'auto-rollback:2' AND outcome = 'committed'`); n != 1 {
			t.Fatalf("%d automatic rollbacks recorded, want 1", n)
		}
	})

	// Without a timer, the first request after the deadline is refused and
	// makes the controller roll back; no request is ever served by the
	// expired grant.
	t.Run("deadline passes while running without timer", func(t *testing.T) {
		e := newEnv(t)
		provisional := e.policy(e.a, true)
		deadline := time.Now().Add(3 * time.Second)
		seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: deadline})
		e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
		if code, _ := e.get("/b/x"); code != http.StatusOK {
			t.Fatalf("before deadline: %d", code)
		}
		time.Sleep(time.Until(deadline) + 200*time.Millisecond)
		if code, _ := e.get("/b/x"); code != http.StatusServiceUnavailable {
			t.Fatalf("after deadline: %d, want 503", code)
		}
		limit := time.Now().Add(5 * time.Second)
		for e.status().Revision != 1 {
			if time.Now().After(limit) {
				t.Fatal("the refused request did not lead to the automatic rollback")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if code, _ := e.get("/b/x"); code != http.StatusNotFound {
			t.Fatalf("after rollback: %d, want 404", code)
		}
		e.assertReceipts(0, 1, 0)
	})

	t.Run("deadline passes while running", func(t *testing.T) {
		e := newEnv(t)
		provisional := e.policy(e.a, true)
		deadline := time.Now().Add(2 * time.Second)
		seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: deadline})
		e.start(binaries.plain)
		if s := e.status(); s.Revision != 2 {
			t.Fatalf("before deadline serving %d, want provisional 2", s.Revision)
		}
		time.Sleep(time.Until(deadline))
		limit := time.Now().Add(5 * time.Second)
		for e.status().Revision != 1 {
			if time.Now().After(limit) {
				t.Fatal("automatic rollback did not happen")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if code, _ := e.get("/b/x"); code != http.StatusNotFound {
			t.Fatalf("after rollback: %d", code)
		}
		e.assertReceipts(0, 0, 0)
	})

	for _, point := range []string{"timeout-before-commit", "timeout-after-commit"} {
		t.Run("crash at "+point, func(t *testing.T) {
			e := newEnv(t)
			provisional := e.policy(e.a, true)
			seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: time.Now().Add(-time.Second)})
			e.launch(binaries.fault, []string{"run", "--config", e.config}, "GATEWAY_FAULT="+point)
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d, want %d\n%s", code, crashExit, e.output)
			}
			e.start(binaries.plain)
			if s := e.status(); s.Revision != 1 {
				t.Fatalf("serving revision %d, want 1", s.Revision)
			}
			if code, _ := e.get("/b/x"); code != http.StatusNotFound {
				t.Fatalf("expired grant after recovery: %d", code)
			}
			e.stop()
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id = 'auto-rollback:2'`); n != 1 {
				t.Fatalf("%d rollback records, want exactly 1", n)
			}
		})
	}

	t.Run("clock behind recorded time", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{highWater: time.Now().Add(time.Hour)})
		e.start(binaries.plain)
		if s := e.status(); s.State != "clock_regression" {
			t.Fatalf("state %q, want clock_regression", s.State)
		}
		if code, _ := e.get("/a/x"); code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", code)
		}
		e.assertReceipts(0, 0, 0)
	})
}

func TestUnavailableAuthorityAdmitsNothing(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(e *env)
		state   string // expected substring of the recorded reason
	}{
		{"missing database", func(e *env) {}, "missing or empty"},
		{"empty file", func(e *env) { os.WriteFile(e.database, nil, 0o600) }, "missing or empty"},
		{"corrupt file", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{})
			corrupt(e.t, e.database)
		}, "unavailable"},
		{"checksum mismatch", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{})
			db := inspect(e.t, e.database)
			if _, err := db.Exec(`UPDATE revision SET document = ? WHERE id = 1`, mustJSON(e.policy(e.b, true))); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "checksum mismatch"},
		{"stored revision in the earlier format", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{})
			earlier := []byte(`{"upstreams":[{"id":"A","dial":"` + e.a.addr + `"}],` +
				`"operations":[{"id":"read-a","method":"GET","target":"/a/*","upstream":"A","access":"allow"}]}`)
			db := inspect(e.t, e.database)
			if _, err := db.Exec(`UPDATE revision SET document = ?, checksum = ? WHERE id = 1`, earlier, documentChecksum(earlier)); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "SCHEMA_VERSION_UNSUPPORTED"},
		// A stored routing-only document never enforced authentication, so it
		// must not be served by a gateway that is expected to.
		{"stored revision in schema version 1", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{})
			earlier := []byte(`{"schema_version":1,"profile":"routing-only","upstreams":[{"id":"A","dial":"` + e.a.addr + `"}],` +
				`"apis":[{"id":"a","upstream":"A"}],` +
				`"operations":[{"id":"read-a","api":"a","method":"GET","target":"/a/*","access":"allow","auth":"required"}]}`)
			db := inspect(e.t, e.database)
			if _, err := db.Exec(`UPDATE revision SET document = ?, checksum = ? WHERE id = 1`, earlier, documentChecksum(earlier)); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "SCHEMA_VERSION_UNSUPPORTED"},
		{"recovery required", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{mode: modeRecovery})
		}, modeRecovery},
		{"bootstrap locked", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{mode: modeBootstrap})
		}, modeBootstrap},
		{"rollback recorded but provisional still active", func(e *env) {
			provisional := e.policy(e.a, true)
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: time.Now().Add(-time.Second)})
			db := inspect(e.t, e.database)
			if _, err := db.Exec(`INSERT INTO transition (id, kind, actor, target, digest, outcome, reason, result_revision, committed_at)
				VALUES ('auto-rollback:2', 'rollback', 'controller', 'revision 2', 'auto-rollback:2', 'committed', '', 1, 0)`); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "auto-rollback:2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.prepare(e)
			e.start(binaries.fault)
			wantHealth, wantMutation := "unavailable", http.StatusServiceUnavailable
			if tc.state == modeRecovery || tc.state == modeBootstrap {
				wantHealth, wantMutation = tc.state, http.StatusOK
			}
			if health := e.health(); health != wantHealth {
				t.Fatalf("health %q, want %q", health, wantHealth)
			}
			if reason := e.unavailableReason(); !strings.Contains(reason, tc.state) {
				t.Fatalf("recorded reason %q, want it to contain %q", reason, tc.state)
			}
			for _, path := range []string{"/a/x", "/b/x", "/d"} {
				if code, _ := e.get(path); code != http.StatusServiceUnavailable {
					t.Fatalf("GET %s: %d, want 503", path, code)
				}
			}
			e.assertReceipts(0, 0, 0)
			if _, code := e.publish(Change{ID: "x", Expected: 1, Policy: mustJSON(e.policy(e.a, true))}); code != http.StatusServiceUnavailable {
				t.Fatalf("publication accepted in unavailable state: %d", code)
			}
			code, body, _ := e.harnessCall(http.MethodPut, "/admin/blocked-accounts/someone", `{"reason":"contain"}`,
				"If-Match", `"r1"`, "Idempotency-Key", "unavailable-probe-1")
			if code != wantMutation {
				t.Fatalf("independent containment in unavailable state: %d %s, want %d", code, body, wantMutation)
			}
			if e.health() != wantHealth {
				t.Fatal("containment changed deployment mode")
			}
		})
	}

	t.Run("missing database is not created", func(t *testing.T) {
		e := newEnv(t)
		e.start(binaries.plain)
		e.stop()
		if _, err := os.Stat(e.database); !os.IsNotExist(err) {
			t.Fatalf("database was created on startup: %v", err)
		}
	})

	t.Run("second instance on the same database", func(t *testing.T) {
		first := newEnv(t)
		seed(t, first.database, first.policy(first.a, false), seedOptions{})
		first.start(binaries.plain)

		second := newEnv(t)
		second.database = first.database
		second.writeConfig(configOptions{})
		second.launch(binaries.plain, []string{"run", "--config", second.config})
		if code := second.waitExit(); code == 0 {
			t.Fatalf("second instance kept running\n%s", second.output)
		}
		if !strings.Contains(second.output.String(), "held by another gateway process") {
			t.Fatalf("second instance failed for another reason:\n%s", second.output)
		}
		if code, _ := first.get("/a/x"); code != http.StatusOK {
			t.Fatalf("first instance after second start attempt: %d", code)
		}
	})
}

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

// A crash at each point of an activation, confirmation or
// rollback. Before its commit nothing changed and the retried request applies
// once; from the commit on, the restarted gateway serves the committed state,
// completes the receipts, records the outcome event exactly once, and the
// retried request returns the recorded receipt.
func TestLifecycleSurvivesCrash(t *testing.T) {
	cases := []struct {
		action    string // activate, confirm or rollback
		point     string
		committed bool
	}{
		{"activate", "before-commit", false},
		{"activate", "commit-partway", false},
		{"activate", "after-commit", true},
		{"activate", "after-swap", true},
		{"activate", "after-receipt", true},
		{"activate", "after-outcome", true},
		{"confirm", "confirm-before-commit", false},
		{"confirm", "confirm-after-commit", true},
		{"confirm", "confirm-after-swap", true},
		{"confirm", "confirm-after-receipt", true},
		{"confirm", "after-outcome", true},
		{"rollback", "rollback-before-commit", false},
		{"rollback", "rollback-after-commit", true},
		{"rollback", "rollback-after-swap", true},
		{"rollback", "rollback-after-receipt", true},
		{"rollback", "after-outcome", true},
	}
	for _, tc := range cases {
		t.Run(tc.action+" "+tc.point, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.plain)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			code, staged, body := e.stage(harness, e.cpaCandidate(), `"r1"`, "stage-crash-candidate")
			if code != http.StatusCreated {
				t.Fatalf("staging: %d %s", code, body)
			}
			candidate := staged.Candidate.candidateRecord
			if tc.action != "activate" {
				if code, _, body := e.activate(harness, candidate, 60, `"r1"`, "activate-before-crash"); code != http.StatusOK {
					t.Fatalf("activation: %d %s", code, body)
				}
			}
			const key = "crash-case-request"
			send := func() (int, Outcome, string) {
				if tc.action == "activate" {
					return e.activate(harness, candidate, 60, `"r1"`, key)
				}
				return e.resolve(harness, tc.action, 2, `"r1"`, key)
			}

			e.start(binaries.fault, "GATEWAY_FAULT="+tc.point)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			if code, _, body := send(); code != 0 {
				t.Fatalf("answered %d instead of crashing: %s", code, body)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d, want %d\n%s", code, crashExit, e.output)
			}

			// What a restart serves: the committed state, or the earlier one.
			type state struct{ confirmed, provisional int64 }
			before := map[string]state{"activate": {1, 0}, "confirm": {1, 2}, "rollback": {1, 2}}[tc.action]
			after := map[string]state{"activate": {1, 2}, "confirm": {2, 0}, "rollback": {1, 0}}[tc.action]
			want := before
			if tc.committed {
				want = after
			}
			e.start(binaries.plain)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			s := e.status()
			if s.Confirmed != want.confirmed || s.Provisional != want.provisional {
				t.Fatalf("after restart: %+v, want confirmed %d provisional %d", s, want.confirmed, want.provisional)
			}
			e.expectEnforced(want.provisional != 0 || want.confirmed == 2, s.Generation)

			// The retry applies once, or returns what was recorded.
			code, outcome, body := send()
			if code != http.StatusOK || outcome.Outcome != "committed" || outcome.Transition != "admin:"+harnessAdmin+":"+key {
				t.Fatalf("retry: %d %s", code, body)
			}
			s = e.status()
			if s.Confirmed != after.confirmed || s.Provisional != after.provisional {
				t.Fatalf("after retry: %+v", s)
			}
			if tc.action == "activate" && !outcome.Deadline.Equal(s.Deadline) {
				t.Fatalf("retry deadline %v, stored %v", outcome.Deadline, s.Deadline)
			}
			if tc.action != "activate" && outcome.Disposition == "" {
				t.Fatalf("retry receipt without its disposition: %s", body)
			}

			e.stop()
			id := "admin:" + harnessAdmin + ":" + key
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id = ? AND outcome = 'committed'
				AND published_at IS NOT NULL AND audited_at IS NOT NULL`, id); n != 1 {
				t.Fatalf("%d complete records of %s", n, id)
			}
			receipt := "publisher"
			if tc.point == "after-commit" || tc.point == "after-swap" || tc.point == "confirm-after-commit" ||
				tc.point == "confirm-after-swap" || tc.point == "rollback-after-commit" || tc.point == "rollback-after-swap" {
				receipt = "reconciliation"
			}
			if by := queryString(t, e.database, `SELECT published_by FROM transition WHERE id = ?`, id); by != receipt {
				t.Fatalf("publication receipt by %q, want %q", by, receipt)
			}
			kind := map[string]string{"activate": "cpa.activated", "confirm": "cpa.confirmed", "rollback": "cpa.rolled_back"}[tc.action]
			outcomes := 0
			for _, ev := range e.auditEvents() {
				if ev["kind"] == kind && ev["transition"] == id {
					outcomes++
				}
			}
			if outcomes != 1 {
				t.Fatalf("%d %s events for %s, want 1", outcomes, kind, id)
			}
			list := storedActivations(t, e.database)
			wantDisposition := map[string]string{"activate": "", "confirm": dispositionConfirmed, "rollback": dispositionRolledBack}[tc.action]
			if len(list) != 1 || list[0].Disposition != wantDisposition {
				t.Fatalf("stored activations %+v", list)
			}
		})
	}
}

// A crash while the running gateway times out a provisional base is
// recovered at the next start, before anything is served, with one timeout.
func TestTimeoutSurvivesCrash(t *testing.T) {
	for _, point := range []string{"timeout-before-commit", "timeout-after-commit", "timeout-after-swap", "timeout-after-receipt"} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+point)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "timeout-crash-case")
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d, want %d\n%s", code, crashExit, e.output)
			}
			e.start(binaries.plain)
			if s := e.status(); s.Revision != 1 || s.Provisional != 0 || s.Confirmed != 1 {
				t.Fatalf("after restart: %+v", s)
			}
			e.expectEnforced(false, "")
			e.stop()
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id = 'auto-rollback:2' AND outcome = 'committed'
				AND published_at IS NOT NULL AND audited_at IS NOT NULL`); n != 1 {
				t.Fatalf("%d complete timeout records", n)
			}
			if list := storedActivations(t, e.database); len(list) != 1 || list[0].Disposition != dispositionTimedOut {
				t.Fatalf("stored activations %+v", list)
			}
			if n := countKind(e.auditEvents(), "cpa.timed_out"); n != 1 {
				t.Fatalf("%d timeout events, want 1", n)
			}
		})
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

// A provisional base whose lifecycle state is missing, damaged or
// inconsistent is never served, and nothing else is either.
func TestCorruptLifecycleStateAdmitsNothing(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, db *sql.DB)
		reason  string
	}{
		{"rollback target missing", func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `PRAGMA foreign_keys = OFF`, `DELETE FROM revision WHERE id = 1`)
		}, "foreign key check failed"},
		{"rollback target altered", func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `UPDATE revision SET document = CAST('{"schema_version":4}' AS BLOB) WHERE id = 1`)
		}, "rollback target revision 1"},
		{"provisional pointer cleared", func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `UPDATE base SET provisional = NULL, deadline = NULL`)
		}, "disagree"},
		{"deadlines disagree", func(t *testing.T, db *sql.DB) {
			// The guard is removed and put back unchanged, as a damaged row
			// written by something other than the gateway would look.
			var trigger string
			if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name = 'base_provisional'`).Scan(&trigger); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, `DROP TRIGGER base_provisional`, `UPDATE base SET deadline = deadline + 3600000`, trigger)
		}, "disagree"},
		{"lifecycle guard removed", func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `DROP TRIGGER activation_final`)
		}, "schema differs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			candidate := e.cpaCandidate()
			seed(t, e.database, e.protectedPolicy(), seedOptions{provisional: &candidate, deadline: time.Now().Add(time.Hour)})
			db, err := sql.Open("sqlite", databaseURI(e.database))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			tc.prepare(t, db)
			db.Close()

			e.start(binaries.plain)
			if health := e.health(); health != "unavailable" {
				t.Fatalf("health %q", health)
			}
			if reason := e.unavailableReason(); !strings.Contains(reason, tc.reason) {
				t.Fatalf("recorded reason %q, want %q", reason, tc.reason)
			}
			bobToken := "Bearer " + issuer.tokenFor(t, "bob")
			for _, probe := range []struct{ method, path string }{{"GET", "/b/x"}, {"POST", "/a/item"}, {"GET", "/a/x"}} {
				if code, _ := e.request(probe.method, probe.path, "application/json", `{}`, "Authorization", bobToken); code != http.StatusServiceUnavailable {
					t.Fatalf("%s %s: %d, want 503", probe.method, probe.path, code)
				}
			}
			e.assertReceipts(0, 0, 0)
			for _, action := range []string{"confirm", "rollback"} {
				code, _, _ := e.harnessCall(http.MethodPost, "/admin/provisional/"+action, `{"revision":2}`,
					"If-Match", `"r1"`, "Idempotency-Key", "corrupt-state-"+action)
				if code != http.StatusServiceUnavailable {
					t.Fatalf("%s on corrupt state: %d", action, code)
				}
			}
		})
	}
}

func mustExec(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
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

func corrupt(t *testing.T, path string) {
	t.Helper()
	db, _ := sql.Open("sqlite", databaseURI(path))
	db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	db.Close()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteAt([]byte(strings.Repeat("\xff", 4000)), 100); err != nil {
		t.Fatal(err)
	}
}
