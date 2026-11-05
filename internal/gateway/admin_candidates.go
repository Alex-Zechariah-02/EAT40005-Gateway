// Admin API candidate staging and confirmed activation handlers.

package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// candidateView marks every candidate as not active: staging never changes
// the enforced policy.
type candidateView struct {
	candidateRecord
	Active   bool            `json:"active"`
	Document json.RawMessage `json:"document,omitempty"`
}

func (c *Controller) handleCandidates(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	records, err := c.store.candidates(r.Context(), cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	items := make([]candidateView, 0, len(records))
	for _, record := range records {
		items = append(items, candidateView{candidateRecord: record})
	}
	result := list{Items: items}
	if len(records) == limit {
		result.Next = strconv.FormatInt(records[len(records)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

func (c *Controller) handleCandidate(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	record, found, err := c.store.candidate(r.Context(), id)
	switch {
	case err != nil:
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
	case !found:
		adminError(w, http.StatusNotFound, "not_found", "")
	default:
		writeJSON(w, http.StatusOK, candidateView{candidateRecord: record, Document: record.Document})
	}
}

// handlePostCandidate validates and stores a complete policy candidate. The
// If-Match precondition names the confirmed revision it was prepared against.
func (c *Controller) handlePostCandidate(w http.ResponseWriter, r *http.Request, who caller) {
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if !decodeBody(w, r, maxCandidateBody, &body) {
		return
	}
	var compact bytes.Buffer
	if json.Compact(&compact, body.Policy) != nil {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	t := adminTransition(who, key, "candidate.create", "candidate", expected, compact.String())
	outcome, status := c.StageCandidate(r.Context(), t, body.Policy)
	if status != http.StatusOK {
		writeJSON(w, status, outcome)
		return
	}
	record, _, err := c.store.candidate(r.Context(), outcome.Candidate)
	if err != nil {
		c.logger.Error("reading committed candidate", "error", err)
		outcome.Reason = "STORE_ERROR"
		writeJSON(w, http.StatusServiceUnavailable, outcome)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"receipt": outcome, "candidate": candidateView{candidateRecord: record}})
}

func (c *Controller) handleWithdrawCandidate(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	t := adminTransition(who, key, "candidate.withdraw", fmt.Sprintf("candidate %d", id), sql.NullInt64{}, nil)
	outcome, status := c.WithdrawCandidate(r.Context(), t, id)
	writeJSON(w, status, outcome)
}

// handleEstablish confirms a deny-all candidate as the first base (genesis)
// or the base that ends a recovery. The checksum shows the administrator
// confirmed the exact document.
func (c *Controller) handleEstablish(kind string) adminHandler {
	return func(w http.ResponseWriter, r *http.Request, who caller) {
		var body struct {
			Candidate int64  `json:"candidate"`
			Checksum  string `json:"checksum"`
		}
		if !decodeBody(w, r, maxAdminBody, &body) {
			return
		}
		outcome, status := c.EstablishBase(r.Context(), kind, who.admin.Name, body.Candidate, body.Checksum)
		writeJSON(w, status, outcome)
	}
}

// handleActivate makes a staged candidate the provisional active base. The
// If-Match precondition names the confirmed revision that becomes the
// rollback target; the checksum shows the administrator chose the exact
// document.
func (c *Controller) handleActivate(w http.ResponseWriter, r *http.Request, who caller) {
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Candidate            int64  `json:"candidate"`
		Checksum             string `json:"checksum"`
		ConfirmWithinSeconds int64  `json:"confirm_within_seconds"`
	}
	if !decodeBody(w, r, maxAdminBody, &body) {
		return
	}
	t := adminTransition(who, key, "cpa.activate", fmt.Sprintf("candidate %d", body.Candidate), expected, body)
	outcome, status := c.Activate(r.Context(), t, body.Candidate, body.Checksum, body.ConfirmWithinSeconds)
	c.writePolicyOutcome(w, r, outcome, status)
}

// handleResolve confirms or rolls back the provisional revision named in the
// body.
func (c *Controller) handleResolve(kind, disposition string) adminHandler {
	return func(w http.ResponseWriter, r *http.Request, who caller) {
		expected, ok := ifMatch(w, r)
		if !ok {
			return
		}
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		var body struct {
			Revision int64 `json:"revision"`
		}
		if !decodeBody(w, r, maxAdminBody, &body) {
			return
		}
		t := adminTransition(who, key, kind, fmt.Sprintf("revision %d", body.Revision), expected, body)
		outcome, status := c.Resolve(r.Context(), t, body.Revision, disposition)
		c.writePolicyOutcome(w, r, outcome, status)
	}
}

// handleProvisional reports the current provisional base: its activation
// record, the generation serving it and every control target it changes
// relative to its rollback target.
func (c *Controller) handleProvisional(w http.ResponseWriter, r *http.Request, who caller) {
	ctx := r.Context()
	a, err := c.store.load(ctx)
	if err != nil {
		c.logger.Error("reading authority", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	w.Header().Set("ETag", policyETag(a.confirmed))
	if a.provisional == 0 {
		adminError(w, http.StatusNotFound, "no_provisional", "")
		return
	}
	record, found, err := c.store.activation(ctx, "revision", a.provisional)
	if err == nil && !found {
		err = fmt.Errorf("activation of revision %d missing", a.provisional)
	}
	var before, after policy.Policy
	if err == nil {
		before, err = c.storedPolicy(ctx, a.confirmed)
	}
	if err == nil {
		after, err = c.storedPolicy(ctx, a.provisional)
	}
	if err != nil {
		c.logger.Error("reading the provisional base", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	view := map[string]any{"activation": record, "changes": compile.PolicyChanges(before, after)}
	if g := c.current.Load(); g != nil && g.Revision == a.provisional {
		view["generation"] = g.ID
	}
	writeJSON(w, http.StatusOK, view)
}

func (c *Controller) storedPolicy(ctx context.Context, id int64) (policy.Policy, error) {
	r, found, err := c.store.revision(ctx, id)
	if err == nil && !found {
		err = fmt.Errorf("revision %d missing", id)
	}
	if err != nil {
		return policy.Policy{}, err
	}
	return policy.DecodePolicy(r.Document)
}

// handleActivations lists every provisional base with its final disposition.
func (c *Controller) handleActivations(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	items, err := c.store.activations(r.Context(), cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	result := list{Items: items}
	if len(items) == limit {
		result.Next = strconv.FormatInt(items[len(items)-1].Revision, 10)
	}
	writeJSON(w, http.StatusOK, result)
}
