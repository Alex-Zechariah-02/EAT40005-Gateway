// Installed schema and database constraints.

package gateway

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
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

	plain, err := sql.Open("sqlite", store.DatabaseURI(e.database))
	if err != nil {
		t.Fatal(err)
	}
	plain.SetMaxOpenConns(1)
	if err := store.Verify(plain); err == nil || !strings.Contains(err.Error(), "foreign key enforcement is not active") {
		t.Fatalf("verify without foreign keys: %v", err)
	}
	plain.Close()

	db, err := store.Connect(e.database, "WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Verify(db); err != nil {
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
	db, err := store.Connect(e.database, "WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := store.Adopt(db)
	ctx := t.Context()
	now := time.Now()
	create := func(id int64, o store.Override) {
		t.Helper()
		o.ID, o.Base, o.CreatedAt, o.ExpiresAt, o.Actor = id, 1, now, now.Add(time.Hour), harnessAdmin
		o.Target = overrideTarget(o.Control, o.Operation, o.Role)
		creation := store.TransitionRecord{ID: fmt.Sprintf("create-%d", id), Kind: "tpo.create", Actor: harnessAdmin, Target: "override", Digest: "d"}
		if err := s.CommitOverride(ctx, creation, o, now); err != nil {
			t.Fatalf("creating override %d: %v", id, err)
		}
	}
	grant := store.Override{Type: store.OverrideException, Control: store.ControlPermit, Operation: "write-a", Role: "READER"}
	create(1, grant)
	create(2, store.Override{Type: store.OverrideContainment, Control: store.ControlOperation, Operation: "read-b"})
	create(3, store.Override{Type: store.OverrideContainment, Control: store.ControlOperation, Operation: "read-a"})
	revoke := store.TransitionRecord{ID: "revoke-1", Kind: "tpo.revoke", Actor: harnessAdmin, Target: "override 1", Digest: "d"}
	if err := s.CommitOverrideEnd(ctx, revoke, 1, store.OverrideRevoked, now); err != nil {
		t.Fatal(err)
	}
	end := `UPDATE override SET status = ?, resolved_at = 1, resolved_by = 'x', resolution = ? WHERE id = ?`
	refused := func(name, failure, query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err == nil || !strings.Contains(err.Error(), failure) {
			t.Errorf("%s: %v, want %q", name, err, failure)
		}
	}
	refused("expiry naming a revocation", "UNIQUE constraint failed: override.resolution", end, store.OverrideExpired, "revoke-1", 2)
	refused("quarantine naming a revocation", "a resolution is shared only by overrides quarantined together", end, store.OverrideQuarantined, "revoke-1", 2)

	restore := store.TransitionRecord{ID: "operator:restore:" + strings.Repeat("a", 32), Kind: "operator.restore", Actor: "operator", Target: "backup", Digest: "d"}
	if err := s.DeclareRecovery(ctx, restore, strings.Repeat("a", 32), now); err != nil {
		t.Fatalf("quarantining two active overrides: %v", err)
	}
	create(4, grant)
	refused("revocation naming a restore", "a resolution is shared only by overrides quarantined together", end, store.OverrideRevoked, restore.ID, 4)
	refused("revoked override changed", "exactly one final disposition", `UPDATE override SET status = 'expired' WHERE id = 1`)
	refused("quarantined override moved to another transition", "exactly one final disposition", `UPDATE override SET resolution = 'create-3' WHERE id = 2`)
	refused("override deleted", "history is kept", `DELETE FROM override WHERE id = 3`)
	again := store.TransitionRecord{ID: "revoke-2", Kind: "tpo.revoke", Actor: harnessAdmin, Target: "override 2", Digest: "d"}
	if err := s.CommitOverrideEnd(ctx, again, 2, store.OverrideRevoked, now); !errors.Is(err, store.ErrNotUpdated) {
		t.Errorf("revoking a quarantined override: %v", err)
	}

	statuses := map[int64]string{1: store.OverrideRevoked, 2: store.OverrideQuarantined, 3: store.OverrideQuarantined, 4: store.OverrideActive}
	resolutions := map[int64]string{1: "revoke-1", 2: restore.ID, 3: restore.ID, 4: ""}
	list, err := s.Overrides(ctx, `ORDER BY id`)
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
		o, found, err := s.OverrideOf(ctx, transition)
		if err != nil || found != (want != 0) || o.ID != want {
			t.Errorf("overrideOf(%s) = %d %v %v, want %d", transition, o.ID, found, err, want)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("%d foreign key violations", n)
	}
}
