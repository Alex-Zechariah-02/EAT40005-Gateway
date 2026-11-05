// Revision and change history reads.

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *DB) Revision(ctx context.Context, id int64) (RevisionInfo, bool, error) {
	var r RevisionInfo
	var parent sql.NullInt64
	var created int64
	var document []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, parent, origin, checksum, actor, created_at, document FROM revision WHERE id = ?`, id).
		Scan(&r.ID, &parent, &r.Origin, &r.Checksum, &r.Actor, &created, &document)
	if errors.Is(err, sql.ErrNoRows) {
		return RevisionInfo{}, false, nil
	}
	if err != nil {
		return RevisionInfo{}, false, err
	}
	if DocumentChecksum(document) != r.Checksum {
		return RevisionInfo{}, false, fmt.Errorf("revision %d checksum mismatch", id)
	}
	r.Parent, r.CreatedAt, r.Document = parent.Int64, time.UnixMilli(created).UTC(), document
	return r, true, nil
}

func (s *DB) Revisions(ctx context.Context, after int64, limit int) ([]RevisionInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, parent, origin, checksum, actor, created_at FROM revision WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []RevisionInfo{}
	for rows.Next() {
		var r RevisionInfo
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

// HistoryFilter selects and orders a history listing. Empty members select
// everything.
type HistoryFilter struct {
	NewestFirst bool
	Kinds       []string
	Outcome     string
	Actor       string
}

// History lists up to limit transitions after the cursor in the filter's
// order. Transitions are never deleted and each new one gets a larger rowid,
// so a newest-first listing that pages from its first answer never skips or
// repeats an entry; entries recorded later appear only in a new listing.
func (s *DB) History(ctx context.Context, filter HistoryFilter, after int64, limit int) ([]HistoryEntry, error) {
	conditions := []string{}
	args := []any{}
	switch {
	case !filter.NewestFirst:
		conditions = append(conditions, "rowid > ?")
		args = append(args, after)
	case after > 0:
		conditions = append(conditions, "rowid < ?")
		args = append(args, after)
	}
	if len(filter.Kinds) > 0 {
		conditions = append(conditions, "kind IN (?"+strings.Repeat(", ?", len(filter.Kinds)-1)+")")
		for _, kind := range filter.Kinds {
			args = append(args, kind)
		}
	}
	if filter.Outcome != "" {
		conditions = append(conditions, "outcome = ?")
		args = append(args, filter.Outcome)
	}
	if filter.Actor != "" {
		conditions = append(conditions, "actor = ?")
		args = append(args, filter.Actor)
	}
	query := `SELECT ` + historyColumns + ` FROM transition`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	if filter.NewestFirst {
		query += ` ORDER BY rowid DESC LIMIT ?`
	} else {
		query += ` ORDER BY rowid LIMIT ?`
	}
	rows, err := s.db.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []HistoryEntry{}
	for rows.Next() {
		h, err := scanHistory(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, h)
	}
	return list, rows.Err()
}

func (s *DB) HistoryEntry(ctx context.Context, id string) (HistoryEntry, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+historyColumns+` FROM transition WHERE id = ?`, id)
	if err != nil {
		return HistoryEntry{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return HistoryEntry{}, false, rows.Err()
	}
	h, err := scanHistory(rows)
	return h, err == nil, err
}

func scanHistory(rows *sql.Rows) (HistoryEntry, error) {
	var h HistoryEntry
	var expected, revision, candidate, published, audited sql.NullInt64
	var committed int64
	err := rows.Scan(&h.Seq, &h.ID, &h.Kind, &h.Actor, &h.Target, &expected, &h.Outcome, &h.Reason,
		&revision, &candidate, &committed, &published, &h.PublishedBy, &audited)
	if err != nil {
		return HistoryEntry{}, err
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

func DocumentChecksum(document []byte) string {
	sum := sha256.Sum256(document)
	return hex.EncodeToString(sum[:])
}
