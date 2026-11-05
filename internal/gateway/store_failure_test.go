// Untrusted databases, crashes inside commits and outcome failures.

package gateway

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Startup refuses a database it must not trust, admits
// nothing, and accepts no mutation.
func TestStartupRefusesUntrustedDatabase(t *testing.T) {
	cases := []struct {
		name   string
		change []string
		reason string
	}{
		{"extra index", []string{`CREATE INDEX extra ON revision (actor)`}, "schema differs"},
		{"added column", []string{`ALTER TABLE revision ADD COLUMN note TEXT`}, "schema differs"},
		{"dropped table", []string{`DROP TABLE enrolment`}, "schema differs"},
		{"newer schema version", []string{fmt.Sprintf(`PRAGMA user_version = %d`, store.DatabaseVersion+1)}, fmt.Sprintf("schema version %d", store.DatabaseVersion+1)},
		{"altered stored policy", []string{`UPDATE revision SET document = CAST('{"schema_version":4}' AS BLOB) WHERE id = 1`}, "checksum mismatch"},
		{"dangling confirmed base", []string{`PRAGMA foreign_keys = OFF`, `UPDATE base SET confirmed = 99`}, "foreign key check failed"},
		{"dangling revision parent", []string{`PRAGMA foreign_keys = OFF`, `UPDATE revision SET parent = 99 WHERE id = 1`}, "foreign key check failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			db, err := sql.Open("sqlite", store.DatabaseURI(e.database))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			for _, statement := range tc.change {
				if _, err := db.Exec(statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			db.Close()

			e.start(binaries.plain)
			if health := e.health(); health != "unavailable" {
				t.Fatalf("health %q", health)
			}
			if reason := e.unavailableReason(); !strings.Contains(reason, tc.reason) {
				t.Fatalf("reason %q, want %q", reason, tc.reason)
			}
			if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusServiceUnavailable {
				t.Fatalf("data plane: %d", code)
			}
			// No mutation is accepted, whether or not anyone can log in.
			code, _, _ := e.harnessCall(http.MethodPut, "/admin/blocked-accounts/alice", `{"reason":"contain"}`,
				"If-Match", `"r1"`, "Idempotency-Key", "untrusted-database-1")
			if code != http.StatusServiceUnavailable {
				t.Fatalf("membership change on an untrusted database: %d", code)
			}
			e.assertReceipts(0, 0, 0)
		})
	}
}

// A crash between the two halves of the commit transaction
// leaves neither half; the retried change then applies once.
func TestCrashInsideCommit(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=commit-partway")
	if code, body, _ := e.harnessCall("PUT", "/admin/blocked-accounts/alice", `{"reason":"contain"}`, "Idempotency-Key", "crash-inside-commit"); code != 0 {
		t.Fatalf("mutation answered %d: %s", code, body)
	}
	if code := e.waitExit(); code != crashExit {
		t.Fatalf("exit %d", code)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM revision`:                 1,
		`SELECT confirmed FROM base`:                    1,
		`SELECT count(*) FROM transition`:               0,
		`SELECT count(*) FROM blocked_account`:          0,
		`SELECT count(*) FROM control_result`:           0,
		`SELECT count(*) FROM pragma_foreign_key_check`: 0,
	} {
		if n := queryInt(t, e.database, query); n != want {
			t.Fatalf("%s: %d want %d", query, n, want)
		}
	}
	e.start(binaries.plain)
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 200 {
		t.Fatal("uncommitted block survived")
	}
	if code, body, _ := e.harnessCall("PUT", "/admin/blocked-accounts/alice", `{"reason":"contain"}`, "Idempotency-Key", "crash-inside-commit"); code != 200 {
		t.Fatalf("retry: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 403 {
		t.Fatal("committed retry did not contain account")
	}
	e.assertReceipts(1, 0, 0)
}

// When the outcome event of a committed change cannot be written,
// the reply says the change is committed with its outcome event pending,
// admission stops, and the next startup records the event from the database.
func TestOutcomeEventFailure(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=audit-fail-published")
	before := e.status()
	header := []string{"Idempotency-Key", "outcome-event-fails"}
	code, body, _ := e.harnessCall("PUT", "/admin/blocked-accounts/alice", `{"reason":"contain"}`, header...)
	if code != 503 {
		t.Fatalf("unacknowledged outcome: %d %s", code, body)
	}
	if status := e.status(); !status.AuditFailed || status.Generation != before.Generation {
		t.Fatalf("status %+v", status)
	}
	if code, _ := e.get("/a/x", bearer(t, "bob")...); code != 503 {
		t.Fatal("admitted with failed durable audit")
	}
	e.assertReceipts(0, 0, 0)
	if code, _, _ := e.harnessCall("PUT", "/admin/blocked-accounts/alice", `{"reason":"contain"}`, header...); code != 503 {
		t.Fatalf("unacknowledged replay: %d", code)
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id='admin:harness:outcome-event-fails' AND audited_at IS NULL`); n != 1 {
		t.Fatalf("pending outcome %d", n)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM blocked_account WHERE subject='alice'`); n != 1 {
		t.Fatal("committed containment was lost")
	}
	e.start(binaries.plain)
	count := 0
	for _, event := range e.auditEvents() {
		if event["kind"] == "account.block" && event["transition"] == "admin:harness:outcome-event-fails" {
			count++
			if event["reason"] != "reconciled" || event["actor"] != harnessAdmin {
				t.Fatalf("reconciled outcome %v", event)
			}
		}
	}
	if count != 1 {
		t.Fatalf("reconciled outcomes %d", count)
	}
	if code, body, _ := e.harnessCall("PUT", "/admin/blocked-accounts/alice", `{"reason":"contain"}`, header...); code != 200 {
		t.Fatalf("settled replay: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 403 {
		t.Fatal("reconciled block was lost")
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE audited_at IS NULL`); n != 0 {
		t.Fatalf("unaudited transitions %d", n)
	}
}

// A crash at each boundary of containment publication ends,
// after restart, in either the old or the new blocked state, and the
// administrator's retry with the same key settles it exactly once.
func TestMembershipChangeSurvivesCrash(t *testing.T) {
	for _, tc := range []struct {
		point       string
		committed   bool
		publishedBy string
	}{
		{"before-commit", false, "publisher"},
		{"after-commit", true, "reconciliation"},
		{"after-swap", true, "reconciliation"},
		{"after-receipt", true, "publisher"},
		{"after-outcome", true, "publisher"},
	} {
		t.Run(tc.point, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+tc.point)
			header := []string{"Idempotency-Key", "membership-crash-key"}
			if code, body, _ := e.harnessCall("PUT", "/admin/blocked-accounts/alice", `{"reason":"contain"}`, header...); code != 0 {
				t.Fatalf("change answered %d: %s", code, body)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit %d", code)
			}
			wantBlocked := 0
			if tc.committed {
				wantBlocked = 1
			}
			if n := queryInt(t, e.database, `SELECT count(*) FROM blocked_account`); n != wantBlocked {
				t.Fatalf("blocks %d want %d", n, wantBlocked)
			}
			if n := queryInt(t, e.database, `SELECT count(*) FROM revision`); n != 1 {
				t.Fatalf("policy changed: %d revisions", n)
			}
			e.start(binaries.plain)
			want := 200
			if tc.committed {
				want = 403
			}
			if code, _ := e.get("/a/x", bearer(t, "alice")...); code != want {
				t.Fatalf("after restart: %d want %d", code, want)
			}
			if code, body, _ := e.harnessCall("PUT", "/admin/blocked-accounts/alice", `{"reason":"contain"}`, header...); code != 200 {
				t.Fatalf("retry: %d %s", code, body)
			}
			_, body, _ := e.harnessCall("GET", "/admin/history/admin:"+harnessAdmin+":membership-crash-key", "")
			if history := fromJSON[store.HistoryEntry](t, body); history.PublishedBy != tc.publishedBy || history.AuditedAt == nil {
				t.Fatalf("history %s", body)
			}
			if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 403 {
				t.Fatal("retry not contained")
			}
			e.stop()
			if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id='admin:harness:membership-crash-key'`); n != 1 {
				t.Fatalf("repeated commit %d", n)
			}
			if n := countKind(e.auditEvents(), "account.block"); n != 1 {
				t.Fatalf("repeated outcome %d", n)
			}
		})
	}
}
