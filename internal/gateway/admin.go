// Admin API listener, routing, host and origin checks and authentication.

package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	maxAdminBody     = 4 << 10
	maxCandidateBody = 80 << 10
	defaultPageSize  = 50
	maxPageSize      = 100
	sessionCookie    = "__Host-gateway-admin"
	csrfHeader       = "X-CSRF-Token"
	minKeyLength     = 16
	maxKeyLength     = 64
	// adminShutdownWait bounds how long stopping waits for admin requests in
	// progress; the native gateway waits that long for the requests of both
	// its listeners together.
	adminShutdownWait = 5 * time.Second
)

// caller is the authenticated administrator of one admin request.
type caller struct {
	admin   administrator
	session session
	token   string
}

type adminHandler func(w http.ResponseWriter, r *http.Request, who caller)

// startAdmin serves the project Admin API. It binds only to loopback and is
// separate from the data plane, so client traffic can never reach it. The
// caller holds c.mu.
func (c *Controller) startAdmin() error {
	listener, err := net.Listen("tcp", c.config.AdminListen)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/health", c.handleHealth)
	mux.HandleFunc("POST /admin/enrol", c.handleEnrol)
	mux.HandleFunc("POST /admin/session", c.handleLogin)

	c.route(mux, "GET /admin/session", 0, c.handleWhoAmI)
	c.route(mux, "DELETE /admin/session", 0, c.handleLogout)
	c.route(mux, "PUT /admin/password", 0, c.handlePassword)
	c.route(mux, "GET /admin/status", permRead, c.handleStatus)
	c.route(mux, "GET /admin/policy", permRead, c.handlePolicy)
	c.route(mux, "GET /admin/revisions", permRead, c.handleRevisions)
	c.route(mux, "GET /admin/revisions/{id}", permRead, c.handleRevision)
	c.route(mux, "GET /admin/keys", permRead, c.handleKeys)
	c.route(mux, "POST /admin/keys/reload", permManagePolicy, c.handleReloadKeys)
	c.route(mux, "GET /admin/blocked-accounts", permRead, c.handleBlockedAccounts)
	c.route(mux, "PUT /admin/blocked-accounts/{subject}", permManageSubjects, c.handleBlockAccount)
	c.route(mux, "DELETE /admin/blocked-accounts/{subject}", permManageSubjects, c.handleUnblockAccount)
	c.route(mux, "GET /admin/administrators", permRead, c.handleAdministrators)
	c.route(mux, "GET /admin/events", permRead, c.handleEvents)
	c.route(mux, "GET /admin/events/stream", permRead, c.handleEventStream)
	c.route(mux, "GET /admin/traffic", permRead, c.handleTraffic)
	c.route(mux, "GET /admin/limits", permRead, c.handleLimits)
	c.route(mux, "POST /admin/access-check", permRead, c.handleAccessCheck)
	c.route(mux, "GET /admin/advice", permRead, c.handleAdvice)
	c.route(mux, "GET /admin/alerts", permRead, c.handleAlerts)
	c.route(mux, "POST /admin/candidates/{id}/impact", permRead, c.handleImpact)
	c.route(mux, "GET /admin/candidates", permRead, c.handleCandidates)
	c.route(mux, "GET /admin/candidates/{id}", permRead, c.handleCandidate)
	c.route(mux, "POST /admin/candidates", permManagePolicy, c.handlePostCandidate)
	c.route(mux, "DELETE /admin/candidates/{id}", permManagePolicy, c.handleWithdrawCandidate)
	c.route(mux, "POST /admin/genesis", permManagePolicy, c.handleEstablish("genesis"))
	c.route(mux, "POST /admin/recovery-base", permManagePolicy, c.handleEstablish("recovery_base"))
	c.route(mux, "GET /admin/provisional", permRead, c.handleProvisional)
	c.route(mux, "POST /admin/provisional", permManagePolicy, c.handleActivate)
	c.route(mux, "POST /admin/provisional/confirm", permManagePolicy, c.handleResolve("cpa.confirm", dispositionConfirmed))
	c.route(mux, "POST /admin/provisional/rollback", permManagePolicy, c.handleResolve("cpa.rollback", dispositionRolledBack))
	c.route(mux, "GET /admin/activations", permReadHistory, c.handleActivations)
	c.route(mux, "GET /admin/overrides", permRead, c.handleOverrides)
	c.route(mux, "GET /admin/overrides/history", permReadHistory, c.handleOverrideHistory)
	c.route(mux, "GET /admin/overrides/{id}", permRead, c.handleOverride)
	c.route(mux, "POST /admin/overrides/validate", permManagePolicy, c.handleValidateOverride)
	c.route(mux, "POST /admin/overrides", permManagePolicy, c.handleCreateOverride)
	c.route(mux, "DELETE /admin/overrides/{id}", permManagePolicy, c.handleRevokeOverride)
	c.route(mux, "GET /admin/history", permReadHistory, c.handleHistory)
	c.route(mux, "GET /admin/history/{id}", permReadHistory, c.handleHistoryEntry)
	c.route(mux, "GET /admin/security-log", permReadHistory, c.handleSecurityLog)
	if c.ui != nil {
		// Only the page and its flat assets directory exist: no fallback page
		// can answer an unknown path, so an API path never receives HTML.
		mux.HandleFunc("GET /{$}", c.serveUI)
		mux.HandleFunc("GET /assets/{file}", c.serveUI)
	}
	registerTestRoutes(c, mux)

	c.admin = &http.Server{
		Handler:           c.checkHost(detachWrites(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func(admin *http.Server) {
		if err := admin.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.logger.Error("admin listener stopped", "error", err)
		}
	}(c.admin)
	return nil
}

// stopAdmin stops a started Admin API server (nil: none was started). It
// waits until ctx is done for requests in progress; if any remain, it closes
// their connections and reports the incomplete shutdown.
func stopAdmin(ctx context.Context, admin *http.Server) error {
	if admin == nil {
		return nil
	}
	if err := admin.Shutdown(ctx); err != nil {
		return errors.Join(fmt.Errorf("admin listener shutdown: %w", err), admin.Close())
	}
	return nil
}

// checkHost refuses any request whose Host is not exactly the admin listener
// address, so a name that resolves to loopback cannot be used to reach the
// API from another site's pages (DNS rebinding).
func (c *Controller) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != c.config.AdminListen {
			c.refuse(w, r, http.StatusBadRequest, "bad_request", "bad_host", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// detachWrites keeps a state-changing request running when its client
// disconnects: a change is committed in steps, and stopping between them
// would leave its outcome unrecorded and fail the audit log. Reads keep
// their cancellation, so a closed event stream ends.
func detachWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unsafeMethod(r.Method) {
			r = r.WithContext(context.WithoutCancel(r.Context()))
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin requires the Origin header of a state-changing request to name
// the admin listener itself. A request without one is refused: a browser
// always sends it on such requests, and other clients can.
func (c *Controller) sameOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	return len(origins) == 1 && origins[0] == "http://"+c.config.AdminListen
}

func unsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}

// route registers a handler that runs only for an authenticated administrator
// holding the required permission (0: any administrator, for their own
// session). A state-changing request also needs the admin origin and the
// session's CSRF token.
func (c *Controller) route(mux *http.ServeMux, pattern string, required permission, handler adminHandler) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		who, ok := c.authenticate(w, r)
		if !ok {
			return
		}
		if unsafeMethod(r.Method) {
			if !c.sameOrigin(r) {
				c.refuse(w, r, http.StatusForbidden, "forbidden", "bad_origin", &who)
				return
			}
			token := r.Header.Get(csrfHeader)
			if subtle.ConstantTimeCompare([]byte(token), []byte(who.session.csrf)) != 1 {
				c.refuse(w, r, http.StatusForbidden, "forbidden", "csrf", &who)
				return
			}
		}
		if who.admin.Permissions&required != required {
			c.refuse(w, r, http.StatusForbidden, "forbidden", "permission", &who)
			return
		}
		if !unsafeMethod(r.Method) {
			c.audit.Record(Event{Kind: "admin.read", Method: r.Method, Path: r.URL.Path, Actor: who.admin.Name, Session: who.session.reference})
		}
		handler(w, r, who)
	})
}

// authenticate resolves the session cookie to a live session and re-reads the
// administrator, so a quarantined administrator or a replaced credential loses
// access at once.
func (c *Controller) authenticate(w http.ResponseWriter, r *http.Request) (caller, bool) {
	reason := "no_session"
	var who caller
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		who.token = cookie.Value
		who.session, reason = c.sessions.lookup(cookie.Value, time.Now())
	}
	if reason == "" {
		admin, found, err := c.store.administrator(r.Context(), who.session.admin)
		if err != nil {
			c.logger.Error("reading administrator", "error", err)
			adminError(w, http.StatusServiceUnavailable, "unavailable", "")
			return caller{}, false
		}
		who.admin = admin
		if !found || admin.Status != "active" || admin.Credential != who.session.credential {
			c.sessions.revoke(who.token)
			reason = "credential_changed"
		}
	}
	if reason != "" {
		c.refuse(w, r, http.StatusUnauthorized, "unauthenticated", reason, nil)
		return caller{}, false
	}
	return who, true
}

// refuse answers a refused admin request and records why. Only the error code
// reaches the client.
func (c *Controller) refuse(w http.ResponseWriter, r *http.Request, status int, code, reason string, who *caller) {
	event := Event{Kind: "admin.rejected", Method: r.Method, Path: r.URL.Path, Status: status, Reason: reason}
	if who != nil {
		event.Kind, event.Actor, event.Session = "admin.denied", who.admin.Name, who.session.reference
	}
	c.audit.Record(event)
	adminError(w, status, code, "")
}

// adminError answers with an error code and, where it helps the client, the
// name of the offending field. Nothing else about the failure is disclosed.
func adminError(w http.ResponseWriter, status int, code, field string) {
	body := map[string]string{"error": code}
	if field != "" {
		body["field"] = field
	}
	writeJSON(w, status, body)
}

func (c *Controller) handleHealth(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	if state != modeNormal && state != modeBootstrap && state != modeRecovery {
		state = "unavailable"
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": state})
}
