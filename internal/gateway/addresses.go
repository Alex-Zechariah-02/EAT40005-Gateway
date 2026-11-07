// The blocked address list: the ingress check of every data-plane request
// and the Admin API that changes the list at once, without a policy change.

package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// admitAddress refuses a request whose client address an active entry of
// the blocked address list covers (403 address_blocked at INGRESS). An
// address that does not parse is refused while any entry exists, since it
// cannot be shown not to match. The list is unset only when the store could
// not be opened; no policy is then published, so CAPTURE refuses the request.
func (h *Handler) admitAddress(q *request) bool {
	list := h.controller.addresses.Load()
	if list == nil || len(*list) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(q.event.ClientAddress)
	if err != nil {
		q.event.Reason = "invalid_peer_address"
		q.reject(audit.StageIngress, http.StatusBadRequest, "bad_request")
		return false
	}
	if list.Blocks(addr, compile.TrustedNow()) {
		q.event.Reason = "address_blocked"
		q.reject(audit.StageIngress, http.StatusForbidden, "address_blocked")
		return false
	}
	return true
}

func (c *Controller) handleBlockedAddresses(w http.ResponseWriter, r *http.Request, who caller) {
	current := c.addresses.Load()
	if current == nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	now := compile.TrustedNow()
	items := []store.BlockedAddress{}
	for _, entry := range *current {
		if entry.Active(now) {
			items = append(items, entry)
		}
	}
	slices.SortFunc(items, func(a, b store.BlockedAddress) int { return strings.Compare(a.Range, b.Range) })
	writeJSON(w, http.StatusOK, list{Items: items})
}

// addressRange reads the {range} path value: a CIDR range in canonical form,
// IPv4-mapped IPv6 accepted as the IPv4 range it covers.
func addressRange(w http.ResponseWriter, r *http.Request) (netip.Prefix, bool) {
	prefix, ok := store.BlockRange(r.PathValue("range"))
	if !ok {
		adminError(w, http.StatusBadRequest, "bad_request", "range")
	}
	return prefix, ok
}

func (c *Controller) handleBlockAddress(w http.ResponseWriter, r *http.Request, who caller) {
	prefix, ok := addressRange(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var request struct {
		Reason    string     `json:"reason"`
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
	}
	if !decodeBody(w, r, maxAdminBody, &request) {
		return
	}
	if !store.ValidBlockReason(request.Reason) {
		adminError(w, http.StatusBadRequest, "bad_request", "reason")
		return
	}
	if request.ExpiresAt != nil {
		ahead := time.Until(*request.ExpiresAt)
		if ahead < store.MinAddressExpiry || ahead > store.MaxAddressExpiry {
			adminError(w, http.StatusBadRequest, "bad_request", "expires_at")
			return
		}
		at := request.ExpiresAt.UTC().Truncate(time.Millisecond)
		request.ExpiresAt = &at
	}
	t := adminTransition(who, key, "address.block", prefix.String(), sql.NullInt64{}, request)
	body, status := c.changeAddress(r.Context(), t, store.BlockedAddress{Range: prefix.String(), Prefix: prefix, Reason: request.Reason, ExpiresAt: request.ExpiresAt})
	writeJSON(w, status, body)
}

func (c *Controller) handleUnblockAddress(w http.ResponseWriter, r *http.Request, who caller) {
	prefix, ok := addressRange(w, r)
	if !ok || !noRequestBody(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	t := adminTransition(who, key, "address.unblock", prefix.String(), sql.NullInt64{}, nil)
	body, status := c.changeAddress(r.Context(), t, store.BlockedAddress{Range: prefix.String(), Prefix: prefix})
	writeJSON(w, status, body)
}

// changeAddress blocks or unblocks one range. Expired entries are dropped
// from the list and the store by the same write; an expired entry is not
// found. Blocking again with the same reason and expiry keeps the entry.
func (c *Controller) changeAddress(ctx context.Context, t store.TransitionRecord, entry store.BlockedAddress) (json.RawMessage, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if body, status, done := c.replayControl(ctx, t); done {
		return body, status
	}
	now := compile.TrustedNow().UTC().Truncate(time.Millisecond)
	current := c.addresses.Load()
	if current == nil {
		return controlError("gateway_unavailable"), http.StatusServiceUnavailable
	}
	next := maps.Clone(*current)
	maps.DeleteFunc(next, func(_ string, e store.BlockedAddress) bool { return !e.Active(now) })
	previous, found := next[entry.Range]
	var result any
	var change func(*sql.Tx) error
	if t.Kind == "address.block" {
		if !found && len(next) >= store.MaxBlockedAddresses {
			return c.rejectControl(ctx, t, "LIMIT_EXCEEDED", now)
		}
		entry.BlockedBy, entry.BlockedAt = t.Actor, now
		if found && previous.Reason == entry.Reason && slices.Equal(expiry(previous), expiry(entry)) {
			entry = previous
		}
		next[entry.Range] = entry
		result = entry
		change = func(tx *sql.Tx) error { return store.SaveBlockedAddress(ctx, tx, entry, now) }
	} else {
		if !found {
			return c.rejectControl(ctx, t, "NOT_FOUND", now)
		}
		delete(next, entry.Range)
		result = map[string]any{"range": entry.Range, "unblocked": true}
		change = func(tx *sql.Tx) error { return store.DeleteBlockedAddress(ctx, tx, entry.Range, now) }
	}
	return c.finishControl(ctx, t, result, change, func() { c.addresses.Store(&next) }, now)
}

// expiry is an entry's expiry as a comparable list: empty when it has none.
func expiry(entry store.BlockedAddress) []time.Time {
	if entry.ExpiresAt == nil {
		return nil
	}
	return []time.Time{*entry.ExpiresAt}
}
