// Corrupt lifecycle state admits nothing.

package gateway

import (
	"database/sql"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

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
			db, err := sql.Open("sqlite", store.DatabaseURI(e.database))
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

func corrupt(t *testing.T, path string) {
	t.Helper()
	db, _ := sql.Open("sqlite", store.DatabaseURI(path))
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
