// The blocked-accounts list and the control changes that edit it.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/Alex-Zechariah-02/apiward/internal/fault"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

type BlockedAccount struct {
	Subject   string    `json:"subject"`
	Reason    string    `json:"reason"`
	BlockedBy string    `json:"blocked_by"`
	BlockedAt time.Time `json:"blocked_at"`
}

// BlockedAccounts is immutable after publication, independently of policy.
type BlockedAccounts map[string]BlockedAccount

// SQLite bounds UTF-8 bytes because its TEXT length stops at NUL. Both HTTP
// input and durable state enforce the contract's Unicode character bound.
func ValidBlockReason(reason string) bool {
	count := utf8.RuneCountInString(reason)
	return utf8.ValidString(reason) && count >= 1 && count <= 200
}

func (s *DB) BlockedAccounts(ctx context.Context) (BlockedAccounts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, reason, blocked_by, blocked_at FROM blocked_account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := BlockedAccounts{}
	for rows.Next() {
		var entry BlockedAccount
		var at int64
		if err := rows.Scan(&entry.Subject, &entry.Reason, &entry.BlockedBy, &at); err != nil {
			return nil, err
		}
		if !policy.IsSubjectID(entry.Subject) || !IsAdminName(entry.BlockedBy) || !ValidBlockReason(entry.Reason) {
			return nil, errors.New("invalid blocked account")
		}
		entry.BlockedAt = time.UnixMilli(at).UTC()
		result[entry.Subject] = entry
	}
	return result, rows.Err()
}

// CommitControl keeps the state change, transition and exact replay response in
// the same SQLite transaction. The callback changes only controller-owned state.
func (s *DB) CommitControl(ctx context.Context, t TransitionRecord, body json.RawMessage, change func(*sql.Tx) error, now time.Time) error {
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
	fault.CrashPoint("commit-partway")
	if err := insertCommitted(ctx, tx, t, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_result (transition, body) VALUES (?, ?)`, t.ID, []byte(body)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DB) ControlResult(ctx context.Context, id string) (json.RawMessage, error) {
	var result []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM control_result WHERE transition = ?`, id).Scan(&result)
	return json.RawMessage(result), err
}
