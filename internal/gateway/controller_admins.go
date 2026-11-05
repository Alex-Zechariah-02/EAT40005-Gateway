// Administrator enrolment and password changes.

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/fault"
)

// Enrol consumes an enrolment and stores the administrator's new credential.
// The password hash is computed by the caller, outside the controller lock.
// Every existing session of the administrator ends.
func (c *Controller) Enrol(ctx context.Context, e enrolment, passwordHash string) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := compile.TrustedNow()
	t := transitionRecord{ID: "enrol:" + e.Secret[:32], Kind: "admin.enrol", Actor: e.Name, Target: e.Name, Digest: e.Secret}
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: e.Name}); err != nil {
		return auditUnavailable(t.ID)
	}
	fault.CrashPoint("enrol-before-commit")
	err := c.store.commitEnrolment(ctx, t, e, passwordHash, now)
	if errors.Is(err, errNotUpdated) {
		// Another request consumed the secret first.
		c.audit.Record(Event{Kind: "transition.rejected", Transition: t.ID, Actor: e.Name, Reason: "ENROLMENT_CONSUMED"})
		return Outcome{Transition: t.ID, Outcome: "rejected", Reason: "ENROLMENT_CONSUMED"}, http.StatusUnauthorized
	}
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	c.sessions.revokeAdmin(e.Name)
	fault.CrashPoint("enrol-after-commit")
	return c.finish(ctx, Outcome{Transition: t.ID, Outcome: "committed"}, Event{Kind: "transition.committed", Transition: t.ID, Actor: e.Name})
}

// ChangePassword replaces an administrator's password, provided the
// credential is still the one the caller verified, and ends every session of
// that administrator.
func (c *Controller) ChangePassword(ctx context.Context, admin administrator, passwordHash string) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := compile.TrustedNow()
	t := transitionRecord{
		ID:     fmt.Sprintf("password:%s:%d", admin.Name, admin.Credential),
		Kind:   "admin.password",
		Actor:  admin.Name,
		Target: admin.Name,
		Digest: "password",
	}
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: admin.Name}); err != nil {
		return auditUnavailable(t.ID)
	}
	err := c.store.commitPassword(ctx, t, admin.Name, admin.Credential, passwordHash, now)
	if errors.Is(err, errNotUpdated) {
		c.audit.Record(Event{Kind: "transition.rejected", Transition: t.ID, Actor: admin.Name, Reason: "CREDENTIAL_CHANGED"})
		return Outcome{Transition: t.ID, Outcome: "rejected", Reason: "CREDENTIAL_CHANGED"}, http.StatusConflict
	}
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	c.sessions.revokeAdmin(admin.Name)
	return c.finish(ctx, Outcome{Transition: t.ID, Outcome: "committed"}, Event{Kind: "transition.committed", Transition: t.ID, Actor: admin.Name})
}
