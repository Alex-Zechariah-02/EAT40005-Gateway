// Startup reconciliation of stored state, audit and unfinished outcomes.

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// reconcile rebuilds the runtime generation from committed authority. It never
// falls back to an older revision: anything it cannot establish leaves the
// gateway serving no protected request.
func (c *Controller) reconcile(now time.Time) {
	ctx := context.Background()
	a, err := c.store.load(ctx)
	if err != nil {
		c.unavailable(err.Error())
		return
	}
	if err := c.reconcileAudit(ctx, now); err != nil {
		c.unavailable(fmt.Sprintf("recording missing outcome events: %v", err))
		return
	}
	if a.mode != modeNormal {
		c.unavailable(a.mode)
		return
	}
	if now.Before(a.highWater.Add(-clockTolerance)) {
		c.unavailable("clock_regression")
		return
	}
	// A provisional base or an override whose time ran out while the gateway
	// was stopped is ended before anything is served; it is never published
	// again.
	if !c.settle(ctx, now, true) {
		return
	}
	a, err = c.store.load(ctx)
	if err != nil {
		c.unavailable(err.Error())
		return
	}
	// A provisional base is served only while the base it returns to can
	// still be restored with the active overrides.
	if a.provisional != 0 {
		if _, err := c.compileEffective(ctx, a.confirmed, time.Time{}, a.overrides); err != nil {
			c.unavailable(fmt.Sprintf("rollback target revision %d: %v", a.confirmed, err))
			return
		}
	}
	// If nothing ended above, nothing is published yet.
	if c.current.Load() == nil && !c.publishAuthority(a) {
		return
	}
	ids, err := c.store.unpublished(ctx)
	if err != nil {
		c.unavailable(fmt.Sprintf("reading publication receipts: %v", err))
		return
	}
	for _, id := range ids {
		if err := c.store.markPublished(ctx, id, "reconciliation", now); err != nil {
			c.unavailable(fmt.Sprintf("recording publication receipt: %v", err))
			return
		}
	}
	if err := c.store.raiseHighWater(ctx, now); err != nil {
		c.unavailable(fmt.Sprintf("recording clock high-water mark: %v", err))
		return
	}
	g := c.current.Load()
	c.audit.Record(Event{Kind: "controller.reconciled", Generation: g.ID, Revision: g.Revision})
}

// reconcileAudit durably records the outcome event of every committed
// transition whose outcome was never acknowledged by the audit log, for
// example because the process stopped between the commit and the outcome
// event. The database holds the authoritative disposition; this restores the
// missing security record from it.
func (c *Controller) reconcileAudit(ctx context.Context, now time.Time) error {
	ids, err := c.store.unaudited(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		t, _, err := c.store.transition(ctx, id)
		if err != nil {
			return err
		}
		event := Event{Kind: outcomeKind(t), Transition: t.ID, Actor: t.Actor, Target: t.Target, Revision: t.Revision, Candidate: t.Candidate, Reason: "reconciled"}
		if strings.HasPrefix(t.Kind, "tpo.") {
			o, found, err := c.store.overrideOf(ctx, t.ID)
			if err != nil || !found {
				return fmt.Errorf("override of transition %s: %v", t.ID, err)
			}
			event.Override, event.Target, event.Expires = o.ID, o.Target, o.ExpiresAt
		}
		if err := c.commitAudit(ctx, event); err != nil {
			return err
		}
		if err := c.store.markAudited(ctx, id, now); err != nil {
			return err
		}
	}
	return nil
}

// replay returns the recorded outcome when the mutation identity was already
// processed. The same identity with a different request is refused, so a
// retried request can never apply twice and a reused identity can never
// change what was recorded.
func (c *Controller) replay(ctx context.Context, t transitionRecord) (Outcome, int, bool) {
	previous, found, err := c.store.transition(ctx, t.ID)
	if err != nil {
		result, status := c.storeFailure(t.ID, err)
		return result, status, true
	}
	if !found {
		return Outcome{}, 0, false
	}
	result := Outcome{Transition: t.ID, Outcome: "rejected", Reason: "TRANSITION_ID_REUSED"}
	if previous.Digest != t.Digest {
		return result, http.StatusUnprocessableEntity, true
	}
	result.Outcome, result.Reason = previous.Outcome, previous.Reason
	result.Revision, result.Candidate = previous.Revision, previous.Candidate
	if previous.Outcome == "rejected" {
		return result, rejectionStatus(previous.Reason), true
	}
	if strings.HasPrefix(previous.Kind, "cpa.") {
		// The activation record holds the rest of a lifecycle receipt.
		a, found, err := c.store.activation(ctx, "candidate", previous.Candidate)
		if err != nil || !found {
			result, status := c.storeFailure(t.ID, fmt.Errorf("activation of candidate %d: %v", previous.Candidate, err))
			return result, status, true
		}
		result.Provisional, result.RollbackTarget, result.Deadline = a.Revision, a.RollbackTarget, a.Deadline
		if a.Resolution == previous.ID {
			result.Disposition = a.Disposition
		}
	}
	if strings.HasPrefix(previous.Kind, "tpo.") {
		// The override record holds the rest of an override receipt.
		o, found, err := c.store.overrideOf(ctx, previous.ID)
		if err != nil || !found {
			result, status := c.storeFailure(t.ID, fmt.Errorf("override of transition %s: %v", previous.ID, err))
			return result, status, true
		}
		result.Override, result.Target, result.Expires = o.ID, o.Target, o.ExpiresAt
		if o.Resolution == previous.ID {
			result.Disposition = o.Status
		}
	}
	if !previous.Audited {
		result.Reason = "OUTCOME_AUDIT_PENDING"
		return result, http.StatusServiceUnavailable, true
	}
	return result, http.StatusOK, true
}
