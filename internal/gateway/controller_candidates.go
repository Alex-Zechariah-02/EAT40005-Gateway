// Candidate staging and withdrawal.

package gateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/fault"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// StageCandidate validates a complete policy with the canonical compiler and
// stores it as an immutable candidate. Nothing is published: the effective
// policy and its generation are unchanged.
func (c *Controller) StageCandidate(ctx context.Context, t transitionRecord, document []byte) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := compile.TrustedNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}

	a, err := c.store.load(ctx)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	var parent int64
	if a.mode == modeNormal {
		parent = a.confirmed
	}
	if t.Expected.Int64 != parent {
		return c.reject(ctx, t, "PRECONDITION_FAILED", "", now)
	}
	policy, err := policy.DecodePolicy(document)
	if err == nil {
		_, err = compile.Compile(policy, 0, time.Time{})
	}
	if err != nil {
		code, field := policyRejection(err)
		return c.reject(ctx, t, code, field, now)
	}

	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		return auditUnavailable(t.ID)
	}
	canonical, _ := json.Marshal(policy)
	record := candidateRecord{Parent: parent, Document: canonical, Checksum: documentChecksum(canonical), DenyAll: compile.DenyAll(policy), Actor: t.Actor}
	id, err := c.store.commitCandidate(ctx, t, record, now)
	if err != nil {
		return c.storeFailure(t.ID, err)
	}
	fault.CrashPoint("candidate-after-commit")
	result := Outcome{Transition: t.ID, Outcome: "committed", Candidate: id}
	return c.finish(ctx, result, Event{Kind: "transition.committed", Transition: t.ID, Actor: t.Actor})
}

// WithdrawCandidate marks a validated candidate withdrawn.
func (c *Controller) WithdrawCandidate(ctx context.Context, t transitionRecord, id int64) (Outcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := compile.TrustedNow()
	if !c.ready() {
		return unavailableOutcome(t.ID)
	}
	if result, status, done := c.replay(ctx, t); done {
		return result, status
	}

	record, found, err := c.store.candidate(ctx, id)
	switch {
	case err != nil:
		return c.storeFailure(t.ID, err)
	case !found:
		return c.reject(ctx, t, "NOT_FOUND", "", now)
	case record.Status != "validated":
		return c.reject(ctx, t, "CANDIDATE_UNAVAILABLE", "", now)
	}
	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		return auditUnavailable(t.ID)
	}
	if err := c.store.commitWithdrawal(ctx, t, id, now); err != nil {
		return c.storeFailure(t.ID, err)
	}
	result := Outcome{Transition: t.ID, Outcome: "committed", Candidate: id}
	return c.finish(ctx, result, Event{Kind: "transition.committed", Transition: t.ID, Actor: t.Actor})
}
