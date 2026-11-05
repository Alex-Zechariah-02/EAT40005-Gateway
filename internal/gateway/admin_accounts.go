package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/fault"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

func controlError(code string) json.RawMessage {
	body, _ := json.Marshal(map[string]string{"error": code})
	return body
}

// replayControl uses the existing actor-scoped transition identity and digest;
// its successful body is the original response, even after later mutations.
// The caller holds c.mu.
func (c *Controller) replayControl(ctx context.Context, t store.TransitionRecord) (json.RawMessage, int, bool) {
	if !c.ready() {
		return controlError("gateway_unavailable"), http.StatusServiceUnavailable, true
	}
	outcome, status, done := c.replay(ctx, t)
	if !done {
		return nil, 0, false
	}
	if status != http.StatusOK {
		return controlError(strings.ToLower(outcome.Reason)), status, true
	}
	body, err := c.store.ControlResult(ctx, t.ID)
	if err != nil {
		c.logger.Error("reading control receipt", "error", err)
		return controlError("gateway_unavailable"), http.StatusServiceUnavailable, true
	}
	return body, status, true
}

func (c *Controller) rejectControl(ctx context.Context, t store.TransitionRecord, reason string, now time.Time) (json.RawMessage, int) {
	outcome, status := c.reject(ctx, t, reason, "", now)
	return controlError(strings.ToLower(outcome.Reason)), status
}

// finishControl follows the same durable intent, commit, publication and audit
// sequence as policy mutations. A lost acknowledgement is retried by identity;
// it never re-applies a block or re-reads keys as a hidden retry.
func (c *Controller) finishControl(ctx context.Context, t store.TransitionRecord, result any, change func(*sql.Tx) error, publish func(), now time.Time) (json.RawMessage, int) {
	body, err := json.Marshal(result)
	if err != nil {
		return controlError("gateway_unavailable"), http.StatusServiceUnavailable
	}
	if err := c.commitAudit(ctx, Event{Kind: "transition.intent", Transition: t.ID, Actor: t.Actor}); err != nil {
		return controlError("gateway_unavailable"), http.StatusServiceUnavailable
	}
	fault.CrashPoint("before-commit")
	if err := c.store.CommitControl(ctx, t, body, change, now); err != nil {
		c.logger.Error("committing control change", "error", err)
		return controlError("gateway_unavailable"), http.StatusServiceUnavailable
	}
	fault.CrashPoint("after-commit")
	publish()
	fault.CrashPoint("after-swap")
	if err := c.store.MarkPublished(ctx, t.ID, "publisher", now); err != nil {
		c.logger.Error("recording control publication receipt", "error", err)
	}
	fault.CrashPoint("after-receipt")
	outcome, status := c.finish(ctx, Outcome{Transition: t.ID, Outcome: "committed"}, Event{Kind: t.Kind, Transition: t.ID, Actor: t.Actor, Target: t.Target})
	if status != http.StatusOK {
		// The change is committed and in force; only its outcome event is
		// missing, which "gateway_unavailable" would deny.
		return controlError(strings.ToLower(outcome.Reason)), status
	}
	return body, status
}

// handleKeys lists the trusted keys with how many tokens each verified since
// the gateway started and when it last did; counts only, no key material.
func (c *Controller) handleKeys(w http.ResponseWriter, r *http.Request, who caller) {
	type keyReport struct {
		keyInfo
		Verified       uint64     `json:"verified"`
		LastVerifiedAt *time.Time `json:"last_verified_at"`
	}
	v := c.verifier.Load()
	items := make([]keyReport, 0, len(v.info))
	for _, info := range v.info {
		report := keyReport{keyInfo: info}
		if use := v.uses[info.Kid]; use != nil {
			report.Verified = use.verified.Load()
			if last := use.lastNano.Load(); last != 0 {
				at := time.Unix(0, last).UTC()
				report.LastVerifiedAt = &at
			}
		}
		items = append(items, report)
	}
	writeJSON(w, http.StatusOK, list{Items: items})
}

func noRequestBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return false
	}
	return true
}

func (c *Controller) handleReloadKeys(w http.ResponseWriter, r *http.Request, who caller) {
	if !noRequestBody(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	t := adminTransition(who, key, "keys.reload", "keys", sql.NullInt64{}, nil)
	body, status := c.reloadKeys(r.Context(), t)
	writeJSON(w, status, body)
}

func (c *Controller) reloadKeys(ctx context.Context, t store.TransitionRecord) (json.RawMessage, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if body, status, done := c.replayControl(ctx, t); done {
		return body, status
	}
	now := compile.TrustedNow()
	cfg, err := loadNativeConfig(c.config.settingsFile)
	if err != nil {
		return c.rejectControl(ctx, t, "KEYS_INVALID", now)
	}
	current := c.verifier.Load()
	if cfg.controller.JWT.Issuer != current.issuer || cfg.controller.JWT.Audience != current.audience {
		return c.rejectControl(ctx, t, "KEYS_INVALID", now)
	}
	loaded, err := loadVerifier(cfg.controller.JWT)
	if err != nil {
		return c.rejectControl(ctx, t, "KEYS_INVALID", now)
	}
	return c.finishControl(ctx, t, list{Items: loaded.info}, nil, func() {
		loaded.adoptUses(c.verifier.Load())
		c.verifier.Store(loaded)
	}, now)
}

func (c *Controller) handleBlockedAccounts(w http.ResponseWriter, r *http.Request, who caller) {
	current := c.blocked.Load()
	if current == nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	items := slices.Collect(maps.Values(*current))
	if items == nil {
		items = []store.BlockedAccount{}
	}
	slices.SortFunc(items, func(a, b store.BlockedAccount) int { return strings.Compare(a.Subject, b.Subject) })
	writeJSON(w, http.StatusOK, list{Items: items})
}

func (c *Controller) handleBlockAccount(w http.ResponseWriter, r *http.Request, who caller) {
	subject := r.PathValue("subject")
	if !policy.IsSubjectID(subject) {
		adminError(w, http.StatusBadRequest, "bad_request", "subject")
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var request struct {
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, maxAdminBody, &request) {
		return
	}
	if !store.ValidBlockReason(request.Reason) {
		adminError(w, http.StatusBadRequest, "bad_request", "reason")
		return
	}
	t := adminTransition(who, key, "account.block", subject, sql.NullInt64{}, request)
	body, status := c.changeBlock(r.Context(), t, request.Reason)
	writeJSON(w, status, body)
}

func (c *Controller) handleUnblockAccount(w http.ResponseWriter, r *http.Request, who caller) {
	subject := r.PathValue("subject")
	if !policy.IsSubjectID(subject) {
		adminError(w, http.StatusBadRequest, "bad_request", "subject")
		return
	}
	if !noRequestBody(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	t := adminTransition(who, key, "account.unblock", subject, sql.NullInt64{}, nil)
	body, status := c.changeBlock(r.Context(), t, "")
	writeJSON(w, status, body)
}

func (c *Controller) changeBlock(ctx context.Context, t store.TransitionRecord, reason string) (json.RawMessage, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if body, status, done := c.replayControl(ctx, t); done {
		return body, status
	}
	now := compile.TrustedNow().UTC().Truncate(time.Millisecond)
	current := c.blocked.Load()
	if current == nil {
		return controlError("gateway_unavailable"), http.StatusServiceUnavailable
	}
	previous, found := (*current)[t.Target]
	next := maps.Clone(*current)
	var result any
	var change func(*sql.Tx) error
	if t.Kind == "account.block" {
		entry := store.BlockedAccount{Subject: t.Target, Reason: reason, BlockedBy: t.Actor, BlockedAt: now}
		if found && previous.Reason == reason {
			entry = previous
		}
		next[t.Target] = entry
		result = entry
		change = func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO blocked_account (subject, reason, blocked_by, blocked_at) VALUES (?, ?, ?, ?)
				ON CONFLICT(subject) DO UPDATE SET reason=excluded.reason, blocked_by=excluded.blocked_by, blocked_at=excluded.blocked_at`,
				entry.Subject, entry.Reason, entry.BlockedBy, entry.BlockedAt.UnixMilli())
			return err
		}
	} else {
		if !found {
			return c.rejectControl(ctx, t, "NOT_FOUND", now)
		}
		delete(next, t.Target)
		result = map[string]any{"subject": t.Target, "unblocked": true}
		change = func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM blocked_account WHERE subject = ?`, t.Target)
			return err
		}
	}
	return c.finishControl(ctx, t, result, change, func() { c.blocked.Store(&next) }, now)
}

func (c *Controller) handleAdministrators(w http.ResponseWriter, r *http.Request, who caller) {
	entries, err := c.store.Administrators(r.Context())
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	type view struct {
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
		Status      string   `json:"status"`
	}
	items := []view{}
	for _, entry := range entries {
		items = append(items, view{Name: entry.Name, Permissions: entry.Permissions.Names(), Status: entry.Status})
	}
	writeJSON(w, http.StatusOK, list{Items: items})
}
