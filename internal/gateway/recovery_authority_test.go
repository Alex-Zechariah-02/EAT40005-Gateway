// Expired or unavailable authority admits nothing.

package gateway

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

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
			if _, err := db.Exec(`UPDATE revision SET document = ?, checksum = ? WHERE id = 1`, earlier, store.DocumentChecksum(earlier)); err != nil {
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
			if _, err := db.Exec(`UPDATE revision SET document = ?, checksum = ? WHERE id = 1`, earlier, store.DocumentChecksum(earlier)); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "SCHEMA_VERSION_UNSUPPORTED"},
		{"recovery required", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{mode: store.ModeRecovery})
		}, store.ModeRecovery},
		{"bootstrap locked", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{mode: store.ModeBootstrap})
		}, store.ModeBootstrap},
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
			if tc.state == store.ModeRecovery || tc.state == store.ModeBootstrap {
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
