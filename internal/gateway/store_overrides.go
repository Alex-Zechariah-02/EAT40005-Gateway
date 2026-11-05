// Override reads and commits.

package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/fault"
)

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
	fault.CrashPoint("tpo-commit-partway")
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
