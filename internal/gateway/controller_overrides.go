// Temporary policy override proposal, creation, revocation and publication.

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// proposeOverride checks a proposed override against the current authority,
// in order: normal mode, no provisional base (none may be created while one
// is pending), the expected confirmed revision, the request itself, the
// bound on active overrides and a target no active override governs. It
// returns the override as it would be stored, or the refusal code and field;
// for a taken target it also returns the override holding it. Whether the
// effective policy compiles with it is checked by the caller.
func proposeOverride(a authority, expected int64, request OverrideRequest, actor string, now time.Time) (override, string, string, int64) {
	switch {
	case a.mode != modeNormal:
		return override{}, "INVALID_STATE", "", 0
	case a.provisional != 0:
		return override{}, "PROVISIONAL_ACTIVE", "", 0
	case a.confirmed != expected:
		return override{}, "PRECONDITION_FAILED", "", 0
	}
	o, err := request.check()
	if err != nil {
		code, field := policyRejection(err)
		return override{}, code, field, 0
	}
	if len(a.overrides) >= maxActiveOverrides {
		return override{}, "OVERRIDE_LIMIT", "", 0
	}
	for _, active := range a.overrides {
		if active.Target == o.Target {
			return override{}, "OVERRIDE_CONFLICT", "", active.ID
		}
	}
	// The database keeps milliseconds, so the running expiry and the one a
	// restart reads are the same instant.
	o.CreatedAt = time.UnixMilli(now.UnixMilli()).UTC()
	o.ExpiresAt = time.UnixMilli(now.Add(time.Duration(request.ExpiresInSeconds) * time.Second).UnixMilli()).UTC()
	o.Base, o.Actor, o.Status = a.revision, actor, overrideActive
	return o, "", "", 0
}

// ValidateOverride answers whether a proposed override would be created now,
// without creating it. It runs every check creation runs, including compiling
// the effective policy, and records the answer in the audit log only.
func (c *Controller) ValidateOverride(ctx context.Context, actor string, expected int64, request OverrideRequest) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome("")
	}
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome("")
	}
	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure("", err)
	}
	o, code, field, holder := proposeOverride(a, expected, request, actor, now)
	if code == "" {
		if _, err := authorityGeneration(a, append(slices.Clone(a.overrides), o)); err != nil {
			code, field = policyRejection(err)
		}
	}
	if code != "" {
		c.audit.Record(Event{Kind: "tpo.rejected", Actor: actor, Reason: code, Override: holder})
		return Outcome{Outcome: "rejected", Reason: code, Field: field, Override: holder}, rejectionStatus(code)
	}
	c.audit.Record(Event{Kind: "tpo.validated", Actor: actor, Target: o.Target, Revision: a.revision, Expires: o.ExpiresAt})
	return Outcome{Outcome: "valid", Revision: a.revision, Target: o.Target, Expires: o.ExpiresAt}, http.StatusOK
}

// CreateOverride creates and publishes a temporary override of the confirmed
// base. Nothing is committed unless every check passes and the complete
// effective policy with the override compiles.
func (c *Controller) CreateOverride(ctx context.Context, t transitionRecord, request OverrideRequest) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := deadlineNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// Overrides that have expired by now no longer hold their targets.
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	o, code, field, holder := proposeOverride(a, t.Expected.Int64, request, t.Actor, now)
	if code != "" {
		t.Override = holder
		return c.reject(ctx, t, code, field, now)
	}
	o.ID, err = c.store.nextOverrideID(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	o.Creation = t.ID
	t.Override = o.ID
	return c.publishOverrides(ctx, t, a, o, overrideActive, now)
}

// RevokeOverride ends an active override on an administrator's request. It
// is allowed while a provisional base is pending. An override that has
// already ended, including one whose expiry time has passed, keeps its
// recorded disposition.
func (c *Controller) RevokeOverride(ctx context.Context, t transitionRecord, id int64) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// An override whose expiry time has passed expires first, so a late
	// revocation cannot replace its expiry.
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	if a.mode != modeNormal {
		return c.reject(ctx, t, "INVALID_STATE", "", now)
	}
	o, found, err := c.store.override(ctx, id)
	switch {
	case err != nil:
		return c.storeFailure(t.ID, err)
	case !found:
		return c.reject(ctx, t, "NOT_FOUND", "", now)
	}
	t.Override = id
	if o.Status != overrideActive {
		result, status := c.reject(ctx, t, "ALREADY_RESOLVED", "", now)
		result.Disposition, result.Target = o.Status, o.Target
		return result, status
	}
	return c.publishOverrides(ctx, t, a, o, overrideRevoked, now)
}

// publishOverrides applies a change to the active overrides: o
// becomes active (status active) or ends with status. It compiles the active
// base with the overrides that follow before anything is committed, records
// the audit intent, commits, swaps the generation in, and records the
// publication receipt and the durable outcome. The base revision is never
// changed. A creation that cannot complete changes nothing; a revocation or
// expiry that cannot complete after its checks passed stops protected
// admission rather than keep serving an override that is being withdrawn.
func (c *Controller) publishOverrides(ctx context.Context, t transitionRecord, a authority, o override, status string, now time.Time) (Outcome, int) {
	stage := strings.ReplaceAll(t.Kind, ".", "-")
	failed := func(reason string, result Outcome, code int) (Outcome, int) {
		if status != overrideActive {
			c.unavailable(fmt.Sprintf("%s (%s of override %d) not completed: %s", t.ID, stage, o.ID, reason))
		}
		return result, code
	}

	next := slices.DeleteFunc(slices.Clone(a.overrides), func(active override) bool { return active.ID == o.ID })
	if status == overrideActive {
		next = append(next, o)
	}
	g, err := authorityGeneration(a, next)
	if err != nil {
		if status == overrideActive {
			// The override was never created, so the refusal names none.
			t.Override = 0
			code, field := policyRejection(err)
			return c.reject(ctx, t, code, field, now)
		}
		c.logger.Error("compiling the policy without an override", "override", o.ID, "error", err)
		result, code := c.reject(ctx, t, "TARGET_INVALID", "", now)
		return failed("effective policy invalid", result, code)
	}

	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor, Override: o.ID}); err != nil {
		result, code := auditUnavailable(t.ID)
		return failed("audit unavailable", result, code)
	}
	crashPoint(stage + "-before-commit")
	if status == overrideActive {
		err = c.store.commitOverride(ctx, t, o, now)
	} else {
		err = c.store.commitOverrideEnd(ctx, t, o.ID, status, now)
	}
	if err != nil {
		result, code := c.storeFailure(t.ID, err)
		return failed("commit failed", result, code)
	}
	crashPoint(stage + "-after-commit")

	c.current.Store(g)
	c.state = modeNormal
	c.scheduleDeadline(g)
	crashPoint(stage + "-after-swap")
	if err := c.store.markPublished(ctx, t.ID, "publisher", now); err != nil {
		c.logger.Error("recording publication receipt", "error", err)
	}
	crashPoint(stage + "-after-receipt")

	result := Outcome{
		Transition: t.ID,
		Outcome:    "committed",
		Revision:   a.revision,
		Generation: g.ID,
		Override:   o.ID,
		Target:     o.Target,
		Expires:    o.ExpiresAt,
	}
	if status != overrideActive {
		result.Disposition = status
	}
	event := Event{
		Kind:       outcomeKind(t),
		Transition: t.ID,
		Actor:      t.Actor,
		Revision:   a.revision,
		Generation: g.ID,
		Override:   o.ID,
		Target:     o.Target,
		Expires:    o.ExpiresAt,
	}
	if a.provisional != 0 {
		event.Provisional, event.RollbackTarget, event.Deadline = a.provisional, a.confirmed, a.deadline.UTC()
	}
	return c.finish(ctx, result, event)
}
