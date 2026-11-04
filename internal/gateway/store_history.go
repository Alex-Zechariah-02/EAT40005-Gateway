// Revision and change history reads.

package gateway

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

// historyFilter selects and orders a history listing. Empty members select
// everything.
type historyFilter struct {
	newestFirst bool
	kinds       []string
	outcome     string
	actor       string
}

// history lists up to limit transitions after the cursor in the filter's
// order. Transitions are never deleted and each new one gets a larger rowid,
// so a newest-first listing that pages from its first answer never skips or
// repeats an entry; entries recorded later appear only in a new listing.
func (s *store) history(ctx context.Context, filter historyFilter, after int64, limit int) ([]historyEntry, error) {
	conditions := []string{}
	args := []any{}
	switch {
	case !filter.newestFirst:
		conditions = append(conditions, "rowid > ?")
		args = append(args, after)
	case after > 0:
		conditions = append(conditions, "rowid < ?")
		args = append(args, after)
	}
	if len(filter.kinds) > 0 {
		conditions = append(conditions, "kind IN (?"+strings.Repeat(", ?", len(filter.kinds)-1)+")")
		for _, kind := range filter.kinds {
			args = append(args, kind)
		}
	}
	if filter.outcome != "" {
		conditions = append(conditions, "outcome = ?")
		args = append(args, filter.outcome)
	}
	if filter.actor != "" {
		conditions = append(conditions, "actor = ?")
		args = append(args, filter.actor)
	}
	query := `SELECT ` + historyColumns + ` FROM transition`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	if filter.newestFirst {
		query += ` ORDER BY rowid DESC LIMIT ?`
	} else {
		query += ` ORDER BY rowid LIMIT ?`
	}
	rows, err := s.db.QueryContext(ctx, query, append(args, limit)...)
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
