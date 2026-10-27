package gateway

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// An installed database has exactly the current schema: the eleven STRICT
// tables, their keys and constraints, the activation and override indexes and
// triggers, WAL journaling and user_version 5, and nothing else.
func TestInstalledSchema(t *testing.T) {
	e := newEnv(t)
	if out, code := e.runOperator(binaries.plain, nil, "install", "--admin", "root"); code != 0 {
		t.Fatalf("install exited %d: %s", code, out)
	}
	db := inspect(t, e.database)

	rows, err := db.Query(`SELECT name, strict FROM pragma_table_list WHERE schema = 'main' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		var strict int
		if err := rows.Scan(&name, &strict); err != nil {
			t.Fatal(err)
		}
		if strict != 1 {
			t.Errorf("table %s is not STRICT", name)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	want := []string{"activation", "administrator", "base", "blocked_account", "candidate", "control_result", "deployment", "enrolment", "override", "revision", "transition"}
	if !slices.Equal(tables, want) {
		t.Fatalf("tables %v, want %v", tables, want)
	}
	// Besides those SQLite creates for primary keys and UNIQUE, the only
	// indexes and triggers guard the provisional and override lifecycles.
	explicit, err := db.Query(`SELECT name FROM sqlite_schema WHERE type IN ('index', 'trigger', 'view') AND sql IS NOT NULL ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for explicit.Next() {
		var name string
		explicit.Scan(&name)
		names = append(names, name)
	}
	if err := explicit.Err(); err != nil {
		t.Fatal(err)
	}
	explicit.Close()
	if want := []string{"activation_final", "activation_fixed", "activation_kept", "activation_unresolved", "base_provisional",
		"override_active_target", "override_final", "override_fixed", "override_kept", "override_resolution", "override_shared"}; !slices.Equal(names, want) {
		t.Fatalf("explicit indexes and triggers %v, want %v", names, want)
	}
	// Of the override's UNIQUE constraints only creation remains on its
	// column; a resolution is unique only among revocations and expiries.
	if n := count(t, db, `SELECT COUNT(*) FROM pragma_index_list('override') WHERE origin = 'u'`); n != 1 {
		t.Fatalf("override has %d UNIQUE column constraints, want 1", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM pragma_index_info('sqlite_autoindex_override_1') WHERE name = 'creation'`); n != 1 {
		t.Fatal("the override UNIQUE column constraint is not on creation")
	}
	foreignKeys := map[string]int{"revision": 1, "base": 2, "candidate": 1, "transition": 2, "activation": 4, "override": 3, "blocked_account": 1, "control_result": 1}
	for table, want := range foreignKeys {
		if n := count(t, db, `SELECT COUNT(*) FROM pragma_foreign_key_list(?)`, table); n != want {
			t.Errorf("table %s has %d foreign keys, want %d", table, n, want)
		}
	}
	if n := count(t, db, `PRAGMA user_version`); n != 5 {
		t.Fatalf("user_version %d", n)
	}
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal mode %q %v", journal, err)
	}
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity check %q %v", check, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("%d foreign key violations", n)
	}
	// The install transition is recorded and audited.
	if n := count(t, db, `SELECT COUNT(*) FROM transition WHERE kind = 'operator.install' AND outcome = 'committed' AND audited_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d audited install transitions", n)
	}
}

// verify refuses a connection without foreign key enforcement, and
// the schema's constraints reject invalid rows on a production connection.
func TestConnectionAndConstraints(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})

	plain, err := sql.Open("sqlite", "file:"+e.database)
	if err != nil {
		t.Fatal(err)
	}
	plain.SetMaxOpenConns(1)
	if err := verify(plain); err == nil || !strings.Contains(err.Error(), "foreign key enforcement is not active") {
		t.Fatalf("verify without foreign keys: %v", err)
	}
	plain.Close()

	db, err := connect(e.database, "WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := verify(db); err != nil {
		t.Fatalf("verify on a production connection: %v", err)
	}
	violations := []struct {
		name, query, failure string
		args                 []any
	}{
		{"revision with an unknown parent", `INSERT INTO revision VALUES (2, 99, 'membership', x'7b7d', ?, 'x', 0)`, "FOREIGN KEY", []any{strings.Repeat("0", 64)}},
		{"confirmed base without its revision", `UPDATE base SET confirmed = 99`, "FOREIGN KEY", nil},
		{"transition naming an unknown candidate", `INSERT INTO transition (id, kind, actor, target, digest, outcome, reason, result_candidate, committed_at)
			VALUES ('t', 'k', 'a', 't', 'd', 'committed', '', 5, 0)`, "FOREIGN KEY", nil},
		{"second deployment row", `INSERT INTO deployment VALUES (2, ?, 'normal', NULL, 0)`, "CHECK", []any{strings.Repeat("a", 32)}},
		{"unknown mode", `UPDATE deployment SET mode = 'open'`, "CHECK", nil},
		{"recovery without an episode", `UPDATE deployment SET mode = 'recovery_required', recovery_episode = NULL`, "CHECK", nil},
		{"unknown revision origin", `INSERT INTO revision VALUES (2, 1, 'direct', x'7b7d', ?, 'x', 0)`, "CHECK", []any{strings.Repeat("0", 64)}},
		{"deadline without a provisional base", `UPDATE base SET deadline = 5`, "CHECK", nil},
		{"permissions outside the four bits", `UPDATE administrator SET permissions = 16`, "CHECK", nil},
		{"plaintext password", `UPDATE administrator SET password = 'hunter2hunter2hunter2'`, "CHECK", nil},
		{"rejected transition with a result", `INSERT INTO transition (id, kind, actor, target, digest, outcome, reason, result_revision, committed_at)
			VALUES ('t', 'k', 'a', 't', 'd', 'rejected', 'STALE', 1, 0)`, "CHECK", nil},
		{"committed transition with a reason", `INSERT INTO transition (id, kind, actor, target, digest, outcome, reason, committed_at)
			VALUES ('t', 'k', 'a', 't', 'd', 'committed', 'STALE', 0)`, "CHECK", nil},
		{"duplicate administrator", `INSERT INTO administrator VALUES (?, 1, ?, 'active', 1, 0)`, "UNIQUE", []any{harnessAdmin, harnessHash()}},
		{"duplicate revision", `INSERT INTO revision SELECT * FROM revision WHERE id = 1`, "UNIQUE", nil},
		{"shared enrolment secret", `INSERT INTO enrolment VALUES ('a', ?, 1, 0, 0), ('b', ?, 1, 0, 0)`, "UNIQUE", []any{strings.Repeat("f", 64), strings.Repeat("f", 64)}},
	}
	for _, v := range violations {
		_, err := db.Exec(v.query, v.args...)
		if err == nil || !strings.Contains(err.Error(), v.failure+" constraint failed") {
			t.Errorf("%s: %v, want a %s constraint failure", v.name, err, v.failure)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM revision`); n != 1 {
		t.Fatalf("%d revisions after the refused statements", n)
	}
}

// One restore transition resolves every override it quarantines. Any other
// resolution ends exactly one override, a quarantine never shares a
// resolution with a revocation or expiry, a final disposition never changes,
// and a refused change leaves nothing behind. overrideOf finds the one
// override a temporary-override transition created or ended.
func TestOverrideResolutionConstraints(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	db, err := connect(e.database, "WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &store{db: db}
	ctx := t.Context()
	now := time.Now()
	create := func(id int64, o override) {
		t.Helper()
		o.ID, o.Base, o.CreatedAt, o.ExpiresAt, o.Actor = id, 1, now, now.Add(time.Hour), harnessAdmin
		o.Target = overrideTarget(o.Control, o.Operation, o.Role)
		creation := transitionRecord{ID: fmt.Sprintf("create-%d", id), Kind: "tpo.create", Actor: harnessAdmin, Target: "override", Digest: "d"}
		if err := s.commitOverride(ctx, creation, o, now); err != nil {
			t.Fatalf("creating override %d: %v", id, err)
		}
	}
	grant := override{Type: overrideException, Control: controlPermit, Operation: "write-a", Role: "READER"}
	create(1, grant)
	create(2, override{Type: overrideContainment, Control: controlOperation, Operation: "read-b"})
	create(3, override{Type: overrideContainment, Control: controlOperation, Operation: "read-a"})
	revoke := transitionRecord{ID: "revoke-1", Kind: "tpo.revoke", Actor: harnessAdmin, Target: "override 1", Digest: "d"}
	if err := s.commitOverrideEnd(ctx, revoke, 1, overrideRevoked, now); err != nil {
		t.Fatal(err)
	}
	end := `UPDATE override SET status = ?, resolved_at = 1, resolved_by = 'x', resolution = ? WHERE id = ?`
	refused := func(name, failure, query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err == nil || !strings.Contains(err.Error(), failure) {
			t.Errorf("%s: %v, want %q", name, err, failure)
		}
	}
	refused("expiry naming a revocation", "UNIQUE constraint failed: override.resolution", end, overrideExpired, "revoke-1", 2)
	refused("quarantine naming a revocation", "a resolution is shared only by overrides quarantined together", end, overrideQuarantined, "revoke-1", 2)

	restore := transitionRecord{ID: "operator:restore:" + strings.Repeat("a", 32), Kind: "operator.restore", Actor: "operator", Target: "backup", Digest: "d"}
	if err := s.declareRecovery(ctx, restore, strings.Repeat("a", 32), now); err != nil {
		t.Fatalf("quarantining two active overrides: %v", err)
	}
	create(4, grant)
	refused("revocation naming a restore", "a resolution is shared only by overrides quarantined together", end, overrideRevoked, restore.ID, 4)
	refused("revoked override changed", "exactly one final disposition", `UPDATE override SET status = 'expired' WHERE id = 1`)
	refused("quarantined override moved to another transition", "exactly one final disposition", `UPDATE override SET resolution = 'create-3' WHERE id = 2`)
	refused("override deleted", "history is kept", `DELETE FROM override WHERE id = 3`)
	again := transitionRecord{ID: "revoke-2", Kind: "tpo.revoke", Actor: harnessAdmin, Target: "override 2", Digest: "d"}
	if err := s.commitOverrideEnd(ctx, again, 2, overrideRevoked, now); !errors.Is(err, errNotUpdated) {
		t.Errorf("revoking a quarantined override: %v", err)
	}

	statuses := map[int64]string{1: overrideRevoked, 2: overrideQuarantined, 3: overrideQuarantined, 4: overrideActive}
	resolutions := map[int64]string{1: "revoke-1", 2: restore.ID, 3: restore.ID, 4: ""}
	list, err := s.overrides(ctx, `ORDER BY id`)
	if err != nil || len(list) != 4 {
		t.Fatalf("overrides %+v %v", list, err)
	}
	for _, o := range list {
		if o.Status != statuses[o.ID] || o.Resolution != resolutions[o.ID] {
			t.Errorf("override %d: %s by %q", o.ID, o.Status, o.Resolution)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM transition WHERE id = 'revoke-2'`); n != 0 {
		t.Error("the refused revocation left its transition")
	}
	for transition, want := range map[string]int64{"create-1": 1, "revoke-1": 1, "create-2": 2, "create-4": 4, restore.ID: 0} {
		o, found, err := s.overrideOf(ctx, transition)
		if err != nil || found != (want != 0) || o.ID != want {
			t.Errorf("overrideOf(%s) = %d %v %v, want %d", transition, o.ID, found, err, want)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("%d foreign key violations", n)
	}
}

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
		{"newer schema version", []string{fmt.Sprintf(`PRAGMA user_version = %d`, databaseVersion+1)}, fmt.Sprintf("schema version %d", databaseVersion+1)},
		{"altered stored policy", []string{`UPDATE revision SET document = CAST('{"schema_version":4}' AS BLOB) WHERE id = 1`}, "checksum mismatch"},
		{"dangling confirmed base", []string{`PRAGMA foreign_keys = OFF`, `UPDATE base SET confirmed = 99`}, "reading revision 99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			db, err := sql.Open("sqlite", "file:"+e.database)
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
			if history := fromJSON[historyEntry](t, body); history.PublishedBy != tc.publishedBy || history.AuditedAt == nil {
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

// Concurrent changes against one revision and lock contention. One
// change applies, every other is refused as stale without waiting, and an
// operator command against the running gateway's database fails at once.
func TestConcurrencyAndLockContention(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	const writers = 20
	code, staged, body := e.stage(harness, e.protectedPolicy(), `"r1"`, "concurrent-base-candidate")
	if code != http.StatusCreated {
		t.Fatalf("stage: %d %s", code, body)
	}
	codes := make([]int, writers)
	outcomes := make([]Outcome, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			codes[i], outcomes[i], _ = e.activate(harness, staged.Candidate.candidateRecord, 60, `"r1"`, fmt.Sprintf("concurrent-activation-%04d", i))
		})
	}
	wg.Wait()
	winner := -1
	conflicts := 0
	for i, code := range codes {
		switch code {
		case http.StatusOK:
			if winner >= 0 {
				t.Fatal("two activations won")
			}
			winner = i
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("activation: %d %+v", code, outcomes[i])
		}
	}
	if winner < 0 || conflicts != writers-1 {
		t.Fatalf("winner %d conflicts %d", winner, conflicts)
	}
	if code, _, body := e.resolve(harness, "confirm", outcomes[winner].Provisional, `"r1"`, "concurrent-base-confirm"); code != http.StatusOK {
		t.Fatalf("confirm: %d %s", code, body)
	}
	code, body, header := e.as(harness, "GET", "/admin/policy", "")
	revision := fromJSON[revisionInfo](t, body)
	if code != http.StatusOK || header.Get("ETag") != `"r2"` || revision.ID != 2 {
		t.Fatalf("policy after activation race: %d %s", code, body)
	}

	// The running gateway is the audit log's only writer. An operator command
	// refused by the lock must not touch it, even when the log ends in a
	// record the gateway is still writing.
	e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "admin.read") >= 1 })
	partial := `{"schema":4,"kind":"request.admitted","id":"in-progress`
	audit, err := os.OpenFile(e.auditLog, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	audit.WriteString(partial)
	audit.Close()
	for _, command := range [][]string{
		{"enrol", "--admin", "late", "--permissions", "read"},
		{"backup", "--output", filepath.Join(e.dir, "busy.db")},
	} {
		started := time.Now()
		out, code := e.runOperator(binaries.plain, nil, command[0], command[1:]...)
		if code == 0 || !strings.Contains(out, "held by another gateway process") {
			t.Errorf("%s while the gateway runs: %d %s", command[0], code, out)
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Errorf("%s waited %s for the lock", command[0], elapsed)
		}
	}
	data, err := os.ReadFile(e.auditLog)
	if err != nil || !strings.HasSuffix(string(data), "\n"+partial) {
		t.Errorf("a refused operator command wrote to the running gateway's audit log: %q", data[max(0, len(data)-200):])
	}
	e.stop()
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM revision`: 2,
		`SELECT COUNT(*) FROM transition WHERE kind = 'cpa.activate' AND outcome = 'rejected'`: writers - 1,
		`SELECT COUNT(*) FROM enrolment`: 0,
	} {
		if n := queryInt(t, e.database, query); n != want {
			t.Errorf("%s = %d, want %d", query, n, want)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(e.dir, "busy.db*")); len(matches) != 0 {
		t.Errorf("a refused backup left files: %v", matches)
	}
}

// A refused mutation is recorded as rejected with its reason and
// actor, and nothing it asked for is stored.
func TestRejectedMutationsAreRecorded(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	_, readerPassword := e.addAdmin(binaries.plain, "reader", "read,read_history")
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	reader := e.mustLogin("reader", readerPassword)

	// Stale and invalid: recorded in history. Unauthorised: refused before
	// the controller, recorded as an audit denial only.
	e.stage(harness, e.protectedPolicy(), `"r7"`, "rejected-stale-01")
	invalid := e.protectedPolicy()
	invalid.Permits = append(invalid.Permits, Permit{Role: "ROOT", Operation: "read-a"})
	e.stage(harness, invalid, `"r1"`, "rejected-invalid-01")
	if code, _, _ := e.as(reader, http.MethodPut, "/admin/blocked-accounts/carol", `{"reason":"contain"}`,
		"If-Match", `"r1"`, "Idempotency-Key", "rejected-unauthorised"); code != http.StatusForbidden {
		t.Fatalf("reader's change: %d", code)
	}

	code, body, _ := e.as(reader, http.MethodGet, "/admin/history", "")
	history := fromJSON[struct {
		Items []historyEntry `json:"items"`
	}](t, body)
	if code != http.StatusOK {
		t.Fatalf("history: %d %s", code, body)
	}
	reasons := map[string]string{}
	for _, entry := range history.Items {
		if entry.Kind == "candidate.create" {
			if entry.Outcome != "rejected" || entry.Actor != harnessAdmin || entry.Revision != 0 || entry.PublishedAt != nil {
				t.Errorf("history entry %+v", entry)
			}
			reasons[entry.ID] = entry.Reason
		}
	}
	want := map[string]string{"admin:harness:rejected-stale-01": "PRECONDITION_FAILED", "admin:harness:rejected-invalid-01": "UNKNOWN_REFERENCE"}
	if fmt.Sprint(reasons) != fmt.Sprint(want) {
		t.Fatalf("recorded refusals %v, want %v", reasons, want)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "transition.rejected") >= 2 })
	denied := false
	for _, ev := range events {
		if ev["kind"] == "transition.rejected" && ev["actor"] != harnessAdmin {
			t.Errorf("rejection event %v", ev)
		}
		denied = denied || (ev["kind"] == "admin.denied" && ev["actor"] == "reader" && ev["reason"] == "permission")
	}
	if !denied {
		t.Error("the reader's refused change was not recorded as a denial")
	}
	if s := e.status(); s.Revision != 1 {
		t.Fatalf("refusals changed the revision to %d", s.Revision)
	}
	encoded, _ := json.Marshal(history)
	if strings.Contains(string(encoded), "rejected-unauthorised") {
		t.Fatal("an unauthorised request reached the controller's history")
	}
}

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
	if code != 0 || !strings.Contains(out, "from schema version 1 to 5") {
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
	if s := e.status(); s.State != modeNormal || s.Revision != 1 || s.Confirmed != 1 {
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
