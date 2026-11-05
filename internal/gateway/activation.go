// Recovery base, confirmed activation and resolution.

package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// EstablishBase confirms a validated deny-all candidate as the first
// confirmed base of an installation (genesis) or as the base that ends a
// declared recovery. Each has one server-chosen identity, so a retried or
// duplicated request can never create a second base.
func (c *Controller) EstablishBase(ctx context.Context, kind, actor string, candidate int64, checksum string) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome(kind)
	}
	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(kind, err)
	}

	t := transitionRecord{ID: "genesis:" + a.installation, Kind: "genesis", Actor: actor, Target: fmt.Sprintf("candidate %d", candidate)}
	requiredMode := modeBootstrap
	if kind == "recovery_base" {
		t.ID, t.Kind = "recovery:"+a.episode, "recovery_base"
		requiredMode = modeRecovery
	}
	t.Digest = requestDigest(t.Kind, t.Target, sql.NullInt64{}, checksum)
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// No override may be active when a base is established: none exists
	// before genesis, and a declared restore quarantines every one.
	if a.mode != requiredMode || (kind == "recovery_base" && a.episode == "") || len(a.overrides) > 0 {
		return c.reject(ctx, t, "INVALID_STATE", "", now)
	}

	record, found, err := c.store.candidate(ctx, candidate)
	switch {
	case err != nil:
		return c.storeFailure(t.ID, err)
	case !found:
		return c.reject(ctx, t, "NOT_FOUND", "", now)
	case record.Status != "validated":
		return c.reject(ctx, t, "CANDIDATE_UNAVAILABLE", "", now)
	case record.Checksum != checksum || documentChecksum(record.Document) != checksum:
		return c.reject(ctx, t, "PRECONDITION_FAILED", "checksum", now)
	}
	policy, err := policy.DecodePolicy(record.Document)
	if err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}
	if !denyAll(policy) {
		return c.reject(ctx, t, "NOT_DENY_ALL", "", now)
	}
	change := revisionChange{origin: t.Kind, actor: actor, mode: modeNormal, candidate: candidate}
	return c.publish(ctx, t, policy, change, nil, now)
}

// Activate makes a validated candidate the provisional active base. The
// candidate must have been prepared against the current confirmed revision,
// which becomes its rollback target, and its checksum must match the one the
// administrator confirmed. It is decoded and compiled again in full before
// anything is committed. Its deadline is persisted with the activation, so a
// restart can neither extend nor forget it, and it is never extended.
func (c *Controller) Activate(ctx context.Context, t transitionRecord, candidate int64, checksum string, seconds int64) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := deadlineNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// Overrides that have expired by now must not block the candidate.
	if c.state == modeNormal && !c.settle(ctx, now, false) {
		return unavailableOutcome(t.ID)
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	t.Candidate = candidate
	switch {
	case a.mode != modeNormal:
		return c.reject(ctx, t, "INVALID_STATE", "", now)
	case a.provisional != 0:
		return c.reject(ctx, t, "PROVISIONAL_ACTIVE", "", now)
	case a.confirmed != t.Expected.Int64:
		return c.reject(ctx, t, "PRECONDITION_FAILED", "", now)
	case seconds < minConfirmSeconds || seconds > maxConfirmSeconds:
		return c.reject(ctx, t, "INVALID_DEADLINE", "confirm_within_seconds", now)
	}

	record, found, err := c.store.candidate(ctx, candidate)
	switch {
	case err != nil:
		return c.storeFailure(t.ID, err)
	case !found:
		return c.reject(ctx, t, "NOT_FOUND", "", now)
	case record.Status != "validated":
		return c.reject(ctx, t, "CANDIDATE_UNAVAILABLE", "", now)
	case record.Parent != a.confirmed:
		return c.reject(ctx, t, "CANDIDATE_STALE", "", now)
	case record.Checksum != checksum:
		return c.reject(ctx, t, "PRECONDITION_FAILED", "checksum", now)
	case documentChecksum(record.Document) != record.Checksum:
		return c.reject(ctx, t, "CANDIDATE_INTEGRITY", "", now)
	}
	policy, err := policy.DecodePolicy(record.Document)
	if err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}
	// A candidate may not change or invalidate an active override; one that
	// leaves every override's targets alone becomes provisional with them.
	if result, status, conflict := c.rejectConflict(ctx, t, a, policy, now); conflict {
		return result, status
	}

	// The database keeps milliseconds, so the running deadline and the one a
	// restart reads are the same instant.
	deadline := time.UnixMilli(now.Add(time.Duration(seconds) * time.Second).UnixMilli())
	change := revisionChange{parent: a.confirmed, origin: "publish", actor: t.Actor, deadline: deadline, mode: modeNormal, candidate: candidate}
	return c.publish(ctx, t, policy, change, a.overrides, now)
}

// Resolve confirms or rolls back the current provisional base on an
// administrator's explicit request. The request names the provisional
// revision it means and the confirmed revision it was prepared against; a
// request for any other state is refused. At or after the deadline neither is
// possible: the request is refused and the automatic rollback runs instead,
// however late its timer is.
func (c *Controller) Resolve(ctx context.Context, t transitionRecord, provisional int64, disposition string) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := trustedNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}
	// The base that follows is compiled with the overrides still active now.
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
	if a.provisional != provisional {
		earlier, found, err := c.store.activation(ctx, "revision", provisional)
		switch {
		case err != nil:
			return c.storeFailure(t.ID, err)
		case found:
			result, status := c.reject(ctx, t, "ALREADY_RESOLVED", "", now)
			result.Disposition = earlier.Disposition
			return result, status
		case a.provisional == 0:
			return c.reject(ctx, t, "NO_PROVISIONAL", "", now)
		default:
			return c.reject(ctx, t, "PROVISIONAL_MISMATCH", "", now)
		}
	}
	t.Candidate = a.candidate
	if a.confirmed != t.Expected.Int64 {
		return c.reject(ctx, t, "PRECONDITION_FAILED", "", now)
	}
	if !now.Before(a.deadline) {
		result, status := c.reject(ctx, t, "DEADLINE_PASSED", "", now)
		c.timeout(ctx, a, now)
		return result, status
	}
	return c.endProvisional(ctx, t, a, disposition, now)
}
