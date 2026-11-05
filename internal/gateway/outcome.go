// Change outcomes, rejections and their audit records.

package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// outcomeKind names the outcome event of a committed transition.
func outcomeKind(t transitionRecord) string {
	switch t.Kind {
	case "account.block", "account.unblock", "keys.reload":
		return t.Kind
	case "cpa.activate":
		return "cpa.activated"
	case "cpa.confirm":
		return "cpa.confirmed"
	case "cpa.rollback":
		return "cpa.rolled_back"
	case "cpa.timeout":
		return "cpa.timed_out"
	case "tpo.create":
		return "tpo.created"
	case "tpo.revoke":
		return "tpo.revoked"
	case "tpo.expire":
		return "tpo.expired"
	}
	if t.Revision != 0 {
		return "transition.published"
	}
	return "transition.committed"
}

func (c *Controller) commitAudit(ctx context.Context, e Event) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.AuditTimeout)
	defer cancel()
	return c.audit.Commit(ctx, e)
}

// Outcome is the controller receipt for a mutation. For a provisional base,
// Revision is the active base after the transition, Provisional the revision
// the lifecycle concerns and RollbackTarget the confirmed revision it returns
// to.
type Outcome struct {
	Transition     string    `json:"transition_id"`
	Outcome        string    `json:"outcome"`
	Reason         string    `json:"reason,omitempty"`
	Field          string    `json:"field,omitempty"`
	Revision       int64     `json:"revision,omitempty"`
	Generation     string    `json:"generation,omitempty"`
	Candidate      int64     `json:"candidate,omitempty"`
	Provisional    int64     `json:"provisional,omitempty"`
	RollbackTarget int64     `json:"rollback_target,omitempty"`
	Deadline       time.Time `json:"deadline,omitzero"`
	Disposition    string    `json:"disposition,omitempty"`
	// For a temporary override: its identifier, the control and target it
	// governs and its expiry time. Override also names the active override a
	// refused candidate conflicts with.
	Override int64     `json:"override,omitempty"`
	Target   string    `json:"target,omitempty"`
	Expires  time.Time `json:"expires_at,omitzero"`
}

func unavailableOutcome(id string) (Outcome, int) {
	return Outcome{Transition: id, Outcome: "rejected", Reason: "UNAVAILABLE"}, http.StatusServiceUnavailable
}

// auditUnavailable refuses a mutation whose intent event could not be
// recorded; nothing was committed.
func auditUnavailable(id string) (Outcome, int) {
	return Outcome{Transition: id, Outcome: "rejected", Reason: "AUDIT_UNAVAILABLE"}, http.StatusServiceUnavailable
}

// reject refuses a mutation and records the refusal under its identity, so a
// retry returns the same answer. Server-chosen identities (genesis, recovery,
// enrolment) are not recorded: one refused attempt must not block the
// transition they name.
func (c *Controller) reject(ctx context.Context, t transitionRecord, reason, field string, now time.Time) (Outcome, int) {
	if !serverIdentity(t.ID) {
		t.Reason = reason
		if err := c.store.recordRejection(ctx, t, now); err != nil {
			return c.storeFailure(t.ID, err)
		}
	}
	kind := "transition.rejected"
	switch {
	case strings.HasPrefix(t.Kind, "cpa."):
		kind = "cpa.rejected"
	case strings.HasPrefix(t.Kind, "tpo."):
		kind = "tpo.rejected"
	}
	c.audit.Record(Event{Kind: kind, Transition: t.ID, Actor: t.Actor, Reason: reason, Candidate: t.Candidate, Override: t.Override})
	return Outcome{Transition: t.ID, Outcome: "rejected", Reason: reason, Field: field, Override: t.Override}, rejectionStatus(reason)
}

func serverIdentity(id string) bool {
	return strings.HasPrefix(id, "genesis:") || strings.HasPrefix(id, "recovery:") ||
		strings.HasPrefix(id, "enrol:") || strings.HasPrefix(id, "auto-rollback:") || strings.HasPrefix(id, "auto-expire:")
}

func rejectionStatus(reason string) int {
	switch reason {
	case "STALE", "PROVISIONAL_ACTIVE", "INVALID_STATE", "CANDIDATE_UNAVAILABLE", "CANDIDATE_STALE",
		"CANDIDATE_INTEGRITY", "NO_PROVISIONAL", "PROVISIONAL_MISMATCH", "ALREADY_RESOLVED", "DEADLINE_PASSED",
		"OVERRIDE_CONFLICT", "OVERRIDE_LIMIT":
		return http.StatusConflict
	case "PRECONDITION_FAILED":
		return http.StatusPreconditionFailed
	case "NOT_FOUND":
		return http.StatusNotFound
	case "TRANSITION_ID_REUSED":
		return http.StatusUnprocessableEntity
	case "KEYS_INVALID":
		return http.StatusUnprocessableEntity
	case "TARGET_INVALID":
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

func policyRejection(err error) (string, string) {
	if policyErr, ok := errors.AsType[*policy.PolicyError](err); ok {
		return policyErr.Code, policyErr.Field
	}
	return "SCHEMA_INVALID", ""
}

func (c *Controller) storeFailure(id string, err error) (Outcome, int) {
	c.logger.Error("control-plane database failure", "error", err)
	return Outcome{Transition: id, Outcome: "rejected", Reason: "STORE_ERROR"}, http.StatusServiceUnavailable
}
