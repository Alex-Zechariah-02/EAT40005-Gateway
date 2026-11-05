// Migration of databases holding overrides.

package gateway

import (
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// A schema version 2 database, including one with a provisional base, is
// served only after the migrate command; afterwards overrides can be used.
func TestMigrateFromVersionTwo(t *testing.T) {
	e := newEnv(t)
	candidate := e.upstreamCandidate()
	seed(t, e.database, e.protectedPolicy(), seedOptions{version: 2, provisional: &candidate, deadline: time.Now().Add(time.Hour)})
	e.start(binaries.plain)
	if reason := e.unavailableReason(); !strings.Contains(reason, "schema version 2") || !strings.Contains(reason, "migrate") {
		t.Fatalf("reason %q", reason)
	}
	e.stop()
	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code != 0 || !strings.Contains(out, fmt.Sprintf("from schema version 2 to %d", store.DatabaseVersion)) {
		t.Fatalf("migrate exited %d: %s", code, out)
	}
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.Provisional != 2 || s.Confirmed != 1 {
		t.Fatalf("status after migration %+v", s)
	}
	if code, outcome, body := e.createOverride(harness, operationContainment("read-b", 60), `"r1"`, "migrated-create-01"); code != http.StatusConflict || outcome.Reason != "PROVISIONAL_ACTIVE" {
		t.Fatalf("creation while provisional: %d %s", code, body)
	}
	if code, _, body := e.resolve(harness, "rollback", 2, `"r1"`, "migrated-rollback-01"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}
	e.mustOverride(harness, operationContainment("read-b", 60), `"r1"`, "migrated-create-02")
	if e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("override after migration not enforced")
	}
}

// version3Grant is the request that created the active override written by
// writeVersion3Overrides.
var version3Grant = permitOverride(store.OverrideException, "READER", "write-a", 3600)

// writeVersion3Overrides adds to a seeded database the override history a
// schema version 3 gateway records: override 1 revoked, 2 expired, 3
// quarantined by an earlier restore and 4 active, each with its committed,
// published and audited transitions. The revocation and the active override's
// creation carry the digests of their Admin API requests, so they can be
// replayed.
func writeVersion3Overrides(t *testing.T, path string) {
	t.Helper()
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	db := inspect(t, path)
	transition := `INSERT INTO transition (id, kind, actor, target, expected, digest, outcome, reason, committed_at, published_at, published_by, audited_at)
		VALUES (?, ?, ?, ?, ?, ?, 'committed', '', ?, ?, 'publisher', ?)`
	transitions := []struct {
		id, kind, actor, target string
		expected                any
		digest                  string
		at                      int64
	}{
		{"admin:harness:version3-grant-00001", "tpo.create", harnessAdmin, "override", 1, "version3-grant", ms(-2 * time.Hour)},
		{"admin:harness:version3-revoke-0001", "tpo.revoke", harnessAdmin, "override 1", nil, requestDigest("tpo.revoke", "override 1", sql.NullInt64{}, nil), ms(-119 * time.Minute)},
		{"admin:harness:version3-close-00001", "tpo.create", harnessAdmin, "override", 1, "version3-close", ms(-2 * time.Hour)},
		{"auto-expire:2", "tpo.expire", "controller", "override 2", nil, "auto-expire:2", ms(-119 * time.Minute)},
		{"admin:harness:version3-limit-00001", "tpo.create", harnessAdmin, "override", 1, "version3-limit", ms(-2 * time.Hour)},
		{"operator:restore:" + strings.Repeat("e", 32), "operator.restore", "operator", "older.db", nil, "version3-restore", ms(-118 * time.Minute)},
		{"admin:harness:version3-grant-00002", "tpo.create", harnessAdmin, "override", 1,
			requestDigest("tpo.create", "override", sql.NullInt64{Int64: 1, Valid: true}, version3Grant), ms(-time.Minute)},
	}
	for _, tr := range transitions {
		if _, err := db.Exec(transition, tr.id, tr.kind, tr.actor, tr.target, tr.expected, tr.digest, tr.at, tr.at, tr.at); err != nil {
			t.Fatalf("transition %s: %v", tr.id, err)
		}
	}
	override := `INSERT INTO override (id, type, control, operation, role, target, base, created_at, expires_at, actor, creation, status, resolved_at, resolved_by, resolution)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, 'harness', ?, ?, ?, ?, ?)`
	overrides := [][]any{
		{1, store.OverrideException, store.ControlPermit, "write-a", "READER", "permit:READER:write-a", ms(-2 * time.Hour), ms(-110 * time.Minute),
			"admin:harness:version3-grant-00001", store.OverrideRevoked, ms(-119 * time.Minute), harnessAdmin, "admin:harness:version3-revoke-0001"},
		{2, store.OverrideContainment, store.ControlOperation, "read-b", nil, "operation:read-b", ms(-2 * time.Hour), ms(-119 * time.Minute),
			"admin:harness:version3-close-00001", store.OverrideExpired, ms(-119 * time.Minute), "controller", "auto-expire:2"},
		{3, store.OverrideContainment, store.ControlPermit, "read-a", "READER", "permit:READER:read-a", ms(-2 * time.Hour), ms(time.Hour),
			"admin:harness:version3-limit-00001", store.OverrideQuarantined, ms(-118 * time.Minute), "operator", "operator:restore:" + strings.Repeat("e", 32)},
		{4, store.OverrideException, store.ControlPermit, "write-a", "READER", "permit:READER:write-a", ms(-time.Minute), ms(59 * time.Minute),
			"admin:harness:version3-grant-00002", store.OverrideActive, nil, nil, nil},
	}
	for _, o := range overrides {
		if _, err := db.Exec(override, o...); err != nil {
			t.Fatalf("override %v: %v", o[0], err)
		}
	}
	db.Close()
}

// A schema version 3 database, with overrides in every state, is served only
// after the migrate command. The migration keeps every override and every
// earlier record exactly as stored and produces exactly the schema an
// installation creates; a database held by another process, an interrupted
// migration and damaged or newer databases are left unchanged. Afterwards
// transitions recorded before the migration replay to their receipts, and the
// active override is still enforced and can be revoked.
func TestMigrateFromVersionThree(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{version: 3})
	writeVersion3Overrides(t, e.database)
	before := map[string][]string{}
	for _, table := range []string{"override", "revision", "base", "candidate", "administrator", "enrolment", "activation", "transition"} {
		before[table] = queryRows(t, e.database, `SELECT * FROM `+table+` ORDER BY rowid`)
	}
	expectVersion3 := func(stage string) {
		t.Helper()
		db := inspect(t, e.database)
		if err := store.CheckDatabaseSchema(db, 3); err != nil || count(t, db, `PRAGMA user_version`) != 3 {
			t.Fatalf("%s: not the version 3 schema: %v", stage, err)
		}
		db.Close()
		for table, rows := range before {
			if after := queryRows(t, e.database, `SELECT * FROM `+table+` ORDER BY rowid`); !slices.Equal(after, rows) {
				t.Fatalf("%s: table %s changed", stage, table)
			}
		}
	}

	e.start(binaries.plain)
	if reason := e.unavailableReason(); !strings.Contains(reason, "schema version 3") || !strings.Contains(reason, "migrate") {
		t.Fatalf("reason %q", reason)
	}
	e.stop()

	// The database is held as a running gateway holds its own.
	held, err := store.Connect(e.database, "WAL")
	if err != nil {
		t.Fatal(err)
	}
	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, "held by another gateway process") {
		t.Fatalf("migrate of a held database: %d %s", code, out)
	}
	held.Close()
	expectVersion3("after a refused migration")
	if out, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=migrate-before-commit"}, "migrate"); code != crashExit {
		t.Fatalf("interrupted migrate exited %d: %s", code, out)
	}
	expectVersion3("after an interrupted migration")

	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code != 0 || !strings.Contains(out, fmt.Sprintf("from schema version 3 to %d", store.DatabaseVersion)) {
		t.Fatalf("migrate exited %d: %s", code, out)
	}
	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, "cannot be migrated") {
		t.Fatalf("second migrate: %d %s", code, out)
	}
	installed := newEnv(t)
	if out, code := installed.runOperator(binaries.plain, nil, "install", "--admin", "root"); code != 0 {
		t.Fatalf("install exited %d: %s", code, out)
	}
	schema := `SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY type, name`
	if migrated, fresh := queryRows(t, e.database, schema), queryRows(t, installed.database, schema); !slices.Equal(migrated, fresh) {
		t.Fatalf("migrated schema differs from an installed one:\n%v\n%v", migrated, fresh)
	}
	for table, rows := range before {
		after := queryRows(t, e.database, `SELECT * FROM `+table+` ORDER BY rowid`)
		if table == "transition" {
			// The migration adds its own record and nothing else.
			if len(after) != len(rows)+1 || !slices.Equal(after[:len(rows)], rows) {
				t.Fatalf("transitions after the migration:\n%v", after)
			}
			continue
		}
		if !slices.Equal(after, rows) {
			t.Fatalf("table %s changed by the migration:\n%v\n%v", table, rows, after)
		}
	}
	for query, want := range map[string]int{
		`PRAGMA user_version`: store.DatabaseVersion,
		`SELECT COUNT(*) FROM transition WHERE kind = 'operator.migrate' AND outcome = 'committed' AND audited_at IS NOT NULL`: 1,
		`SELECT COUNT(*) FROM pragma_foreign_key_check`: 0,
	} {
		if n := queryInt(t, e.database, query); n != want {
			t.Fatalf("%s = %d, want %d", query, n, want)
		}
	}
	if check := queryString(t, e.database, `PRAGMA integrity_check`); check != "ok" {
		t.Fatalf("integrity check %q", check)
	}

	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.State != store.ModeNormal || !slices.Equal(s.Overrides, []int64{4}) {
		t.Fatalf("status after migration %+v", s)
	}
	code, replayed, body := e.revokeOverride(harness, 1, "version3-revoke-0001")
	if code != http.StatusOK || replayed.Transition != "admin:harness:version3-revoke-0001" || replayed.Override != 1 || replayed.Disposition != store.OverrideRevoked {
		t.Fatalf("replayed revocation: %d %s", code, body)
	}
	code, replayed, body = e.createOverride(harness, version3Grant, `"r1"`, "version3-grant-00002")
	if code != http.StatusCreated || replayed.Override != 4 || replayed.Disposition != "" {
		t.Fatalf("replayed creation: %d %s", code, body)
	}
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusOK {
		t.Fatalf("active override after migration: %d", code)
	}
	if code, ended, body := e.revokeOverride(harness, 4, "version4-revoke-0001"); code != http.StatusOK || ended.Disposition != store.OverrideRevoked {
		t.Fatalf("revocation after migration: %d %s", code, body)
	}
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("revoked override after migration: %d", code)
	}
	e.stop()
	if list := storedOverrides(t, e.database); len(list) != 4 || list[3].Status != store.OverrideRevoked || list[3].Resolution != "admin:harness:version4-revoke-0001" {
		t.Fatalf("overrides after the revocation %+v", list)
	}

	// Damaged or newer databases are refused unchanged. Damage is written as
	// something other than the gateway would write it: with foreign key or
	// CHECK enforcement off, and any guard in the way removed and put back
	// unchanged.
	withoutGuards := func(db *sql.DB, triggers []string, statements ...string) {
		saved := make([]string, len(triggers))
		for i, name := range triggers {
			if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name = ?`, name).Scan(&saved[i]); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, `DROP TRIGGER `+name)
		}
		mustExec(t, db, statements...)
		mustExec(t, db, saved...)
	}
	refusals := []struct {
		name    string
		version int
		damage  func(db *sql.DB)
		reason  string
	}{
		{"newer schema version", store.DatabaseVersion, func(db *sql.DB) {
			mustExec(t, db, fmt.Sprintf(`PRAGMA user_version = %d`, store.DatabaseVersion+1))
		}, fmt.Sprintf("schema version %d cannot be migrated", store.DatabaseVersion+1)},
		{"override naming a missing revision", 3, func(db *sql.DB) {
			withoutGuards(db, []string{"override_fixed"}, `PRAGMA foreign_keys = OFF`, `UPDATE override SET base = 99 WHERE id = 4`)
		}, "FOREIGN KEY constraint failed"},
		{"override outside its constraints", 3, func(db *sql.DB) {
			withoutGuards(db, []string{"override_final"}, `PRAGMA ignore_check_constraints = ON`, `UPDATE override SET resolved_by = NULL WHERE id = 1`)
		}, "CHECK constraint failed"},
		{"transition naming a missing revision", 3, func(db *sql.DB) {
			mustExec(t, db, `PRAGMA foreign_keys = OFF`, `UPDATE transition SET result_revision = 99 WHERE id = 'auto-expire:2'`)
		}, "foreign key check failed"},
		{"administrator outside its constraints", 3, func(db *sql.DB) {
			mustExec(t, db, `PRAGMA ignore_check_constraints = ON`, `UPDATE administrator SET permissions = 16`)
		}, "integrity check failed"},
	}
	for _, r := range refusals {
		other := newEnv(t)
		seed(t, other.database, other.protectedPolicy(), seedOptions{version: r.version})
		if r.version == 3 {
			writeVersion3Overrides(t, other.database)
		}
		db, err := sql.Open("sqlite", store.DatabaseURI(other.database))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		r.damage(db)
		db.Close()
		damaged := fileSHA256(t, other.database)
		if out, code := other.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, r.reason) {
			t.Errorf("%s: migrate exited %d: %s", r.name, code, out)
		}
		if fileSHA256(t, other.database) != damaged {
			t.Errorf("%s: a refused migration changed the database", r.name)
		}
	}
	damaged := newEnv(t)
	seed(t, damaged.database, damaged.protectedPolicy(), seedOptions{version: 3})
	corrupt(t, damaged.database)
	corrupted := fileSHA256(t, damaged.database)
	if out, code := damaged.runOperator(binaries.plain, nil, "migrate"); code == 0 || fileSHA256(t, damaged.database) != corrupted {
		t.Errorf("migrate of a corrupted file exited %d and changed it: %s", code, out)
	}
}
