package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"
)

type blockedAccount struct {
	Subject   string    `json:"subject"`
	Reason    string    `json:"reason"`
	BlockedBy string    `json:"blocked_by"`
	BlockedAt time.Time `json:"blocked_at"`
}

// blockedAccounts is immutable after publication, independently of policy.
type blockedAccounts map[string]blockedAccount

// SQLite bounds UTF-8 bytes because its TEXT length stops at NUL. Both HTTP
// input and durable state enforce the contract's Unicode character bound.
func validBlockReason(reason string) bool {
	count := utf8.RuneCountInString(reason)
	return utf8.ValidString(reason) && count >= 1 && count <= 200
}

func (s *store) blockedAccounts(ctx context.Context) (blockedAccounts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, reason, blocked_by, blocked_at FROM blocked_account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := blockedAccounts{}
	for rows.Next() {
		var entry blockedAccount
		var at int64
		if err := rows.Scan(&entry.Subject, &entry.Reason, &entry.BlockedBy, &at); err != nil {
			return nil, err
		}
		if !isSubjectID(entry.Subject) || !isAdminName(entry.BlockedBy) || !validBlockReason(entry.Reason) {
			return nil, errors.New("invalid blocked account")
		}
		entry.BlockedAt = time.UnixMilli(at).UTC()
		result[entry.Subject] = entry
	}
	return result, rows.Err()
}

// commitControl keeps the state change, transition and exact replay response in
// the same SQLite transaction. The callback changes only controller-owned state.
func (s *store) commitControl(ctx context.Context, t transitionRecord, body json.RawMessage, change func(*sql.Tx) error, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if change != nil {
		if err := change(tx); err != nil {
			return err
		}
	}
	crashPoint("commit-partway")
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_result (transition, body) VALUES (?, ?)`, t.ID, []byte(body)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) controlResult(ctx context.Context, id string) (json.RawMessage, error) {
	var result []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM control_result WHERE transition = ?`, id).Scan(&result)
	return json.RawMessage(result), err
}
