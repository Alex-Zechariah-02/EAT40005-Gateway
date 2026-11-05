// Candidate and activation reads.

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/fault"
)

func (s *DB) Candidate(ctx context.Context, id int64) (CandidateRecord, bool, error) {
	if err := fault.CandidateReadFault(); err != nil {
		return CandidateRecord{}, false, err
	}
	var c CandidateRecord
	var parent sql.NullInt64
	var created int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, parent, document, checksum, deny_all, status, actor, created_at FROM candidate WHERE id = ?`, id).
		Scan(&c.ID, &parent, &c.Document, &c.Checksum, &c.DenyAll, &c.Status, &c.Actor, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateRecord{}, false, nil
	}
	c.Parent, c.CreatedAt = parent.Int64, time.UnixMilli(created).UTC()
	return c, err == nil, err
}

func (s *DB) Candidates(ctx context.Context, after int64, limit int) ([]CandidateRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, parent, checksum, deny_all, status, actor, created_at FROM candidate
		WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []CandidateRecord{}
	for rows.Next() {
		var c CandidateRecord
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

const ActivationColumns = `revision, candidate, rollback_target, deadline, activated_at, actor,
	coalesce(disposition, ''), resolved_at, coalesce(resolved_by, ''), coalesce(resolution, '')`

// Activation reads the activation record selected by column, which is
// revision or candidate.
func (s *DB) Activation(ctx context.Context, column string, id int64) (ActivationRecord, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ActivationColumns+` FROM activation WHERE `+column+` = ?`, id)
	if err != nil {
		return ActivationRecord{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return ActivationRecord{}, false, rows.Err()
	}
	record, err := ScanActivation(rows)
	return record, err == nil, err
}

func (s *DB) Activations(ctx context.Context, after int64, limit int) ([]ActivationRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ActivationColumns+` FROM activation WHERE revision > ? ORDER BY revision LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []ActivationRecord{}
	for rows.Next() {
		record, err := ScanActivation(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, record)
	}
	return list, rows.Err()
}

func ScanActivation(rows *sql.Rows) (ActivationRecord, error) {
	var r ActivationRecord
	var deadline, activated int64
	var resolved sql.NullInt64
	err := rows.Scan(&r.Revision, &r.Candidate, &r.RollbackTarget, &deadline, &activated, &r.Actor,
		&r.Disposition, &resolved, &r.ResolvedBy, &r.Resolution)
	if err != nil {
		return ActivationRecord{}, err
	}
	r.Deadline = time.UnixMilli(deadline).UTC()
	r.ActivatedAt = time.UnixMilli(activated).UTC()
	r.ResolvedAt = optionalTime(resolved)
	return r, nil
}
