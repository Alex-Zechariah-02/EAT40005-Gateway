// Transactional commits and publication and audit markers.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

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
