// Offline migration of older databases.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

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
