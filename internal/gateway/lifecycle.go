// Controller start, stop and admission state.

package gateway

import (
	"context"
	"errors"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Start serves the Admin API on its loopback listener. Protected admission
// does not wait for it: it follows the generation published since
// NewController. Start succeeds at most once, and never once Stop has begun.
func (c *Controller) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.stopping:
		return errors.New("controller stopped")
	case c.admin != nil:
		return errors.New("controller already started")
	}
	return c.startAdmin()
}

// Stop begins the stop (see beginStop), shuts the Admin API down, waiting at
// most adminShutdownWait for admin requests in progress, then closes the
// audit log, waiting at most auditDrainWait for the events queued until then
// to be written, and the database. It returns nil only when every step
// completed. It also releases a controller that was never started. Stop may
// be called any number of times, also concurrently: the controller stops
// once, and every call returns that stop's result.
func (c *Controller) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), adminShutdownWait)
	defer cancel()
	return c.stopWithin(ctx)
}

// stopWithin is Stop with ctx bounding the wait for admin requests in
// progress; the native gateway shares that bound with its data plane.
func (c *Controller) stopWithin(ctx context.Context) error {
	c.stopOnce.Do(func() { c.stopErr = c.shutdown(ctx) })
	return c.stopErr
}

// beginStop ends protected admission and every change of authority: the
// published generation is withdrawn, so a request that has not captured one
// yet is refused; mutations and the deadline timer change nothing any more;
// Start is refused. A request that captured a generation earlier runs to its
// end with it. Stop begins with it; the native gateway calls it before it
// stops its data plane.
func (c *Controller) beginStop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping {
		return
	}
	c.stopping = true
	c.observations.close()
	c.state = stateStopping
	c.current.Store(nil)
	if c.timer != nil {
		c.timer.Stop()
	}
}

func (c *Controller) shutdown(ctx context.Context) error {
	c.beginStop()
	c.mu.Lock()
	admin := c.admin
	c.mu.Unlock()
	// Admin requests in progress may need the lock to finish, so the listener
	// is shut down without holding it.
	err := stopAdmin(ctx, admin)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = stateStopped
	// Every producer of audit events has stopped by now: the admission and
	// admin requests of the stopped listeners, mutations (the lock is held)
	// and the deadline timer (no generation is published). What they queued is
	// written before the database closes; an event arriving later anyway, from
	// a request cut off by an incomplete listener shutdown, is counted as lost.
	pending, _ := c.audit.Unconfirmed()
	c.logger.Info("draining the audit log", "pending", pending)
	drain, cancel := context.WithTimeout(context.Background(), audit.AuditDrainWait)
	defer cancel()
	errs := []error{err, c.audit.Close(drain)}
	if c.store != nil {
		errs = append(errs, c.store.Close())
	}
	if c.ownership != nil {
		errs = append(errs, c.ownership.Close())
	}
	return errors.Join(errs...)
}

// admission reports whether protected admission can run at this moment:
// "ready" when a published generation is unexpired and the audit log accepts
// acknowledgements, otherwise why it cannot (the stop, the deployment mode,
// "unavailable", "generation_expired" or "audit_failed"). A ready gateway may
// still refuse every request its policy denies, and the next request may
// still fail; this is an observation, not a promise.
func (c *Controller) admission() string {
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	switch state {
	case store.ModeNormal:
	case store.ModeBootstrap, store.ModeRecovery, stateStopping, stateStopped:
		return state
	default:
		return "unavailable"
	}
	g := c.current.Load()
	switch {
	case g == nil:
		return "unavailable"
	case g.Expired(compile.TrustedNow()):
		return "generation_expired"
	case c.audit.Failed():
		return "audit_failed"
	}
	return "ready"
}

func (c *Controller) unavailable(reason string) {
	c.current.Store(nil)
	c.state = reason
	c.logger.Error("protected admission unavailable", "reason", reason)
	c.audit.Record(audit.Event{Kind: "controller.unavailable", Reason: reason})
}

// ready reports whether mutations can be processed: the database is open and
// its authority was read, whatever the deployment mode.
func (c *Controller) ready() bool {
	return c.store != nil && (c.state == store.ModeNormal || c.state == store.ModeBootstrap || c.state == store.ModeRecovery)
}
