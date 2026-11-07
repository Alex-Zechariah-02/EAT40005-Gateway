// Migration from the first database version.

package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// A database of the previous schema version is not served until the
// local migrate command upgrades it. The command refuses a database held by a
// running gateway, a database whose schema is not exactly its version's, and
// one holding a provisional base the new schema could not describe; an
// interrupted migration leaves the earlier version intact.
func TestMigrateFromVersionOne(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{version: 1})
	e.start(binaries.plain)
	if health := e.health(); health != "unavailable" {
		t.Fatalf("health %q on a version 1 database", health)
	}
	if reason := e.unavailableReason(); !strings.Contains(reason, "schema version 1") || !strings.Contains(reason, "migrate") {
		t.Fatalf("reason %q", reason)
	}
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusServiceUnavailable {
		t.Fatalf("data plane on a version 1 database: %d", code)
	}
	// A gateway that refused the database holds no lock on it, so, as for
	// every operator command, the operator stops it first.
	e.stop()

	if out, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=migrate-before-commit"}, "migrate"); code != crashExit {
		t.Fatalf("interrupted migrate exited %d: %s", code, out)
	}
	if n := queryInt(t, e.database, `PRAGMA user_version`); n != 1 {
		t.Fatalf("user_version %d after an interrupted migration", n)
	}
	out, code := e.runOperator(binaries.plain, nil, "migrate")
	if code != 0 || !strings.Contains(out, fmt.Sprintf("from schema version 1 to %d", store.DatabaseVersion)) {
		t.Fatalf("migrate exited %d: %s", code, out)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'operator.migrate' AND outcome = 'committed' AND audited_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d recorded migrations", n)
	}
	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, "cannot be migrated") {
		t.Fatalf("second migrate: %d %s", code, out)
	}

	// The migrated database serves its confirmed base and supports the
	// lifecycle.
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.State != store.ModeNormal || s.Revision != 1 || s.Confirmed != 1 {
		t.Fatalf("status after migration: %+v", s)
	}
	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, "held by another gateway process") {
		t.Fatalf("migrate while a gateway holds the database: %d %s", code, out)
	}
	e.provisionalFrom(harness, e.cpaCandidate(), 1, 60, "after-migration-01")
	e.expectEnforced(true, "")
	if code, _, body := e.resolve(harness, "rollback", 2, `"r1"`, "after-migration-rollback"); code != http.StatusOK {
		t.Fatalf("rollback after migration: %d %s", code, body)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "transition.committed") > 0 })
	if findTransition(events, "transition.committed", queryLatestMigration(t, e)) == nil {
		t.Fatal("no committed migration event")
	}

	refusals := []struct {
		name    string
		prepare func(path string)
		reason  string
	}{
		{"provisional base", func(path string) {
			candidate := e.cpaCandidate()
			seed(t, path, e.protectedPolicy(), seedOptions{version: 1, provisional: &candidate, deadline: time.Now().Add(time.Hour)})
		}, "provisional base"},
		{"altered schema", func(path string) {
			seed(t, path, e.protectedPolicy(), seedOptions{version: 1})
			db := inspect(t, path)
			mustExec(t, db, `CREATE INDEX extra ON revision (actor)`)
			db.Close()
		}, "schema differs"},
	}
	for _, r := range refusals {
		other := newEnv(t)
		r.prepare(other.database)
		before := fileSHA256(t, other.database)
		if out, code := other.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, r.reason) {
			t.Errorf("%s: migrate exited %d: %s", r.name, code, out)
		}
		if after := fileSHA256(t, other.database); after != before {
			t.Errorf("%s: a refused migration changed the database", r.name)
		}
	}
}

// queryLatestMigration returns the identity of the recorded migration, read
// through the running gateway's history.
func queryLatestMigration(t *testing.T, e *env) string {
	t.Helper()
	for _, h := range e.historyItems(e.mustLogin(harnessAdmin, harnessPassword)) {
		if h.Kind == "operator.migrate" {
			return h.ID
		}
	}
	t.Fatal("no migration in the history")
	return ""
}
