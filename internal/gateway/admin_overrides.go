// Admin API temporary policy override handlers.

package gateway

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
)

// handleOverrides reports the overrides active now and the effective policy
// they belong to: the active base revision, the confirmed and provisional
// revisions and the serving generation, whose identity names the base and
// every applied override.
func (c *Controller) handleOverrides(w http.ResponseWriter, r *http.Request, who caller) {
	a, err := c.store.load(r.Context())
	if err != nil {
		c.logger.Error("reading authority", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	view := map[string]any{
		"revision":  a.revision,
		"confirmed": a.confirmed,
		"overrides": append([]override{}, a.overrides...),
	}
	if a.provisional != 0 {
		view["provisional"] = a.provisional
	}
	if g := c.current.Load(); g != nil {
		view["generation"] = g.ID
		view["generation_overrides"] = append([]int64{}, g.Overrides...)
	}
	w.Header().Set("ETag", policyETag(a.confirmed))
	writeJSON(w, http.StatusOK, view)
}

func (c *Controller) handleOverride(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, found, err := c.store.override(r.Context(), id)
	switch {
	case err != nil:
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
	case !found:
		adminError(w, http.StatusNotFound, "not_found", "")
	default:
		writeJSON(w, http.StatusOK, o)
	}
}

// handleOverrideHistory lists every override with its final disposition.
func (c *Controller) handleOverrideHistory(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	items, err := c.store.overrides(r.Context(), `WHERE id > ? ORDER BY id LIMIT ?`, cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	result := list{Items: items}
	if len(items) == limit {
		result.Next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

// handleValidateOverride answers whether a proposed override would be
// created now. Nothing is created or recorded in the history.
func (c *Controller) handleValidateOverride(w http.ResponseWriter, r *http.Request, who caller) {
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	var request OverrideRequest
	if !decodeBody(w, r, maxAdminBody, &request) {
		return
	}
	outcome, status := c.ValidateOverride(r.Context(), who.admin.Name, expected.Int64, request)
	writeJSON(w, status, outcome)
}

// handleCreateOverride creates a temporary override of the confirmed base.
// The If-Match precondition names the confirmed revision the administrator
// saw.
func (c *Controller) handleCreateOverride(w http.ResponseWriter, r *http.Request, who caller) {
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var request OverrideRequest
	if !decodeBody(w, r, maxAdminBody, &request) {
		return
	}
	t := adminTransition(who, key, "tpo.create", "override", expected, request)
	outcome, status := c.CreateOverride(r.Context(), t, request)
	switch status {
	case http.StatusOK:
		status = http.StatusCreated
	case http.StatusPreconditionFailed:
		if a, err := c.store.load(r.Context()); err == nil && a.mode == modeNormal {
			w.Header().Set("ETag", policyETag(a.confirmed))
		}
	}
	writeJSON(w, status, outcome)
}

// handleRevokeOverride ends one active override.
func (c *Controller) handleRevokeOverride(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	t := adminTransition(who, key, "tpo.revoke", fmt.Sprintf("override %d", id), sql.NullInt64{}, nil)
	outcome, status := c.RevokeOverride(r.Context(), t, id)
	writeJSON(w, status, outcome)
}
