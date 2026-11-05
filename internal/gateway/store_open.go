// Opening, creating, verifying and loading the store.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/fault"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

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
		fault.CrashPoint("install-before-commit")
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
