package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Deployment modes. Only normal may serve traffic; the other two allow only
// the administrator flows that establish a new confirmed base.
const (
	modeBootstrap = "bootstrap_locked"
	modeNormal    = "normal"
	modeRecovery  = "recovery_required"
)

// errUnavailable marks state that must never be served: missing, empty,
// corrupt or unrecognised authority.
var errUnavailable = errors.New("authoritative state unavailable")

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

// databaseVersion is the schema version this build reads and writes.
var databaseVersion = len(migrations)

// Final dispositions of a provisional base. Confirmed keeps its policy as the
// new confirmed base; the others restore the rollback target. Quarantined
// marks a provisional base found in a restored historical database.
const (
	dispositionConfirmed   = "confirmed"
	dispositionRolledBack  = "rolled_back"
	dispositionTimedOut    = "timed_out"
	dispositionQuarantined = "quarantined"
)

// store is the single writer of control-plane state. The exclusive locking
// mode keeps a second gateway process, or an operator command, from opening
// the same database while it is in use.
type store struct {
	db *sql.DB
}

// authority is the committed state from which a generation is compiled.
type authority struct {
	mode         string
	installation string
	episode      string // recovery episode, when one was declared
	highWater    time.Time
	confirmed    int64 // 0 until a base has been established
	revision     int64 // active base: provisional if present, else confirmed
	provisional  int64
	candidate    int64 // the candidate the provisional base was activated from
	deadline     time.Time
	document     []byte     // active base document; read only in normal mode
	overrides    []override // active overrides in identifier order
}

// activationRecord is the durable lifecycle record of one provisional base:
// the candidate it came from, the confirmed revision it replaced and returns
// to on rollback, its deadline and its single final disposition.
type activationRecord struct {
	Revision       int64      `json:"revision"`
	Candidate      int64      `json:"candidate"`
	RollbackTarget int64      `json:"rollback_target"`
	Deadline       time.Time  `json:"deadline"`
	ActivatedAt    time.Time  `json:"activated_at"`
	Actor          string     `json:"actor"`
	Disposition    string     `json:"disposition,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy     string     `json:"resolved_by,omitempty"`
	Resolution     string     `json:"resolution,omitempty"`
}

// transitionRecord is the durable history entry and idempotency record of one
// mutation. Its ID is the mutation identity: a repeated ID returns this record
// instead of applying the mutation again.
type transitionRecord struct {
	ID        string
	Kind      string
	Actor     string
	Target    string
	Expected  sql.NullInt64 // revision the request required, if any
	Digest    string        // canonical request digest
	Outcome   string
	Reason    string
	Revision  int64 // revision the mutation produced, if any
	Candidate int64 // candidate the mutation produced, if any
	Audited   bool  // the outcome event was durably recorded
	// Override is the override the mutation concerns, for its receipt and
	// events. It is not a column: the override row names its transitions.
	Override int64
}

// revisionChange is a new active base and the state committed with it.
type revisionChange struct {
	parent    int64 // 0: no parent
	origin    string
	document  []byte
	actor     string
	deadline  time.Time // zero: the revision becomes the confirmed base; else provisional until then
	mode      string    // deployment mode after the commit
	candidate int64     // candidate this revision establishes, if any
}

// candidateRecord is an immutable staged policy and its validation result.
type candidateRecord struct {
	ID        int64     `json:"id"`
	Parent    int64     `json:"parent,omitempty"`
	Checksum  string    `json:"checksum"`
	DenyAll   bool      `json:"deny_all"`
	Status    string    `json:"status"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
	Document  []byte    `json:"-"`
}

type administrator struct {
	Name        string
	Permissions permission
	Password    string // argon2id PHC string
	Status      string
	Credential  int64 // incremented by every credential change
}

type enrolment struct {
	Name        string
	Secret      string // SHA-256 of the enrolment secret
	Permissions permission
	ExpiresAt   time.Time
}

// revisionInfo describes a stored revision; Document is set only when one
// revision is read.
type revisionInfo struct {
	ID        int64           `json:"id"`
	Parent    int64           `json:"parent,omitempty"`
	Origin    string          `json:"origin"`
	Checksum  string          `json:"checksum"`
	Actor     string          `json:"actor"`
	CreatedAt time.Time       `json:"created_at"`
	Document  json.RawMessage `json:"document,omitempty"`
}

// historyEntry is one transition as administrators see it.
type historyEntry struct {
	Seq         int64      `json:"seq"`
	ID          string     `json:"transition_id"`
	Kind        string     `json:"kind"`
	Actor       string     `json:"actor"`
	Target      string     `json:"target"`
	Expected    *int64     `json:"expected_revision,omitempty"`
	Outcome     string     `json:"outcome"`
	Reason      string     `json:"reason,omitempty"`
	Revision    int64      `json:"revision,omitempty"`
	Candidate   int64      `json:"candidate,omitempty"`
	CommittedAt time.Time  `json:"recorded_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	PublishedBy string     `json:"published_by,omitempty"`
	AuditedAt   *time.Time `json:"audited_at,omitempty"`
}

// databaseURI keeps literal filename characters out of SQLite's URI options
// and fragment. In particular, a percent escape in a filename is not decoded.
func databaseURI(path string) string {
	location := url.URL{Path: filepath.ToSlash(path)}
	return "file:" + location.EscapedPath()
}

// connect opens the database with the settings every connection must have
// and takes the exclusive lock at once, so a second process fails here rather
// than at its first mutation.
func connect(path, journal string) (*sql.DB, error) {
	dsn := databaseURI(path) +
		"?_pragma=locking_mode(EXCLUSIVE)" +
		"&_pragma=journal_mode(" + journal + ")" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("BEGIN IMMEDIATE; COMMIT"); err != nil {
		db.Close()
		if isBusy(err) {
			return nil, fmt.Errorf("database %q is held by another gateway process: %w", path, err)
		}
		return nil, fmt.Errorf("%w: opening database: %v", errUnavailable, err)
	}
	return db, nil
}

func isBusy(err error) bool {
	sqliteErr, ok := errors.AsType[*sqlite.Error](err)
	return ok && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

// openStore opens an installed database. It never creates one: a missing or
// empty file means no authority exists.
func openStore(path string) (*store, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return nil, fmt.Errorf("%w: database %q missing or empty", errUnavailable, path)
	}
	db, err := connect(path, "WAL")
	if err != nil {
		return nil, err
	}
	if err := verify(db); err != nil {
		db.Close()
		return nil, err
	}
	return &store{db: db}, nil
}

// verify refuses a database this build must not trust: foreign keys not
// enforced on the connection, a failed integrity check, another schema
// version, or a schema that differs from what the migrations create.
func verify(db *sql.DB) error {
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		return fmt.Errorf("%w: foreign key enforcement is not active (%v)", errUnavailable, err)
	}
	var check string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("%w: integrity check failed: %v %s", errUnavailable, err, check)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("%w: reading schema version: %v", errUnavailable, err)
	}
	if version < databaseVersion && version > 0 {
		return fmt.Errorf("%w: database schema version %d, this gateway needs %d (stop the gateway and run its migrate command)", errUnavailable, version, databaseVersion)
	}
	if version != databaseVersion {
		return fmt.Errorf("%w: database schema version %d, this gateway needs %d", errUnavailable, version, databaseVersion)
	}
	if err := checkDatabaseSchema(db, databaseVersion); err != nil {
		return err
	}
	// quick_check and integrity_check do not check foreign-key consistency.
	// Enabling enforcement protects new writes, not corrupt existing rows.
	var violations int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		return fmt.Errorf("%w: foreign key check failed: %v", errUnavailable, err)
	}
	if violations != 0 {
		return fmt.Errorf("%w: foreign key check failed: %d violations", errUnavailable, violations)
	}
	return nil
}

// checkDatabaseSchema compares the database schema with the one the
// migrations up to version create.
func checkDatabaseSchema(db *sql.DB, version int) error {
	actual, err := schemaFingerprint(db)
	if err != nil {
		return fmt.Errorf("%w: reading schema: %v", errUnavailable, err)
	}
	expected, err := referenceFingerprint(version)
	if err != nil {
		return fmt.Errorf("building reference schema: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("%w: database schema differs from version %d", errUnavailable, version)
	}
	return nil
}

// applyMigrations applies the migrations after schema version from up to
// version to, and records the new version.
func applyMigrations(ctx context.Context, tx *sql.Tx, from, to int) error {
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
	if err := applyMigrations(ctx, tx, 0, version); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return schemaFingerprint(db)
}

// createStore installs a new database at path in bootstrap_locked mode with
// the first administrator's enrolment and the install transition, all in one
// transaction. The file must not exist: installation never replaces a
// database. An interrupted install leaves a file without a schema, which
// startup and install both refuse.
func createStore(path, installation string, first enrolment, install transitionRecord, now time.Time) (*store, error) {
	if err := createPrivate(path); err != nil {
		return nil, err
	}
	db, err := connect(path, "WAL")
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return nil, err
	}
	defer tx.Rollback()

	steps := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO deployment (id, installation, mode, high_water) VALUES (1, ?, ?, ?)`, []any{installation, modeBootstrap, now.UnixMilli()}},
		{`INSERT INTO base (id) VALUES (1)`, nil},
		{`INSERT INTO enrolment (administrator, secret, permissions, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
			[]any{first.Name, first.Secret, first.Permissions, first.ExpiresAt.UnixMilli(), now.UnixMilli()}},
	}
	err = applyMigrations(ctx, tx, 0, databaseVersion)
	for _, step := range steps {
		if err != nil {
			break
		}
		_, err = tx.ExecContext(ctx, step.query, step.args...)
	}
	if err == nil {
		err = insertCommitted(ctx, tx, install, now)
	}
	if err == nil {
		crashPoint("install-before-commit")
		err = tx.Commit()
	}
	if err == nil {
		err = verify(db)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &store{db: db}, nil
}

// createPrivate creates an empty database file with mode 0600. Windows uses
// the parent directory DACL; mode bits do not provide confidentiality there.
// It fails if the file exists, so nothing is ever replaced. SQLite
// writes a database or VACUUM INTO output into the empty file it finds.
func createPrivate(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func (s *store) close() error { return s.db.Close() }

func (s *store) load(ctx context.Context) (authority, error) {
	var a authority
	var highWater int64
	var provisional, deadline sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT d.mode, d.installation, coalesce(d.recovery_episode, ''), d.high_water,
			coalesce(b.confirmed, 0), b.provisional, b.deadline
		FROM deployment d, base b WHERE d.id = 1 AND b.id = 1`).
		Scan(&a.mode, &a.installation, &a.episode, &highWater, &a.confirmed, &provisional, &deadline)
	if err != nil {
		return authority{}, fmt.Errorf("%w: reading base state: %v", errUnavailable, err)
	}
	a.highWater = time.UnixMilli(highWater)
	a.revision = a.confirmed
	if provisional.Valid {
		a.provisional = provisional.Int64
		a.revision = provisional.Int64
		a.deadline = time.UnixMilli(deadline.Int64)
	}
	if err := s.checkActivation(ctx, &a); err != nil {
		return authority{}, err
	}
	a.overrides, err = s.overrides(ctx, `WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return authority{}, fmt.Errorf("%w: reading overrides: %v", errUnavailable, err)
	}
	if a.mode != modeNormal {
		return a, nil
	}
	if a.confirmed == 0 {
		return authority{}, fmt.Errorf("%w: normal mode without a confirmed base", errUnavailable)
	}
	var checksum string
	if err := s.db.QueryRowContext(ctx, `SELECT document, checksum FROM revision WHERE id = ?`, a.revision).
		Scan(&a.document, &checksum); err != nil {
		return authority{}, fmt.Errorf("%w: reading revision %d: %v", errUnavailable, a.revision, err)
	}
	if documentChecksum(a.document) != checksum {
		return authority{}, fmt.Errorf("%w: revision %d checksum mismatch", errUnavailable, a.revision)
	}
	return a, nil
}

// checkActivation requires the provisional base and the one unresolved
// activation record to describe the same revision, deadline and rollback
// target, and fills in the candidate. Any disagreement means the lifecycle
// state cannot be trusted, so nothing is served.
func (s *store) checkActivation(ctx context.Context, a *authority) error {
	var revision, candidate, target, deadline int64
	err := s.db.QueryRowContext(ctx, `
		SELECT revision, candidate, rollback_target, deadline FROM activation WHERE disposition IS NULL`).
		Scan(&revision, &candidate, &target, &deadline)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: reading activation: %v", errUnavailable, err)
	}
	consistent := found == (a.provisional != 0)
	if found {
		consistent = consistent &&
			revision == a.provisional &&
			deadline == a.deadline.UnixMilli() &&
			target == a.confirmed
	}
	if !consistent {
		return fmt.Errorf("%w: provisional base and activation record disagree", errUnavailable)
	}
	a.candidate = candidate
	return nil
}

func (s *store) transition(ctx context.Context, id string) (transitionRecord, bool, error) {
	var t transitionRecord
	var revision, candidate, audited sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, actor, target, expected, digest, outcome, reason, result_revision, result_candidate, audited_at
		FROM transition WHERE id = ?`, id).
		Scan(&t.ID, &t.Kind, &t.Actor, &t.Target, &t.Expected, &t.Digest, &t.Outcome, &t.Reason, &revision, &candidate, &audited)
	if errors.Is(err, sql.ErrNoRows) {
		return transitionRecord{}, false, nil
	}
	if err != nil {
		return transitionRecord{}, false, err
	}
	t.Revision, t.Candidate, t.Audited = revision.Int64, candidate.Int64, audited.Valid
	return t, true, nil
}

func (s *store) recordRejection(ctx context.Context, t transitionRecord, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO transition (id, kind, actor, target, expected, digest, outcome, reason, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, 'rejected', ?, ?)`,
		t.ID, t.Kind, t.Actor, t.Target, t.Expected, t.Digest, t.Reason, now.UnixMilli())
	return err
}

// commitRevision atomically stores a new revision, makes it the active base,
// applies the deployment mode and candidate changes that go with it, and
// records the committed transition. This is the authoritative commit point;
// the runtime pointer swap follows it.
func (s *store) commitRevision(ctx context.Context, t transitionRecord, change revisionChange, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO revision (parent, origin, document, checksum, actor, created_at) VALUES (NULLIF(?, 0), ?, ?, ?, ?, ?)`,
		change.parent, change.origin, change.document, documentChecksum(change.document), change.actor, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	revision, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	crashPoint("commit-partway")
	if change.deadline.IsZero() {
		_, err = tx.ExecContext(ctx, `UPDATE base SET confirmed = ?, provisional = NULL, deadline = NULL WHERE id = 1`, revision)
	} else {
		// The activation record holds the rollback target and deadline; the
		// base may point at a provisional revision only through it.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO activation (revision, candidate, rollback_target, deadline, activated_at, actor)
			VALUES (?, ?, ?, ?, ?, ?)`,
			revision, change.candidate, change.parent, change.deadline.UnixMilli(), now.UnixMilli(), change.actor)
		if err == nil {
			err = updateOne(ctx, tx, `UPDATE base SET provisional = ?, deadline = ? WHERE id = 1 AND confirmed = ? AND provisional IS NULL`,
				revision, change.deadline.UnixMilli(), change.parent)
		}
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deployment SET mode = ? WHERE id = 1`, change.mode); err != nil {
		return 0, err
	}
	if change.candidate != 0 {
		if err := updateOne(ctx, tx, `UPDATE candidate SET status = 'established' WHERE id = ? AND status = 'validated'`, change.candidate); err != nil {
			return 0, fmt.Errorf("establishing candidate %d: %w", change.candidate, err)
		}
	}
	t.Revision = revision
	t.Candidate = change.candidate
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return 0, err
	}
	return revision, tx.Commit()
}

// commitResolution records the final disposition of the provisional base and
// the base that follows it, in one transaction: confirmation makes the
// provisional revision the confirmed base, every other disposition leaves the
// earlier confirmed base active. Both guarded updates fail unless the
// provisional base is still the one the caller resolved.
func (s *store) commitResolution(ctx context.Context, t transitionRecord, a authority, disposition string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	if err := updateOne(ctx, tx, `
		UPDATE activation SET disposition = ?, resolved_at = ?, resolved_by = ?, resolution = ?
		WHERE revision = ? AND disposition IS NULL`,
		disposition, now.UnixMilli(), t.Actor, t.ID, a.provisional); err != nil {
		return fmt.Errorf("resolving revision %d: %w", a.provisional, err)
	}
	confirmed := a.confirmed
	if disposition == dispositionConfirmed {
		confirmed = a.provisional
	}
	if err := updateOne(ctx, tx, `
		UPDATE base SET confirmed = ?, provisional = NULL, deadline = NULL WHERE id = 1 AND provisional = ?`,
		confirmed, a.provisional); err != nil {
		return fmt.Errorf("ending provisional revision %d: %w", a.provisional, err)
	}
	return tx.Commit()
}

// commitCandidate stores a validated candidate with its committed transition.
func (s *store) commitCandidate(ctx context.Context, t transitionRecord, c candidateRecord, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO candidate (parent, document, checksum, deny_all, status, actor, created_at)
		VALUES (NULLIF(?, 0), ?, ?, ?, 'validated', ?, ?)`,
		c.Parent, c.Document, c.Checksum, c.DenyAll, c.Actor, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	t.Candidate = id
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// commitWithdrawal withdraws a validated candidate with its committed
// transition.
func (s *store) commitWithdrawal(ctx context.Context, t transitionRecord, id int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := updateOne(ctx, tx, `UPDATE candidate SET status = 'withdrawn' WHERE id = ? AND status = 'validated'`, id); err != nil {
		return err
	}
	t.Candidate = id
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	return tx.Commit()
}

// commitEnrolment consumes an enrolment and writes the administrator's new
// credential in one transaction. Only one request can consume a secret.
func (s *store) commitEnrolment(ctx context.Context, t transitionRecord, e enrolment, passwordHash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Hashing and waiting for the controller can outlive the handler's first
	// expiry check. Consume only an enrolment still valid at this decision.
	if err := updateOne(ctx, tx, `DELETE FROM enrolment WHERE administrator = ? AND secret = ? AND expires_at > ?`, e.Name, e.Secret, now.UnixMilli()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO administrator (name, permissions, password, status, credential, updated_at)
		VALUES (?, ?, ?, 'active', 1, ?)
		ON CONFLICT (name) DO UPDATE SET permissions = excluded.permissions, password = excluded.password,
			status = 'active', credential = credential + 1, updated_at = excluded.updated_at`,
		e.Name, e.Permissions, passwordHash, now.UnixMilli())
	if err != nil {
		return err
	}
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	return tx.Commit()
}

// commitPassword replaces an administrator's password if the credential has
// not changed since it was verified.
func (s *store) commitPassword(ctx context.Context, t transitionRecord, name string, credential int64, passwordHash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := updateOne(ctx, tx, `
		UPDATE administrator SET password = ?, credential = credential + 1, updated_at = ?
		WHERE name = ? AND credential = ? AND status = 'active'`,
		passwordHash, now.UnixMilli(), name, credential); err != nil {
		return err
	}
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	return tx.Commit()
}

// errNotUpdated reports that a guarded update found no matching row, so the
// state it depended on had changed.
var errNotUpdated = errors.New("no matching row")

func updateOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errNotUpdated
	}
	return nil
}

func insertCommitted(ctx context.Context, tx *sql.Tx, t transitionRecord, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO transition (id, kind, actor, target, expected, digest, outcome, reason,
			result_revision, result_candidate, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, 'committed', '', NULLIF(?, 0), NULLIF(?, 0), ?)`,
		t.ID, t.Kind, t.Actor, t.Target, t.Expected, t.Digest, t.Revision, t.Candidate, now.UnixMilli())
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE deployment SET high_water = MAX(high_water, ?) WHERE id = 1`, now.UnixMilli())
	return err
}

// markPublished records that the runtime now serves the transition's result.
// It is a receipt only; authority was established by the commit.
func (s *store) markPublished(ctx context.Context, id string, how string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE transition SET published_at = ?, published_by = ? WHERE id = ? AND published_at IS NULL`,
		now.UnixMilli(), how, id)
	return err
}

// unpublished lists committed transitions that changed the effective policy,
// by producing a revision or by creating or ending an override, but have no
// publication receipt.
func (s *store) unpublished(ctx context.Context) ([]string, error) {
	return s.ids(ctx, `SELECT id FROM transition
		WHERE outcome = 'committed' AND (result_revision IS NOT NULL OR kind LIKE 'tpo.%' OR kind IN ('account.block','account.unblock','keys.reload')) AND published_at IS NULL ORDER BY rowid`)
}

// markAudited records that the transition's outcome event was durably written.
func (s *store) markAudited(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE transition SET audited_at = ? WHERE id = ? AND audited_at IS NULL`, now.UnixMilli(), id)
	return err
}

// unaudited lists committed transitions whose outcome event was never
// acknowledged by the audit log.
func (s *store) unaudited(ctx context.Context) ([]string, error) {
	return s.ids(ctx, `SELECT id FROM transition WHERE outcome = 'committed' AND audited_at IS NULL ORDER BY rowid`)
}

func (s *store) ids(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *store) raiseHighWater(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployment SET high_water = MAX(high_water, ?) WHERE id = 1`, now.UnixMilli())
	return err
}

func (s *store) administrator(ctx context.Context, name string) (administrator, bool, error) {
	var a administrator
	err := s.db.QueryRowContext(ctx, `SELECT name, permissions, password, status, credential FROM administrator WHERE name = ?`, name).
		Scan(&a.Name, &a.Permissions, &a.Password, &a.Status, &a.Credential)
	if errors.Is(err, sql.ErrNoRows) {
		return administrator{}, false, nil
	}
	return a, err == nil, err
}

func (s *store) enrolment(ctx context.Context, secretHash string) (enrolment, bool, error) {
	var e enrolment
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT administrator, secret, permissions, expires_at FROM enrolment WHERE secret = ?`, secretHash).
		Scan(&e.Name, &e.Secret, &e.Permissions, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return enrolment{}, false, nil
	}
	e.ExpiresAt = time.UnixMilli(expires)
	return e, err == nil, err
}

// issueEnrolment replaces any pending enrolment for the administrator and
// records the operator transition, in one transaction.
func (s *store) issueEnrolment(ctx context.Context, t transitionRecord, e enrolment, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO enrolment (administrator, secret, permissions, expires_at, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (administrator) DO UPDATE SET secret = excluded.secret, permissions = excluded.permissions,
			expires_at = excluded.expires_at, created_at = excluded.created_at`,
		e.Name, e.Secret, e.Permissions, e.ExpiresAt.UnixMilli(), now.UnixMilli())
	if err != nil {
		return err
	}
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	return tx.Commit()
}

// declareRecovery puts a restored database into recovery_required under a new
// episode and quarantines everything whose authority cannot be trusted after a
// historical restore: administrators, candidates, pending enrolments, any
// provisional base, whose activation is closed as quarantined rather than
// confirmed or rolled back, and every override still active in the backup,
// which may since have been revoked. The activation and all those overrides
// name the restore transition as their resolution. History and revisions are
// kept as evidence.
func (s *store) declareRecovery(ctx context.Context, t transitionRecord, episode string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE deployment SET mode = ?, recovery_episode = ? WHERE id = 1`, []any{modeRecovery, episode}},
		{`UPDATE administrator SET status = 'quarantined', updated_at = ?`, []any{now.UnixMilli()}},
		{`UPDATE candidate SET status = 'quarantined' WHERE status = 'validated'`, nil},
		{`DELETE FROM enrolment`, nil},
		{`UPDATE activation SET disposition = ?, resolved_at = ?, resolved_by = 'operator', resolution = ? WHERE disposition IS NULL`,
			[]any{dispositionQuarantined, now.UnixMilli(), t.ID}},
		{`UPDATE base SET provisional = NULL, deadline = NULL WHERE id = 1`, nil},
		{`UPDATE override SET status = ?, resolved_at = ?, resolved_by = 'operator', resolution = ? WHERE status = 'active'`,
			[]any{overrideQuarantined, now.UnixMilli(), t.ID}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return err
		}
	}
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) candidate(ctx context.Context, id int64) (candidateRecord, bool, error) {
	var c candidateRecord
	var parent sql.NullInt64
	var created int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, parent, document, checksum, deny_all, status, actor, created_at FROM candidate WHERE id = ?`, id).
		Scan(&c.ID, &parent, &c.Document, &c.Checksum, &c.DenyAll, &c.Status, &c.Actor, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return candidateRecord{}, false, nil
	}
	c.Parent, c.CreatedAt = parent.Int64, time.UnixMilli(created).UTC()
	return c, err == nil, err
}

func (s *store) candidates(ctx context.Context, after int64, limit int) ([]candidateRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, parent, checksum, deny_all, status, actor, created_at FROM candidate
		WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []candidateRecord{}
	for rows.Next() {
		var c candidateRecord
		var parent sql.NullInt64
		var created int64
		if err := rows.Scan(&c.ID, &parent, &c.Checksum, &c.DenyAll, &c.Status, &c.Actor, &created); err != nil {
			return nil, err
		}
		c.Parent, c.CreatedAt = parent.Int64, time.UnixMilli(created).UTC()
		list = append(list, c)
	}
	return list, rows.Err()
}

const activationColumns = `revision, candidate, rollback_target, deadline, activated_at, actor,
	coalesce(disposition, ''), resolved_at, coalesce(resolved_by, ''), coalesce(resolution, '')`

// activation reads the activation record selected by column, which is
// revision or candidate.
func (s *store) activation(ctx context.Context, column string, id int64) (activationRecord, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+activationColumns+` FROM activation WHERE `+column+` = ?`, id)
	if err != nil {
		return activationRecord{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return activationRecord{}, false, rows.Err()
	}
	record, err := scanActivation(rows)
	return record, err == nil, err
}

func (s *store) activations(ctx context.Context, after int64, limit int) ([]activationRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+activationColumns+` FROM activation WHERE revision > ? ORDER BY revision LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []activationRecord{}
	for rows.Next() {
		record, err := scanActivation(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, record)
	}
	return list, rows.Err()
}

func scanActivation(rows *sql.Rows) (activationRecord, error) {
	var r activationRecord
	var deadline, activated int64
	var resolved sql.NullInt64
	err := rows.Scan(&r.Revision, &r.Candidate, &r.RollbackTarget, &deadline, &activated, &r.Actor,
		&r.Disposition, &resolved, &r.ResolvedBy, &r.Resolution)
	if err != nil {
		return activationRecord{}, err
	}
	r.Deadline = time.UnixMilli(deadline).UTC()
	r.ActivatedAt = time.UnixMilli(activated).UTC()
	r.ResolvedAt = optionalTime(resolved)
	return r, nil
}

const overrideColumns = `id, type, control, operation, coalesce(role, ''), coalesce(count, 0), target, base,
	created_at, expires_at, actor, creation, status, resolved_at, coalesce(resolved_by, ''), coalesce(resolution, '')`

// overrides reads the overrides selected by the WHERE and ORDER clause.
func (s *store) overrides(ctx context.Context, clause string, args ...any) ([]override, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+overrideColumns+` FROM override `+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []override{}
	for rows.Next() {
		var o override
		var created, expires int64
		var resolved sql.NullInt64
		err := rows.Scan(&o.ID, &o.Type, &o.Control, &o.Operation, &o.Role, &o.Count, &o.Target, &o.Base,
			&created, &expires, &o.Actor, &o.Creation, &o.Status, &resolved, &o.ResolvedBy, &o.Resolution)
		if err != nil {
			return nil, err
		}
		o.CreatedAt, o.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
		o.ResolvedAt = optionalTime(resolved)
		list = append(list, o)
	}
	return list, rows.Err()
}

// override reads one override by its identifier.
func (s *store) override(ctx context.Context, id int64) (override, bool, error) {
	list, err := s.overrides(ctx, `WHERE id = ?`, id)
	if err != nil || len(list) == 0 {
		return override{}, false, err
	}
	return list[0], true, nil
}

// overrideOf reads the override a temporary-override transition created,
// revoked or expired; the unique creation column and the override_resolution
// index allow at most one of each. A restore transition, which can quarantine
// several overrides, is not one.
func (s *store) overrideOf(ctx context.Context, transition string) (override, bool, error) {
	list, err := s.overrides(ctx, `WHERE creation = ? OR (resolution = ? AND status IN ('revoked', 'expired'))`, transition, transition)
	if err != nil || len(list) == 0 {
		return override{}, false, err
	}
	return list[0], true, nil
}

// nextOverrideID is the identifier the next override will have. Rows are
// never deleted and the controller is the only writer, so it is known before
// the override is committed and can label the generation compiled for it.
func (s *store) nextOverrideID(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) + 1 FROM override`).Scan(&id)
	return id, err
}

// commitOverride stores a new active override with its committed transition.
// The unique index on active targets refuses a second active override of the
// same target even if a caller skipped the conflict check.
func (s *store) commitOverride(ctx context.Context, t transitionRecord, o override, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	var role, count any
	if o.Control == controlPermit {
		role = o.Role
	}
	if o.Control == controlRate {
		count = o.Count
	}
	crashPoint("tpo-commit-partway")
	_, err = tx.ExecContext(ctx, `
		INSERT INTO override (id, type, control, operation, role, count, target, base, created_at, expires_at, actor, creation, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active')`,
		o.ID, o.Type, o.Control, o.Operation, role, count, o.Target, o.Base,
		o.CreatedAt.UnixMilli(), o.ExpiresAt.UnixMilli(), o.Actor, t.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// commitOverrideEnd records the final status of an active override with its
// committed transition. The guarded update fails unless the override is still
// active, so it can end only once.
func (s *store) commitOverrideEnd(ctx context.Context, t transitionRecord, id int64, status string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	if err := updateOne(ctx, tx, `
		UPDATE override SET status = ?, resolved_at = ?, resolved_by = ?, resolution = ? WHERE id = ? AND status = 'active'`,
		status, now.UnixMilli(), t.Actor, t.ID, id); err != nil {
		return fmt.Errorf("ending override %d: %w", id, err)
	}
	return tx.Commit()
}

// openForMigration opens a database at an earlier schema version and returns
// it with that version. A database whose schema is not exactly its version's,
// or a version 1 database with a provisional base, which has no activation
// record to migrate, is refused unchanged.
func openForMigration(path string) (*store, int, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return nil, 0, fmt.Errorf("database %q missing or empty", path)
	}
	db, err := connect(path, "WAL")
	if err != nil {
		return nil, 0, err
	}
	version, err := migratable(db)
	if err != nil {
		db.Close()
		return nil, version, err
	}
	return &store{db: db}, version, nil
}

func migratable(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	if version < 1 || version >= databaseVersion {
		return version, fmt.Errorf("database schema version %d cannot be migrated to %d", version, databaseVersion)
	}
	if err := checkDatabaseSchema(db, version); err != nil {
		return version, err
	}
	if version >= 2 {
		return version, nil
	}
	var provisional sql.NullInt64
	if err := db.QueryRow(`SELECT provisional FROM base WHERE id = 1`).Scan(&provisional); err != nil {
		return version, err
	}
	if provisional.Valid {
		return version, errors.New("the database has a provisional base without an activation record; it cannot be migrated")
	}
	return version, nil
}

// migrate applies the migrations from version to the current one in a single
// transaction with the operator transition, then checks the result. A
// database whose migrated content fails the integrity or foreign key check is
// left at its earlier version: the checks run before the commit, so damaged
// rows are refused rather than carried into the new version.
func (s *store) migrate(ctx context.Context, from int, t transitionRecord, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyMigrations(ctx, tx, from, databaseVersion); err != nil {
		return err
	}
	var check string
	if err := tx.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("integrity check failed: %v %s", err, check)
	}
	var violations int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("foreign key check failed: %d violations", violations)
	}
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	crashPoint("migrate-before-commit")
	if err := tx.Commit(); err != nil {
		return err
	}
	return verify(s.db)
}

func (s *store) revision(ctx context.Context, id int64) (revisionInfo, bool, error) {
	var r revisionInfo
	var parent sql.NullInt64
	var created int64
	var document []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, parent, origin, checksum, actor, created_at, document FROM revision WHERE id = ?`, id).
		Scan(&r.ID, &parent, &r.Origin, &r.Checksum, &r.Actor, &created, &document)
	if errors.Is(err, sql.ErrNoRows) {
		return revisionInfo{}, false, nil
	}
	if err != nil {
		return revisionInfo{}, false, err
	}
	if documentChecksum(document) != r.Checksum {
		return revisionInfo{}, false, fmt.Errorf("revision %d checksum mismatch", id)
	}
	r.Parent, r.CreatedAt, r.Document = parent.Int64, time.UnixMilli(created).UTC(), document
	return r, true, nil
}

func (s *store) revisions(ctx context.Context, after int64, limit int) ([]revisionInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, parent, origin, checksum, actor, created_at FROM revision WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []revisionInfo{}
	for rows.Next() {
		var r revisionInfo
		var parent sql.NullInt64
		var created int64
		if err := rows.Scan(&r.ID, &parent, &r.Origin, &r.Checksum, &r.Actor, &created); err != nil {
			return nil, err
		}
		r.Parent, r.CreatedAt = parent.Int64, time.UnixMilli(created).UTC()
		list = append(list, r)
	}
	return list, rows.Err()
}

const historyColumns = `rowid, id, kind, actor, target, expected, outcome, reason, result_revision, result_candidate,
	committed_at, published_at, coalesce(published_by, ''), audited_at`

func (s *store) history(ctx context.Context, after int64, limit int) ([]historyEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+historyColumns+` FROM transition WHERE rowid > ? ORDER BY rowid LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []historyEntry{}
	for rows.Next() {
		h, err := scanHistory(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, h)
	}
	return list, rows.Err()
}

func (s *store) historyEntry(ctx context.Context, id string) (historyEntry, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+historyColumns+` FROM transition WHERE id = ?`, id)
	if err != nil {
		return historyEntry{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return historyEntry{}, false, rows.Err()
	}
	h, err := scanHistory(rows)
	return h, err == nil, err
}

func scanHistory(rows *sql.Rows) (historyEntry, error) {
	var h historyEntry
	var expected, revision, candidate, published, audited sql.NullInt64
	var committed int64
	err := rows.Scan(&h.Seq, &h.ID, &h.Kind, &h.Actor, &h.Target, &expected, &h.Outcome, &h.Reason,
		&revision, &candidate, &committed, &published, &h.PublishedBy, &audited)
	if err != nil {
		return historyEntry{}, err
	}
	if expected.Valid {
		h.Expected = &expected.Int64
	}
	h.Revision, h.Candidate = revision.Int64, candidate.Int64
	h.CommittedAt = time.UnixMilli(committed).UTC()
	h.PublishedAt = optionalTime(published)
	h.AuditedAt = optionalTime(audited)
	return h, nil
}

func optionalTime(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}
	t := time.UnixMilli(ms.Int64).UTC()
	return &t
}

func documentChecksum(document []byte) string {
	sum := sha256.Sum256(document)
	return hex.EncodeToString(sum[:])
}
