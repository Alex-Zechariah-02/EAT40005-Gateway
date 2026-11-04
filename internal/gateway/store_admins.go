// Administrator, enrolment and recovery records.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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
