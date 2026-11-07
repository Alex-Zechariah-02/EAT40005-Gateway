// The blocked address list: client address ranges refused at ingress,
// changed by control transitions like the blocked accounts list.

package store

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// MaxBlockedAddresses bounds the list, which is checked on every request.
const MaxBlockedAddresses = 1024

// Bounds of an entry's optional expiry, from the time it is written.
const (
	MinAddressExpiry = time.Minute
	MaxAddressExpiry = 30 * 24 * time.Hour
)

// BlockedAddress is one blocked range. ExpiresAt nil means until unblocked.
type BlockedAddress struct {
	Range     string       `json:"range"`
	Reason    string       `json:"reason"`
	BlockedBy string       `json:"blocked_by"`
	BlockedAt time.Time    `json:"blocked_at"`
	ExpiresAt *time.Time   `json:"expires_at"`
	Prefix    netip.Prefix `json:"-"`
}

// Active reports whether the entry applies at now.
func (b BlockedAddress) Active(now time.Time) bool {
	return b.ExpiresAt == nil || now.Before(*b.ExpiresAt)
}

// BlockedAddresses is immutable after publication, keyed by range.
type BlockedAddresses map[string]BlockedAddress

// Blocks reports whether an active entry covers the address.
func (list BlockedAddresses) Blocks(addr netip.Addr, now time.Time) bool {
	addr = addr.Unmap()
	for _, entry := range list {
		if entry.Prefix.Contains(addr) && entry.Active(now) {
			return true
		}
	}
	return false
}

// BlockRange returns the canonical CIDR range of an entry key: host bits zero,
// IPv4-mapped IPv6 written as IPv4. A range not already in that form is
// refused, so one range has one stored key.
func BlockRange(text string) (netip.Prefix, bool) {
	prefix, ok := policy.ParseRange(text)
	if !ok {
		return netip.Prefix{}, false
	}
	if mapped, err := netip.ParsePrefix(text); err == nil && mapped.Addr().Is4In6() {
		return prefix, true
	}
	return prefix, prefix.String() == text
}

// BlockedAddresses reads the list, refusing rows the schema cannot express.
func (s *DB) BlockedAddresses(ctx context.Context) (BlockedAddresses, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cidr, reason, blocked_by, blocked_at, expires_at FROM blocked_address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := BlockedAddresses{}
	for rows.Next() {
		var entry BlockedAddress
		var at int64
		var expires sql.NullInt64
		if err := rows.Scan(&entry.Range, &entry.Reason, &entry.BlockedBy, &at, &expires); err != nil {
			return nil, err
		}
		prefix, ok := BlockRange(entry.Range)
		if !ok || prefix.String() != entry.Range || !IsAdminName(entry.BlockedBy) || !ValidBlockReason(entry.Reason) {
			return nil, errors.New("invalid blocked address")
		}
		entry.Prefix, entry.BlockedAt = prefix, time.UnixMilli(at).UTC()
		if expires.Valid {
			end := time.UnixMilli(expires.Int64).UTC()
			entry.ExpiresAt = &end
		}
		result[entry.Range] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) > MaxBlockedAddresses {
		return nil, errors.New("too many blocked addresses")
	}
	return result, nil
}

// SaveBlockedAddress writes one entry and removes expired ones in tx.
func SaveBlockedAddress(ctx context.Context, tx *sql.Tx, entry BlockedAddress, now time.Time) error {
	if err := dropExpired(ctx, tx, now); err != nil {
		return err
	}
	var expires sql.NullInt64
	if entry.ExpiresAt != nil {
		expires = sql.NullInt64{Int64: entry.ExpiresAt.UnixMilli(), Valid: true}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO blocked_address (cidr, reason, blocked_by, blocked_at, expires_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(cidr) DO UPDATE SET reason=excluded.reason, blocked_by=excluded.blocked_by, blocked_at=excluded.blocked_at, expires_at=excluded.expires_at`,
		entry.Range, entry.Reason, entry.BlockedBy, entry.BlockedAt.UnixMilli(), expires)
	return err
}

// DeleteBlockedAddress removes one entry and expired ones in tx.
func DeleteBlockedAddress(ctx context.Context, tx *sql.Tx, cidr string, now time.Time) error {
	if err := dropExpired(ctx, tx, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM blocked_address WHERE cidr = ?`, cidr)
	return err
}

func dropExpired(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM blocked_address WHERE expires_at IS NOT NULL AND expires_at <= ?`, now.UnixMilli())
	return err
}
