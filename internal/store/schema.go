// Database schema migrations and schema verification.

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// migrations holds the database schema, one entry per schema version. Install
// applies all of them; startup accepts only a database at the latest version
// whose schema is exactly what they create.
var migrations = []string{`
CREATE TABLE deployment (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	installation TEXT NOT NULL CHECK (length(installation) = 32),
	mode TEXT NOT NULL CHECK (mode IN ('bootstrap_locked', 'normal', 'recovery_required')),
	recovery_episode TEXT CHECK (recovery_episode IS NULL OR length(recovery_episode) = 32),
	high_water INTEGER NOT NULL,
	CHECK (mode <> 'recovery_required' OR recovery_episode IS NOT NULL)
) STRICT;
CREATE TABLE revision (
	id INTEGER PRIMARY KEY,
	parent INTEGER REFERENCES revision (id),
	origin TEXT NOT NULL CHECK (origin IN ('genesis', 'recovery_base', 'membership', 'publish')),
	document BLOB NOT NULL CHECK (length(document) BETWEEN 1 AND 65536),
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	actor TEXT NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;
CREATE TABLE base (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	confirmed INTEGER REFERENCES revision (id),
	provisional INTEGER REFERENCES revision (id),
	deadline INTEGER,
	CHECK ((provisional IS NULL) = (deadline IS NULL)),
	CHECK (provisional IS NULL OR confirmed IS NOT NULL)
) STRICT;
CREATE TABLE candidate (
	id INTEGER PRIMARY KEY,
	parent INTEGER REFERENCES revision (id),
	document BLOB NOT NULL CHECK (length(document) BETWEEN 1 AND 65536),
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	deny_all INTEGER NOT NULL CHECK (deny_all IN (0, 1)),
	status TEXT NOT NULL CHECK (status IN ('validated', 'withdrawn', 'established', 'quarantined')),
	actor TEXT NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;
CREATE TABLE administrator (
	name TEXT PRIMARY KEY CHECK (length(name) BETWEEN 1 AND 64),
	permissions INTEGER NOT NULL CHECK (permissions BETWEEN 1 AND 15),
	password TEXT NOT NULL CHECK (password LIKE '$argon2id$%'),
	status TEXT NOT NULL CHECK (status IN ('active', 'quarantined')),
	credential INTEGER NOT NULL CHECK (credential >= 1),
	updated_at INTEGER NOT NULL
) STRICT;
CREATE TABLE enrolment (
	administrator TEXT PRIMARY KEY CHECK (length(administrator) BETWEEN 1 AND 64),
	secret TEXT NOT NULL UNIQUE CHECK (length(secret) = 64),
	permissions INTEGER NOT NULL CHECK (permissions BETWEEN 1 AND 15),
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;
CREATE TABLE transition (
	id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 256),
	kind TEXT NOT NULL,
	actor TEXT NOT NULL,
	target TEXT NOT NULL,
	expected INTEGER,
	digest TEXT NOT NULL,
	outcome TEXT NOT NULL CHECK (outcome IN ('committed', 'rejected')),
	reason TEXT NOT NULL,
	result_revision INTEGER REFERENCES revision (id),
	result_candidate INTEGER REFERENCES candidate (id),
	committed_at INTEGER NOT NULL,
	published_at INTEGER,
	published_by TEXT,
	audited_at INTEGER,
	CHECK ((outcome = 'committed') = (reason = '')),
	CHECK (outcome = 'committed' OR (result_revision IS NULL AND result_candidate IS NULL AND published_at IS NULL))
) STRICT;
`, `
CREATE TABLE activation (
	revision INTEGER PRIMARY KEY REFERENCES revision (id),
	candidate INTEGER NOT NULL UNIQUE REFERENCES candidate (id),
	rollback_target INTEGER NOT NULL REFERENCES revision (id),
	deadline INTEGER NOT NULL,
	activated_at INTEGER NOT NULL,
	actor TEXT NOT NULL,
	disposition TEXT CHECK (disposition IN ('confirmed', 'rolled_back', 'timed_out', 'quarantined')),
	resolved_at INTEGER,
	resolved_by TEXT,
	resolution TEXT REFERENCES transition (id) DEFERRABLE INITIALLY DEFERRED,
	CHECK (rollback_target < revision),
	CHECK (deadline > activated_at),
	CHECK ((disposition IS NULL) = (resolved_at IS NULL)),
	CHECK ((disposition IS NULL) = (resolved_by IS NULL)),
	CHECK ((disposition IS NULL) = (resolution IS NULL))
) STRICT;
CREATE UNIQUE INDEX activation_unresolved ON activation ((disposition IS NULL)) WHERE disposition IS NULL;
CREATE TRIGGER activation_fixed BEFORE UPDATE OF revision, candidate, rollback_target, deadline, activated_at, actor ON activation
BEGIN SELECT RAISE(ABORT, 'an activation record cannot change'); END;
CREATE TRIGGER activation_final BEFORE UPDATE ON activation WHEN OLD.disposition IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'an activation has exactly one final disposition'); END;
CREATE TRIGGER activation_kept BEFORE DELETE ON activation
BEGIN SELECT RAISE(ABORT, 'activation history is kept'); END;
CREATE TRIGGER base_provisional BEFORE UPDATE ON base WHEN NEW.provisional IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM activation WHERE revision = NEW.provisional AND deadline = NEW.deadline
		AND rollback_target = NEW.confirmed AND disposition IS NULL)
BEGIN SELECT RAISE(ABORT, 'a provisional base needs its unresolved activation'); END;
`, `
CREATE TABLE override (
	id INTEGER PRIMARY KEY,
	type TEXT NOT NULL CHECK (type IN ('exception', 'containment')),
	control TEXT NOT NULL CHECK (control IN ('permit', 'rate', 'operation')),
	operation TEXT NOT NULL CHECK (length(operation) BETWEEN 1 AND 64),
	role TEXT CHECK (role IS NULL OR length(role) BETWEEN 1 AND 64),
	count INTEGER CHECK (count IS NULL OR count BETWEEN 1 AND 1000000),
	target TEXT NOT NULL,
	base INTEGER NOT NULL REFERENCES revision (id),
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	actor TEXT NOT NULL,
	creation TEXT NOT NULL UNIQUE REFERENCES transition (id) DEFERRABLE INITIALLY DEFERRED,
	status TEXT NOT NULL CHECK (status IN ('active', 'revoked', 'expired', 'quarantined')),
	resolved_at INTEGER,
	resolved_by TEXT,
	resolution TEXT UNIQUE REFERENCES transition (id) DEFERRABLE INITIALLY DEFERRED,
	CHECK ((control = 'permit') = (role IS NOT NULL)),
	CHECK ((control = 'rate') = (count IS NOT NULL)),
	CHECK (control <> 'operation' OR type = 'containment'),
	CHECK (target = control || ':' || CASE WHEN role IS NULL THEN '' ELSE role || ':' END || operation),
	CHECK (expires_at > created_at),
	CHECK ((status = 'active') = (resolved_at IS NULL)),
	CHECK ((status = 'active') = (resolved_by IS NULL)),
	CHECK ((status = 'active') = (resolution IS NULL))
) STRICT;
CREATE UNIQUE INDEX override_active_target ON override (target) WHERE status = 'active';
CREATE TRIGGER override_fixed BEFORE UPDATE OF id, type, control, operation, role, count, target, base, created_at, expires_at, actor, creation ON override
BEGIN SELECT RAISE(ABORT, 'an override cannot change'); END;
CREATE TRIGGER override_final BEFORE UPDATE ON override WHEN OLD.status <> 'active'
BEGIN SELECT RAISE(ABORT, 'an override has exactly one final disposition'); END;
CREATE TRIGGER override_kept BEFORE DELETE ON override
BEGIN SELECT RAISE(ABORT, 'override history is kept'); END;
`,
	// A historical restore quarantines every active override under its one
	// restore transition, which a UNIQUE resolution column refuses as soon as
	// two were active. SQLite cannot drop a column constraint, so the table is
	// rebuilt in the documented order (create, copy, drop, rename) with its
	// other definitions unchanged. Foreign key enforcement stays on: no table
	// references override, so dropping it affects no other row, and every row
	// is checked again as it is copied. Ordinary revocations and expiries stay
	// one transition per override; a resolution is otherwise shared only by
	// overrides quarantined together.
	`
CREATE TABLE new_override (
	id INTEGER PRIMARY KEY,
	type TEXT NOT NULL CHECK (type IN ('exception', 'containment')),
	control TEXT NOT NULL CHECK (control IN ('permit', 'rate', 'operation')),
	operation TEXT NOT NULL CHECK (length(operation) BETWEEN 1 AND 64),
	role TEXT CHECK (role IS NULL OR length(role) BETWEEN 1 AND 64),
	count INTEGER CHECK (count IS NULL OR count BETWEEN 1 AND 1000000),
	target TEXT NOT NULL,
	base INTEGER NOT NULL REFERENCES revision (id),
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	actor TEXT NOT NULL,
	creation TEXT NOT NULL UNIQUE REFERENCES transition (id) DEFERRABLE INITIALLY DEFERRED,
	status TEXT NOT NULL CHECK (status IN ('active', 'revoked', 'expired', 'quarantined')),
	resolved_at INTEGER,
	resolved_by TEXT,
	resolution TEXT REFERENCES transition (id) DEFERRABLE INITIALLY DEFERRED,
	CHECK ((control = 'permit') = (role IS NOT NULL)),
	CHECK ((control = 'rate') = (count IS NOT NULL)),
	CHECK (control <> 'operation' OR type = 'containment'),
	CHECK (target = control || ':' || CASE WHEN role IS NULL THEN '' ELSE role || ':' END || operation),
	CHECK (expires_at > created_at),
	CHECK ((status = 'active') = (resolved_at IS NULL)),
	CHECK ((status = 'active') = (resolved_by IS NULL)),
	CHECK ((status = 'active') = (resolution IS NULL))
) STRICT;
INSERT INTO new_override SELECT * FROM override;
DROP TABLE override;
ALTER TABLE new_override RENAME TO override;
CREATE UNIQUE INDEX override_active_target ON override (target) WHERE status = 'active';
CREATE UNIQUE INDEX override_resolution ON override (resolution) WHERE status IN ('revoked', 'expired');
CREATE TRIGGER override_fixed BEFORE UPDATE OF id, type, control, operation, role, count, target, base, created_at, expires_at, actor, creation ON override
BEGIN SELECT RAISE(ABORT, 'an override cannot change'); END;
CREATE TRIGGER override_final BEFORE UPDATE ON override WHEN OLD.status <> 'active'
BEGIN SELECT RAISE(ABORT, 'an override has exactly one final disposition'); END;
CREATE TRIGGER override_kept BEFORE DELETE ON override
BEGIN SELECT RAISE(ABORT, 'override history is kept'); END;
CREATE TRIGGER override_shared BEFORE UPDATE OF resolution ON override WHEN EXISTS (
	SELECT 1 FROM override WHERE resolution = NEW.resolution AND (status = 'quarantined') <> (NEW.status = 'quarantined'))
BEGIN SELECT RAISE(ABORT, 'a resolution is shared only by overrides quarantined together'); END;
`, `
CREATE TABLE blocked_account (
	subject TEXT PRIMARY KEY CHECK (length(subject) BETWEEN 1 AND 128),
	reason TEXT NOT NULL CHECK (length(CAST(reason AS BLOB)) BETWEEN 1 AND 800),
	blocked_by TEXT NOT NULL REFERENCES administrator (name),
	blocked_at INTEGER NOT NULL
) STRICT;
CREATE TABLE control_result (
	transition TEXT PRIMARY KEY REFERENCES transition (id),
	body BLOB NOT NULL CHECK (length(body) BETWEEN 1 AND 65536)
) STRICT;
`}

// DatabaseVersion is the schema version this build reads and writes.
var DatabaseVersion = len(migrations)

// CheckDatabaseSchema compares the database schema with the one the
// migrations up to version create.
func CheckDatabaseSchema(db *sql.DB, version int) error {
	actual, err := schemaFingerprint(db)
	if err != nil {
		return fmt.Errorf("%w: reading schema: %v", ErrUnavailable, err)
	}
	expected, err := referenceFingerprint(version)
	if err != nil {
		return fmt.Errorf("building reference schema: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("%w: database schema differs from version %d", ErrUnavailable, version)
	}
	return nil
}

// ApplyMigrations applies the migrations after schema version from up to
// version to, and records the new version.
func ApplyMigrations(ctx context.Context, tx *sql.Tx, from, to int) error {
	for _, migration := range migrations[from:to] {
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", to))
	return err
}

// schemaFingerprint hashes every table, index and constraint definition.
func schemaFingerprint(db *sql.DB) (string, error) {
	rows, err := db.Query(`SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	sum := sha256.New()
	for rows.Next() {
		var kind, name, table, definition string
		if err := rows.Scan(&kind, &name, &table, &definition); err != nil {
			return "", err
		}
		fmt.Fprintf(sum, "%q %q %q %q\n", kind, name, table, definition)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// referenceFingerprint is the fingerprint of the migrations up to version
// applied to an empty in-memory database.
func referenceFingerprint(version int) (string, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := ApplyMigrations(ctx, tx, 0, version); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return schemaFingerprint(db)
}
