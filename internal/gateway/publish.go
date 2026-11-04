// Policy generation publication and change requests.

package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// publishAuthority compiles committed authority, the active base with its
// active overrides, and swaps it in.
func (c *Controller) publishAuthority(a authority) bool {
	g, err := authorityGeneration(a, a.overrides)
	if err != nil {
		c.unavailable(fmt.Sprintf("stored revision %d with overrides %v: %v", a.revision, overrideIDs(a.overrides), err))
		return false
	}
	c.current.Store(g)
	c.state = modeNormal
	c.scheduleDeadline(g)
	return true
}

// authorityGeneration compiles the active base of a with the given overrides,
// keeping a provisional base's deadline.
func authorityGeneration(a authority, overrides []override) (*Generation, error) {
	p, err := DecodePolicy(a.document)
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if a.provisional != 0 {
		deadline = a.deadline
	}
	return effectiveGeneration(p, a.revision, deadline, overrides)
}

func overrideIDs(overrides []override) []int64 {
	ids := make([]int64, 0, len(overrides))
	for _, o := range overrides {
		ids = append(ids, o.ID)
	}
	return ids
}

// compileEffective compiles a stored revision with the given overrides.
func (c *Controller) compileEffective(ctx context.Context, revision int64, deadline time.Time, overrides []override) (*Generation, error) {
	p, err := c.storedPolicy(ctx, revision)
	if err != nil {
		return nil, err
	}
	return effectiveGeneration(p, revision, deadline, overrides)
}

// publish applies a complete proposed policy: compile it with the
// active overrides, record the audit intent, commit it as the new active base,
// swap the generation in, and record the publication receipt and the durable
// outcome event. Callers have checked that the policy changes no target the
// overrides depend on.
func (c *Controller) publish(ctx context.Context, t transitionRecord, policy Policy, change revisionChange, overrides []override, now time.Time) (Outcome, int) {
	if _, err := effectiveGeneration(policy, 0, change.deadline, overrides); err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}

	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		return auditUnavailable(t.ID)
	}
	crashPoint("before-commit")

	change.document, _ = json.Marshal(policy)
	revision, err := c.store.commitRevision(ctx, t, change, now)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	crashPoint("after-commit")

	g, err := effectiveGeneration(policy, revision, change.deadline, overrides)
	if err != nil {
		c.unavailable(fmt.Sprintf("compiling committed revision %d: %v", revision, err))
		return Outcome{Transition: t.ID, Outcome: "committed", Reason: "PUBLICATION_FAILED"}, http.StatusInternalServerError
	}
	c.current.Store(g)
	c.state = modeNormal
	c.scheduleDeadline(g)
	crashPoint("after-swap")

	if err := c.store.markPublished(ctx, t.ID, "publisher", now); err != nil {
		c.logger.Error("recording publication receipt", "error", err)
	}
	crashPoint("after-receipt")

	t.Revision = revision
	result := Outcome{Transition: t.ID, Outcome: "committed", Revision: revision, Generation: g.ID, Candidate: change.candidate}
	event := Event{Kind: outcomeKind(t), Transition: t.ID, Actor: t.Actor, Revision: revision, Generation: g.ID, Candidate: change.candidate}
	if !change.deadline.IsZero() {
		result.Provisional, result.RollbackTarget, result.Deadline = revision, change.parent, change.deadline.UTC()
		event.Provisional, event.RollbackTarget, event.Deadline = revision, change.parent, change.deadline.UTC()
	}
	return c.finish(ctx, result, event)
}

// finish durably records a committed mutation's outcome event and
// returns the receipt. If the audit log cannot acknowledge it, the mutation
// stays committed and is reported as such, with the outcome event pending;
// the audit log is marked failed, so protected admission stops until a
// restart, whose reconciliation records the missing outcome event.
func (c *Controller) finish(ctx context.Context, result Outcome, event Event) (Outcome, int) {
	cancelPoint(ctx, "cancel-before-outcome")
	if err := c.commitAudit(ctx, event); err != nil {
		c.logger.Error("outcome event not recorded", "transition", result.Transition, "error", err)
		c.audit.fail(err)
		result.Reason = "OUTCOME_AUDIT_PENDING"
		return result, http.StatusServiceUnavailable
	}
	if err := c.store.markAudited(ctx, result.Transition, trustedNow()); err != nil {
		c.logger.Error("recording audit receipt", "error", err)
	}
	crashPoint("after-outcome")
	return result, http.StatusOK
}

// Change is one whole-policy publication, used by the test publication hook.
// The transition ID makes retries safe: a repeated ID returns the recorded
// outcome instead of applying again.
type Change struct {
	ID       string          `json:"transition_id"`
	Expected int64           `json:"expected_revision"`
	Policy   json.RawMessage `json:"policy"`
}

// Publish applies a complete policy as the new confirmed base. Production
// binaries expose no route to it: whole-policy activation belongs to the
// confirmed-activation lifecycle, and only the test publication hook calls it.
// It cannot create a provisional base.
func (c *Controller) Publish(ctx context.Context, actor string, change Change) (Outcome, int) {
	validID := change.ID != "" &&
		len(change.ID) <= maxTransitionID &&
		!strings.Contains(change.ID, ":")
	if !validID {
		return Outcome{Transition: change.ID, Outcome: "rejected", Reason: "SCHEMA_INVALID"}, http.StatusBadRequest
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if c.store == nil || c.state != modeNormal {
		return unavailableOutcome(change.ID)
	}
	t := transitionRecord{
		ID:       change.ID,
		Kind:     "publish",
		Actor:    actor,
		Target:   "policy",
		Expected: sql.NullInt64{Int64: change.Expected, Valid: true},
		Digest:   changeDigest(change),
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	if !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	if a.revision != change.Expected {
		return c.reject(ctx, t, "STALE", "", now)
	}
	if a.provisional != 0 {
		return c.reject(ctx, t, "PROVISIONAL_ACTIVE", "", now)
	}
	policy, err := DecodePolicy(change.Policy)
	if err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}
	if result, status, conflict := c.rejectConflict(ctx, t, a, policy, now); conflict {
		return result, status
	}
	return c.publish(ctx, t, policy, revisionChange{parent: a.revision, origin: "publish", actor: actor, mode: modeNormal}, a.overrides, now)
}

// rejectConflict refuses, with the active override it conflicts with, a whole
// policy that would replace the active base of a while changing or
// invalidating an active override. It reports whether it refused.
func (c *Controller) rejectConflict(ctx context.Context, t transitionRecord, a authority, next Policy, now time.Time) (Outcome, int, bool) {
	current, err := DecodePolicy(a.document)
	if err != nil {
		result, status := c.storeFailure(t.ID, err)
		return result, status, true
	}
	o := overrideConflict(current, next, a.overrides)
	if o == nil {
		return Outcome{}, 0, false
	}
	t.Override = o.ID
	result, status := c.reject(ctx, t, "OVERRIDE_CONFLICT", "", now)
	result.Target = o.Target
	return result, status, true
}

func changeDigest(change Change) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%d|", change.Expected)
	sum.Write(change.Policy)
	return hex.EncodeToString(sum.Sum(nil))
}

// requestDigest identifies a mutation request by its kind, target, required
// revision and canonical payload.
func requestDigest(kind, target string, expected sql.NullInt64, payload any) string {
	canonical, _ := json.Marshal(payload)
	sum := sha256.New()
	fmt.Fprintf(sum, "%s|%s|%v|%d|", kind, target, expected.Valid, expected.Int64)
	sum.Write(canonical)
	return hex.EncodeToString(sum.Sum(nil))
}
