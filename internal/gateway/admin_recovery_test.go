// Interrupted bootstrap, credential rotation and recovery.

package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// An interrupted bootstrap leaves either the pending enrolment or
// the committed administrator, never a partial one. An interrupted install
// leaves a database that neither startup nor install accepts.
func TestInterruptedBootstrap(t *testing.T) {
	for _, tc := range []struct {
		point     string
		committed bool
	}{
		{"enrol-before-commit", false},
		{"enrol-after-commit", true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			e := newEnv(t)
			out, _ := e.runOperator(binaries.plain, nil, "install", "--admin", "root")
			secret := enrolmentSecret(t, out)
			e.start(binaries.fault, "GATEWAY_FAULT="+tc.point)
			password := testPassword()
			if code, body := e.enrol("root", secret, password); code != 0 {
				t.Fatalf("enrolment answered %d instead of crashing: %s", code, body)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}

			admins := queryInt(t, e.database, `SELECT COUNT(*) FROM administrator`)
			pending := queryInt(t, e.database, `SELECT COUNT(*) FROM enrolment`)
			if tc.committed != (admins == 1) || tc.committed == (pending == 1) || admins+pending != 1 {
				t.Fatalf("after the crash: %d administrators, %d pending enrolments", admins, pending)
			}

			e.start(binaries.plain)
			if tc.committed {
				e.mustLogin("root", password)
				if code, _ := e.enrol("root", secret, testPassword()); code != http.StatusUnauthorized {
					t.Fatalf("consumed secret accepted after restart: %d", code)
				}
				// The outcome event lost in the crash is recorded at startup.
				events := e.eventsWhen(func(events []map[string]any) bool {
					for _, ev := range events {
						if ev["kind"] == "transition.committed" && ev["reason"] == "reconciled" {
							return true
						}
					}
					return false
				})
				found := false
				for _, ev := range events {
					id, _ := ev["transition"].(string)
					found = found || (ev["kind"] == "transition.committed" && ev["reason"] == "reconciled" && strings.HasPrefix(id, "enrol:"))
				}
				if !found {
					t.Fatal("no reconciled outcome event for the enrolment")
				}
				return
			}
			if _, code := e.login("root", password); code != http.StatusUnauthorized {
				t.Fatalf("uncommitted enrolment authenticates: %d", code)
			}
			if code, body := e.enrol("root", secret, password); code != http.StatusCreated {
				t.Fatalf("retry after the interrupted enrolment: %d %s", code, body)
			}
			e.mustLogin("root", password)
		})
	}

	t.Run("install-before-commit", func(t *testing.T) {
		e := newEnv(t)
		if _, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=install-before-commit"}, "install", "--admin", "root"); code != crashExit {
			t.Fatalf("install exit code %d", code)
		}
		if n := queryInt(t, e.database, `PRAGMA user_version`); n != 0 {
			t.Fatalf("interrupted install left schema version %d", n)
		}
		e.start(binaries.plain)
		if health := e.health(); health != "unavailable" {
			t.Fatalf("health %q", health)
		}
		// Depending on what reached the main file, it is empty or holds no schema.
		if reason := e.unavailableReason(); !strings.Contains(reason, "schema version 0") && !strings.Contains(reason, "missing or empty") {
			t.Fatalf("reason %q", reason)
		}
		e.stop()
		if out, code := e.runOperator(binaries.plain, nil, "install", "--admin", "root"); code == 0 {
			t.Fatalf("install over the interrupted file succeeded: %s", out)
		}
	})
}

// A password change and a local recovery enrolment replace the
// credential and end every session based on the old one.
func TestCredentialRotationAndRecovery(t *testing.T) {
	e, secret := installEnv(t, binaries.plain)
	password := testPassword()
	if code, body := e.enrol("root", secret, password); code != http.StatusCreated {
		t.Fatalf("enrolment: %d %s", code, body)
	}
	first, second := e.mustLogin("root", password), e.mustLogin("root", password)

	changed := testPassword()
	wrong := string(mustJSON(map[string]string{"current_password": "not-the-password", "new_password": changed}))
	if code, _, _ := e.as(first, http.MethodPut, "/admin/password", wrong); code != http.StatusUnauthorized {
		t.Fatalf("password change with a wrong current password: %d", code)
	}
	tooShort := string(mustJSON(map[string]string{"current_password": password, "new_password": "short"}))
	if code, _, _ := e.as(first, http.MethodPut, "/admin/password", tooShort); code != http.StatusBadRequest {
		t.Fatalf("short new password: %d", code)
	}
	change := string(mustJSON(map[string]string{"current_password": password, "new_password": changed}))
	if code, body, _ := e.as(first, http.MethodPut, "/admin/password", change); code != http.StatusNoContent {
		t.Fatalf("password change: %d %s", code, body)
	}
	for _, s := range []adminSession{first, second} {
		if code, _, _ := e.as(s, http.MethodGet, "/admin/session", ""); code != http.StatusUnauthorized {
			t.Fatalf("session after password change: %d", code)
		}
	}
	if _, code := e.login("root", password); code != http.StatusUnauthorized {
		t.Fatalf("old password after change: %d", code)
	}
	current := e.mustLogin("root", changed)

	// Local recovery: the operator command refuses to run beside the gateway.
	if out, code := e.runOperator(binaries.plain, nil, "enrol", "--admin", "root", "--permissions", "read"); code == 0 ||
		!strings.Contains(out, "held by another gateway process") {
		t.Fatalf("enrol while the gateway runs: %d %s", code, out)
	}
	e.stop()
	out, code := e.runOperator(binaries.plain, nil, "enrol", "--admin", "root", "--permissions", "read")
	if code != 0 {
		t.Fatalf("enrol exited %d: %s", code, out)
	}
	recovery := enrolmentSecret(t, out)
	e.start(binaries.plain)
	current = e.mustLogin("root", changed) // valid until the enrolment is consumed

	recovered := testPassword()
	if code, body := e.enrol("root", recovery, recovered); code != http.StatusCreated || !strings.Contains(body, `["read"]`) {
		t.Fatalf("recovery enrolment: %d %s", code, body)
	}
	if code, _, _ := e.as(current, http.MethodGet, "/admin/session", ""); code != http.StatusUnauthorized {
		t.Fatalf("session after recovery: %d", code)
	}
	if _, code := e.login("root", changed); code != http.StatusUnauthorized {
		t.Fatalf("replaced password after recovery: %d", code)
	}
	reader := e.mustLogin("root", recovered)
	if code, _, _ := e.as(reader, http.MethodGet, "/admin/status", ""); code != http.StatusOK {
		t.Fatalf("recovered administrator reads: %d", code)
	}
	if code, _, _ := e.as(reader, http.MethodPost, "/admin/candidates", `{"policy":{}}`, "If-Match", `"r0"`, "Idempotency-Key", "recovered-reader-1"); code != http.StatusForbidden {
		t.Fatalf("recovered read-only administrator stages a candidate: %d", code)
	}

	// An expired enrolment is refused.
	e.stop()
	out, _ = e.runOperator(binaries.plain, nil, "enrol", "--admin", "late", "--permissions", "read")
	late := enrolmentSecret(t, out)
	db := inspect(t, e.database)
	if _, err := db.Exec(`UPDATE enrolment SET expires_at = ? WHERE administrator = 'late'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.start(binaries.plain)
	if code, _ := e.enrol("late", late, testPassword()); code != http.StatusUnauthorized {
		t.Fatalf("expired enrolment: %d", code)
	}
	assertAbsent(t, e, password, changed, recovered, recovery, late)
}
