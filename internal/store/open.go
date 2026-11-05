// Opening, creating, verifying and loading the store.

package store

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

// DatabaseURI keeps literal filename characters out of SQLite's URI options
// and fragment. In particular, a percent escape in a filename is not decoded.
func DatabaseURI(path string) string {
	location := url.URL{Path: filepath.ToSlash(path)}
	return "file:" + location.EscapedPath()
}

// Connect opens the database with the settings every connection must have
// and takes the exclusive lock at once, so a second process fails here rather
// than at its first mutation.
func Connect(path, journal string) (*sql.DB, error) {
	dsn := DatabaseURI(path) +
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
		return nil, fmt.Errorf("%w: opening database: %v", ErrUnavailable, err)
	}
	return db, nil
}

func isBusy(err error) bool {
	sqliteErr, ok := errors.AsType[*sqlite.Error](err)
	return ok && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

// OpenStore opens an installed database. It never creates one: a missing or
// empty file means no authority exists.
func OpenStore(path string) (*DB, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return nil, fmt.Errorf("%w: database %q missing or empty", ErrUnavailable, path)
	}
	db, err := Connect(path, "WAL")
	if err != nil {
		return nil, err
	}
	if err := Verify(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

// Verify refuses a database this build must not trust: foreign keys not
// enforced on the connection, a failed integrity check, another schema
// version, or a schema that differs from what the migrations create.
func Verify(db *sql.DB) error {
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		return fmt.Errorf("%w: foreign key enforcement is not active (%v)", ErrUnavailable, err)
	}
	var check string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("%w: integrity check failed: %v %s", ErrUnavailable, err, check)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("%w: reading schema version: %v", ErrUnavailable, err)
	}
	if version < DatabaseVersion && version > 0 {
		return fmt.Errorf("%w: database schema version %d, this gateway needs %d (stop the gateway and run its migrate command)", ErrUnavailable, version, DatabaseVersion)
	}
	if version != DatabaseVersion {
		return fmt.Errorf("%w: database schema version %d, this gateway needs %d", ErrUnavailable, version, DatabaseVersion)
	}
	if err := CheckDatabaseSchema(db, DatabaseVersion); err != nil {
		return err
	}
	// quick_check and integrity_check do not check foreign-key consistency.
	// Enabling enforcement protects new writes, not corrupt existing rows.
	var violations int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		return fmt.Errorf("%w: foreign key check failed: %v", ErrUnavailable, err)
	}
	if violations != 0 {
		return fmt.Errorf("%w: foreign key check failed: %d violations", ErrUnavailable, violations)
	}
	return nil
}

// CreateStore installs a new database at path in bootstrap_locked mode with
// the first administrator's enrolment and the install transition, all in one
// transaction. The file must not exist: installation never replaces a
// database. An interrupted install leaves a file without a schema, which
// startup and install both refuse.
func CreateStore(path, installation string, first Enrolment, install TransitionRecord, now time.Time) (*DB, error) {
	if err := CreatePrivate(path); err != nil {
		return nil, err
	}
	db, err := Connect(path, "WAL")
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
		{`INSERT INTO deployment (id, installation, mode, high_water) VALUES (1, ?, ?, ?)`, []any{installation, ModeBootstrap, now.UnixMilli()}},
		{`INSERT INTO base (id) VALUES (1)`, nil},
		{`INSERT INTO enrolment (administrator, secret, permissions, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
			[]any{first.Name, first.Secret, first.Permissions, first.ExpiresAt.UnixMilli(), now.UnixMilli()}},
	}
	err = ApplyMigrations(ctx, tx, 0, DatabaseVersion)
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
		err = Verify(db)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

// CreatePrivate creates an empty database file with mode 0600. Windows uses
// the parent directory DACL; mode bits do not provide confidentiality there.
// It fails if the file exists, so nothing is ever replaced. SQLite
// writes a database or VACUUM INTO output into the empty file it finds.
func CreatePrivate(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func (s *DB) Close() error { return s.db.Close() }

// Adopt makes db, an open connection to a deployment database, a store; the
// store's close closes it.
func Adopt(db *sql.DB) *DB { return &DB{db: db} }

// Ping reports whether the database connection is still usable.
func (s *DB) Ping() error { return s.db.Ping() }

// Snapshot writes a consistent copy of the database to path, which must not
// exist yet.
func (s *DB) Snapshot(path string) error {
	_, err := s.db.Exec("VACUUM INTO ?", path)
	return err
}

func (s *DB) Load(ctx context.Context) (Authority, error) {
	var a Authority
	var highWater int64
	var provisional, deadline sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT d.mode, d.installation, coalesce(d.recovery_episode, ''), d.high_water,
			coalesce(b.confirmed, 0), b.provisional, b.deadline
		FROM deployment d, base b WHERE d.id = 1 AND b.id = 1`).
		Scan(&a.Mode, &a.Installation, &a.Episode, &highWater, &a.Confirmed, &provisional, &deadline)
	if err != nil {
		return Authority{}, fmt.Errorf("%w: reading base state: %v", ErrUnavailable, err)
	}
	a.HighWater = time.UnixMilli(highWater)
	a.Revision = a.Confirmed
	if provisional.Valid {
		a.Provisional = provisional.Int64
		a.Revision = provisional.Int64
		a.Deadline = time.UnixMilli(deadline.Int64)
	}
	if err := s.checkActivation(ctx, &a); err != nil {
		return Authority{}, err
	}
	a.Overrides, err = s.Overrides(ctx, `WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return Authority{}, fmt.Errorf("%w: reading overrides: %v", ErrUnavailable, err)
	}
	if a.Mode != ModeNormal {
		return a, nil
	}
	if a.Confirmed == 0 {
		return Authority{}, fmt.Errorf("%w: normal mode without a confirmed base", ErrUnavailable)
	}
	var checksum string
	if err := s.db.QueryRowContext(ctx, `SELECT document, checksum FROM revision WHERE id = ?`, a.Revision).
		Scan(&a.Document, &checksum); err != nil {
		return Authority{}, fmt.Errorf("%w: reading revision %d: %v", ErrUnavailable, a.Revision, err)
	}
	if DocumentChecksum(a.Document) != checksum {
		return Authority{}, fmt.Errorf("%w: revision %d checksum mismatch", ErrUnavailable, a.Revision)
	}
	return a, nil
}
