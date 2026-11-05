// Provisional deadlines, expiry and settlement.

package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/fault"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// scheduleDeadline arms the timer that wakes the controller at the earliest
// deadline the generation depends on. The timer only wakes the controller:
// every deadline is persisted and is also enforced on every request by the
// generation itself.
func (c *Controller) scheduleDeadline(g *compile.Generation) {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if g.NotAfter.IsZero() || fault.TimerFault() {
		return
	}
	c.timer = time.AfterFunc(g.NotAfter.Sub(compile.TrustedNow()), func() { c.expire(g) })
}

// deadlinePassed is called by the request path when the generation it
// captured has expired. It starts at most one check at a time, so a late or
// missing timer cannot keep the gateway refusing every request.
func (c *Controller) deadlinePassed(g *compile.Generation) {
	if !c.expiring.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.expiring.Store(false)
		c.expire(g)
	}()
}

// expire ends whatever made the published generation g expire. Under the
// controller lock it rechecks that g is still the published generation, so a
// timer or wake-up that arrives after another transition does nothing. A timer
// that fires before g has expired (the wall clock was set back after it was
// armed) is armed again.
func (c *Controller) expire(g *compile.Generation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := compile.TrustedNow()
	// After beginStop no generation is published, so this is never g.
	if c.current.Load() != g {
		return
	}
	if !g.Expired(now) {
		c.scheduleDeadline(g)
		return
	}
	if !c.settle(context.Background(), now, true) {
		return
	}
	// The generation expired, so a stored deadline must have passed and been
	// ended above. If none had, runtime and stored state disagree.
	if c.current.Load() == g {
		c.unavailable(fmt.Sprintf("generation %s expired but no stored deadline has passed", g.ID))
	}
}

// settle ends, one transition at a time in deadline order, every override
// whose expiry time has passed at now and, when withProvisional is set, a
// provisional base whose deadline has passed. Each ending publishes the
// effective policy that follows it. Mutation requests settle overrides only,
// so a request about a provisional base past its deadline is still answered
// as such. settle reports false when an ending could not be completed;
// protected admission has then stopped.
func (c *Controller) settle(ctx context.Context, now time.Time, withProvisional bool) bool {
	for {
		a, err := c.store.Load(ctx)
		if err != nil {
			c.unavailable(err.Error())
			return false
		}
		var due *store.Override
		for i, o := range a.Overrides {
			if !now.Before(o.ExpiresAt) && (due == nil || o.ExpiresAt.Before(due.ExpiresAt)) {
				due = &a.Overrides[i]
			}
		}
		provisionalDue := withProvisional && a.Provisional != 0 && !now.Before(a.Deadline)
		switch {
		case provisionalDue && (due == nil || !due.ExpiresAt.Before(a.Deadline)):
			if !c.timeout(ctx, a, now) {
				return false
			}
		case due != nil:
			t := store.TransitionRecord{
				ID:       fmt.Sprintf("auto-expire:%d", due.ID),
				Kind:     "tpo.expire",
				Actor:    "controller",
				Target:   fmt.Sprintf("override %d", due.ID),
				Override: due.ID,
			}
			t.Digest = t.ID
			if result, _ := c.publishOverrides(ctx, t, a, *due, store.OverrideExpired, now); result.Outcome != "committed" {
				return false
			}
		default:
			return true
		}
	}
}

// timeout rolls back an expired provisional base under its deterministic
// identity, so a repeated attempt can never apply twice. It reports whether
// the restored base is now published; when it is not, nothing is served.
func (c *Controller) timeout(ctx context.Context, a store.Authority, now time.Time) bool {
	t := store.TransitionRecord{
		ID:        fmt.Sprintf("auto-rollback:%d", a.Provisional),
		Kind:      "cpa.timeout",
		Actor:     "controller",
		Target:    fmt.Sprintf("revision %d", a.Provisional),
		Expected:  sql.NullInt64{Int64: a.Confirmed, Valid: true},
		Candidate: a.Candidate,
	}
	t.Digest = t.ID
	result, _ := c.endProvisional(ctx, t, a, store.DispositionTimedOut, now)
	return result.Outcome == "committed"
}

// endProvisional records the final disposition of the provisional base in a
// and publishes the base that follows it, through the same steps as any
// publication: compile the target before anything is committed, record the
// audit intent, commit, swap, record the receipt and the durable outcome.
// Confirmation republishes the provisional revision without its deadline.
// Every other disposition restores the rollback target; if that cannot be
// completed, the controller stops admitting protected requests rather than
// keep serving a provisional policy that is being withdrawn.
func (c *Controller) endProvisional(ctx context.Context, t store.TransitionRecord, a store.Authority, disposition string, now time.Time) (Outcome, int) {
	target := a.Confirmed
	if disposition == store.DispositionConfirmed {
		target = a.Provisional
	}
	stage := strings.TrimPrefix(t.Kind, "cpa.")
	failed := func(reason string, result Outcome, status int) (Outcome, int) {
		if disposition != store.DispositionConfirmed {
			c.unavailable(fmt.Sprintf("%s (%s of revision %d) not completed: %s", t.ID, stage, a.Provisional, reason))
		}
		return result, status
	}

	// The base that follows is compiled with the overrides active now, never
	// with an earlier effective policy: an override that ended while the
	// base was provisional stays ended.
	g, err := c.compileEffective(ctx, target, time.Time{}, a.Overrides)
	if err == nil && disposition != store.DispositionConfirmed {
		err = fault.RollbackFault()
	}
	if err != nil {
		c.logger.Error("compiling the base after the provisional revision", "revision", target, "error", err)
		result, status := c.reject(ctx, t, "TARGET_INVALID", "", now)
		return failed("target revision invalid", result, status)
	}

	if err := c.commitAudit(ctx, audit.Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		result, status := auditUnavailable(t.ID)
		return failed("audit unavailable", result, status)
	}
	fault.CrashPoint(stage + "-before-commit")
	t.Revision = target
	if err := c.store.CommitResolution(ctx, t, a, disposition, now); err != nil {
		result, status := c.storeFailure(t.ID, err)
		return failed("commit failed", result, status)
	}
	fault.CrashPoint(stage + "-after-commit")

	c.current.Store(g)
	c.state = store.ModeNormal
	c.scheduleDeadline(g)
	fault.CrashPoint(stage + "-after-swap")
	if err := c.store.MarkPublished(ctx, t.ID, "publisher", now); err != nil {
		c.logger.Error("recording publication receipt", "error", err)
	}
	fault.CrashPoint(stage + "-after-receipt")

	result := Outcome{
		Transition:     t.ID,
		Outcome:        "committed",
		Revision:       target,
		Generation:     g.ID,
		Candidate:      a.Candidate,
		Provisional:    a.Provisional,
		RollbackTarget: a.Confirmed,
		Disposition:    disposition,
	}
	event := audit.Event{
		Kind:           outcomeKind(t),
		Transition:     t.ID,
		Actor:          t.Actor,
		Revision:       target,
		Generation:     g.ID,
		Candidate:      a.Candidate,
		Provisional:    a.Provisional,
		RollbackTarget: a.Confirmed,
		Deadline:       a.Deadline.UTC(),
	}
	return c.finish(ctx, result, event)
}
