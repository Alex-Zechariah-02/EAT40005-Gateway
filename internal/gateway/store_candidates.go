// Candidate and activation reads.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/fault"
)

func (s *store) candidate(ctx context.Context, id int64) (candidateRecord, bool, error) {
	if err := fault.CandidateReadFault(); err != nil {
		return candidateRecord{}, false, err
	}
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
