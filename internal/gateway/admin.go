package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
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
	c.route(mux, "GET /admin/subjects", permRead, c.handleSubjects)
	c.route(mux, "GET /admin/subjects/{id}", permRead, c.handleSubject)
	c.route(mux, "PUT /admin/subjects/{id}", permManageSubjects, c.handlePutSubject)
	c.route(mux, "DELETE /admin/subjects/{id}", permManageSubjects, c.handleDeleteSubject)
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
	if c.ui != nil {
		// Only the page and its flat assets directory exist: no fallback page
		// can answer an unknown path, so an API path never receives HTML.
		mux.HandleFunc("GET /{$}", c.serveUI)
		mux.HandleFunc("GET /assets/{file}", c.serveUI)
	}
	registerTestRoutes(c, mux)

	c.admin = &http.Server{
		Handler:           c.checkHost(mux),
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

// decodeBody reads a bounded JSON request body into value. It refuses another
// media type, an oversized body, repeated or non-lowercase members, unknown
// fields and trailing data, and answers the request itself when it does.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, value any) bool {
	if !isJSONMediaType(r.Header.Values("Content-Type")) {
		adminError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		adminError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "")
		return false
	}
	if err != nil || checkMembers(body, maxPolicyDepth+2) != nil || decodeStrict(body, value) != nil {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return false
	}
	return true
}

// ifMatch reads the required policy precondition "r<N>". It answers 428 when
// it is missing and 400 when it is malformed.
func ifMatch(w http.ResponseWriter, r *http.Request) (sql.NullInt64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		adminError(w, http.StatusPreconditionRequired, "precondition_required", "")
		return sql.NullInt64{}, false
	}
	text, ok := strings.CutPrefix(values[0], `"r`)
	text, closed := strings.CutSuffix(text, `"`)
	revision, err := strconv.ParseInt(text, 10, 64)
	if len(values) != 1 || !ok || !closed || err != nil || revision < 0 || strconv.FormatInt(revision, 10) != text {
		adminError(w, http.StatusBadRequest, "bad_request", "if_match")
		return sql.NullInt64{}, false
	}
	return sql.NullInt64{Int64: revision, Valid: true}, true
}

func policyETag(revision int64) string { return fmt.Sprintf(`"r%d"`, revision) }

// idempotencyKey reads the required Idempotency-Key header.
func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	valid := len(values) == 1 && len(values[0]) >= minKeyLength && len(values[0]) <= maxKeyLength
	for i := 0; valid && i < len(values[0]); i++ {
		ch := values[0][i]
		valid = (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_'
	}
	if !valid {
		adminError(w, http.StatusBadRequest, "bad_request", "idempotency_key")
		return "", false
	}
	return values[0], true
}

// adminTransition names a mutation by its administrator and idempotency key,
// so two administrators can never collide and a retry finds its own record.
func adminTransition(who caller, key, kind, target string, expected sql.NullInt64, payload any) transitionRecord {
	return transitionRecord{
		ID:       "admin:" + who.admin.Name + ":" + key,
		Kind:     kind,
		Actor:    who.admin.Name,
		Target:   target,
		Expected: expected,
		Digest:   requestDigest(kind, target, expected, payload),
	}
}

// page reads the list parameters: limit (1 to 100) and an optional cursor.
// Any other parameter is refused.
func page(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	query := r.URL.Query()
	limit := defaultPageSize
	valid := true
	for name, values := range query {
		valid = valid && len(values) == 1 && (name == "limit" || name == "after")
	}
	if text := query.Get("limit"); valid && text != "" {
		n, err := strconv.Atoi(text)
		valid = err == nil && n >= 1 && n <= maxPageSize && strconv.Itoa(n) == text
		limit = n
	}
	if !valid {
		adminError(w, http.StatusBadRequest, "bad_request", "page")
		return "", 0, false
	}
	return query.Get("after"), limit, true
}

// numericCursor reads an integer list cursor; absent means from the start.
func numericCursor(w http.ResponseWriter, after string) (int64, bool) {
	if after == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(after, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != after {
		adminError(w, http.StatusBadRequest, "bad_request", "page")
		return 0, false
	}
	return n, true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	text := r.PathValue("id")
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != text {
		adminError(w, http.StatusNotFound, "not_found", "")
		return 0, false
	}
	return n, true
}

type list struct {
	Items any    `json:"items"`
	Next  string `json:"next,omitempty"`
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

// acquireHashing takes one of the bounded password-hashing slots.
func (c *Controller) acquireHashing() bool {
	select {
	case c.hashing <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c *Controller) releaseHashing() { <-c.hashing }

func (c *Controller) handleEnrol(w http.ResponseWriter, r *http.Request) {
	if !c.sameOrigin(r) {
		c.refuse(w, r, http.StatusForbidden, "forbidden", "bad_origin", nil)
		return
	}
	var body struct {
		Name     string `json:"name"`
		Secret   string `json:"secret"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, maxAdminBody, &body) {
		return
	}
	if !isAdminName(body.Name) || !validPassword(body.Password) {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	if c.store == nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	now := time.Now()
	if reason := c.throttle.check(body.Name, now); reason != "" {
		c.refuse(w, r, http.StatusTooManyRequests, "too_many_attempts", reason, nil)
		return
	}
	e, found, err := c.store.enrolment(r.Context(), tokenHash(body.Secret))
	if err != nil {
		c.logger.Error("reading enrolment", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	if !found || e.Name != body.Name || !now.Before(e.ExpiresAt) {
		c.throttle.failed(body.Name, false, now)
		c.refuse(w, r, http.StatusUnauthorized, "unauthenticated", "enrolment_invalid", nil)
		return
	}
	if !c.acquireHashing() {
		adminError(w, http.StatusServiceUnavailable, "busy", "")
		return
	}
	passwordHash := hashPassword(body.Password)
	c.releaseHashing()

	outcome, status := c.Enrol(r.Context(), e, passwordHash)
	if status != http.StatusOK {
		writeJSON(w, status, outcome)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": e.Name, "permissions": e.Permissions.names()})
}

func (c *Controller) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !c.sameOrigin(r) {
		c.refuse(w, r, http.StatusForbidden, "forbidden", "bad_origin", nil)
		return
	}
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, maxAdminBody, &body) {
		return
	}
	if c.store == nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	now := time.Now()
	if reason := c.throttle.check(body.Name, now); reason != "" {
		c.refuse(w, r, http.StatusTooManyRequests, "too_many_attempts", reason, nil)
		return
	}
	admin, found, err := c.store.administrator(r.Context(), body.Name)
	if err != nil {
		c.logger.Error("reading administrator", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}

	// An unknown or quarantined name is verified against a dummy hash, so the
	// answer and its timing are the same as for a wrong password.
	active := found && admin.Status == "active"
	encoded := dummyHash()
	if active {
		encoded = admin.Password
	}
	if !c.acquireHashing() {
		adminError(w, http.StatusServiceUnavailable, "busy", "")
		return
	}
	verified := verifyPassword(encoded, body.Password) && active
	c.releaseHashing()
	if !verified {
		c.throttle.failed(body.Name, found, now)
		c.refuse(w, r, http.StatusUnauthorized, "unauthenticated", "bad_credentials", nil)
		return
	}
	c.throttle.succeeded(body.Name)

	token, s, err := c.sessions.create(admin.Name, admin.Credential, now)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "session_limit", "")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	c.audit.Record(Event{Kind: "admin.authenticated", Method: r.Method, Path: r.URL.Path, Actor: admin.Name, Session: s.reference})
	writeJSON(w, http.StatusCreated, sessionView(admin, s))
}

func sessionView(admin administrator, s session) map[string]any {
	return map[string]any{
		"name":        admin.Name,
		"permissions": admin.Permissions.names(),
		"csrf_token":  s.csrf,
		"expires_at":  s.expires.UTC(),
	}
}

func (c *Controller) handleWhoAmI(w http.ResponseWriter, r *http.Request, who caller) {
	writeJSON(w, http.StatusOK, sessionView(who.admin, who.session))
}

func (c *Controller) handleLogout(w http.ResponseWriter, r *http.Request, who caller) {
	c.sessions.revoke(who.token)
	c.audit.Record(Event{Kind: "admin.logout", Method: r.Method, Path: r.URL.Path, Actor: who.admin.Name, Session: who.session.reference})
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// handlePassword changes the caller's own password after verifying the
// current one, and ends all of the caller's sessions.
func (c *Controller) handlePassword(w http.ResponseWriter, r *http.Request, who caller) {
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decodeBody(w, r, maxAdminBody, &body) {
		return
	}
	if !validPassword(body.New) {
		adminError(w, http.StatusBadRequest, "bad_request", "new_password")
		return
	}
	now := time.Now()
	if reason := c.throttle.check(who.admin.Name, now); reason != "" {
		c.refuse(w, r, http.StatusTooManyRequests, "too_many_attempts", reason, &who)
		return
	}
	if !c.acquireHashing() {
		adminError(w, http.StatusServiceUnavailable, "busy", "")
		return
	}
	verified := verifyPassword(who.admin.Password, body.Current)
	var passwordHash string
	if verified {
		passwordHash = hashPassword(body.New)
	}
	c.releaseHashing()
	if !verified {
		c.throttle.failed(who.admin.Name, true, now)
		c.refuse(w, r, http.StatusUnauthorized, "unauthenticated", "bad_credentials", &who)
		return
	}

	outcome, status := c.ChangePassword(r.Context(), who.admin, passwordHash)
	if status != http.StatusOK {
		writeJSON(w, status, outcome)
		return
	}
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (c *Controller) handleStatus(w http.ResponseWriter, r *http.Request, who caller) {
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	status := map[string]any{
		"state":          state,
		"admission":      c.admission(),
		"audit_failed":   c.audit.failed.Load(),
		"jwt_key_sha256": c.verifier.fingerprint,
	}
	if g := c.current.Load(); g != nil {
		status["revision"] = g.Revision
		status["generation"] = g.ID
		status["profile"] = g.Profile
		status["overrides"] = append([]int64{}, g.Overrides...)
		if !g.NotAfter.IsZero() {
			status["not_after"] = g.NotAfter.UTC()
		}
	}
	// The stored base, which differs from the served generation only while
	// the gateway is unavailable.
	if a, err := c.store.load(r.Context()); err == nil && a.mode == modeNormal {
		status["confirmed"] = a.confirmed
		if a.provisional != 0 {
			status["provisional"] = a.provisional
			status["deadline"] = a.deadline.UTC()
		}
	}
	writeJSON(w, http.StatusOK, status)
}

// confirmedPolicy reads the confirmed base. It answers the request itself and
// returns false when no confirmed base is serving.
func (c *Controller) confirmedPolicy(w http.ResponseWriter, r *http.Request) (revisionInfo, Policy, bool) {
	a, err := c.store.load(r.Context())
	if err != nil {
		c.logger.Error("reading authority", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return revisionInfo{}, Policy{}, false
	}
	if a.mode != modeNormal {
		adminError(w, http.StatusConflict, "no_confirmed_policy", "")
		return revisionInfo{}, Policy{}, false
	}
	revision, ok, err := c.store.revision(r.Context(), a.confirmed)
	if err == nil && !ok {
		err = fmt.Errorf("confirmed revision %d missing", a.confirmed)
	}
	var p Policy
	if err == nil {
		p, err = DecodePolicy(revision.Document)
	}
	if err != nil {
		c.logger.Error("reading confirmed policy", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return revisionInfo{}, Policy{}, false
	}
	return revision, p, true
}

func (c *Controller) handlePolicy(w http.ResponseWriter, r *http.Request, who caller) {
	revision, _, ok := c.confirmedPolicy(w, r)
	if !ok {
		return
	}
	w.Header().Set("ETag", policyETag(revision.ID))
	writeJSON(w, http.StatusOK, revision)
}

func (c *Controller) handleRevisions(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	items, err := c.store.revisions(r.Context(), cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	result := list{Items: items}
	if len(items) == limit {
		result.Next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

func (c *Controller) handleRevision(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	revision, found, err := c.store.revision(r.Context(), id)
	switch {
	case err != nil:
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
	case !found:
		adminError(w, http.StatusNotFound, "not_found", "")
	default:
		writeJSON(w, http.StatusOK, revision)
	}
}

func (c *Controller) handleSubjects(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	confirmed, p, ok := c.confirmedPolicy(w, r)
	if !ok {
		return
	}
	subjects := slices.SortedFunc(slices.Values(p.Subjects), func(a, b Subject) int { return strings.Compare(a.ID, b.ID) })
	start, _ := slices.BinarySearchFunc(subjects, after, func(s Subject, id string) int { return strings.Compare(s.ID, id) })
	if start < len(subjects) && after != "" && subjects[start].ID == after {
		start++
	}
	items := subjects[start:min(start+limit, len(subjects))]
	result := list{Items: items}
	if len(items) == limit && start+limit < len(subjects) {
		result.Next = items[len(items)-1].ID
	}
	w.Header().Set("ETag", policyETag(confirmed.ID))
	writeJSON(w, http.StatusOK, result)
}

func (c *Controller) handleSubject(w http.ResponseWriter, r *http.Request, who caller) {
	confirmed, p, ok := c.confirmedPolicy(w, r)
	if !ok {
		return
	}
	index := slices.IndexFunc(p.Subjects, func(s Subject) bool { return s.ID == r.PathValue("id") })
	if index < 0 {
		adminError(w, http.StatusNotFound, "not_found", "")
		return
	}
	w.Header().Set("ETag", policyETag(confirmed.ID))
	writeJSON(w, http.StatusOK, p.Subjects[index])
}

// handlePutSubject creates or replaces one subject's trusted membership and
// publishes the new effective generation.
func (c *Controller) handlePutSubject(w http.ResponseWriter, r *http.Request, who caller) {
	id := r.PathValue("id")
	if !isSubjectID(id) {
		adminError(w, http.StatusBadRequest, "bad_request", "id")
		return
	}
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Status string   `json:"status"`
		Roles  []string `json:"roles"`
	}
	if !decodeBody(w, r, maxAdminBody, &body) {
		return
	}
	subject := Subject{ID: id, Status: body.Status, Roles: body.Roles}
	if subject.Roles == nil {
		subject.Roles = []string{}
	}
	t := adminTransition(who, key, "membership.put", "subject "+id, expected, subject)
	outcome, status := c.ChangeMembership(r.Context(), t, id, &subject)
	c.writePolicyOutcome(w, r, outcome, status)
}

func (c *Controller) handleDeleteSubject(w http.ResponseWriter, r *http.Request, who caller) {
	id := r.PathValue("id")
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	t := adminTransition(who, key, "membership.delete", "subject "+id, expected, nil)
	outcome, status := c.ChangeMembership(r.Context(), t, id, nil)
	c.writePolicyOutcome(w, r, outcome, status)
}

// writePolicyOutcome returns the receipt with the policy ETag the client
// needs next: the confirmed revision after a change, the current one after a
// stale precondition.
func (c *Controller) writePolicyOutcome(w http.ResponseWriter, r *http.Request, outcome Outcome, status int) {
	switch {
	case status == http.StatusOK && outcome.Provisional != 0 && outcome.Disposition == "":
		// An activation leaves the confirmed revision as it was.
		w.Header().Set("ETag", policyETag(outcome.RollbackTarget))
	case status == http.StatusOK && outcome.Revision != 0:
		w.Header().Set("ETag", policyETag(outcome.Revision))
	case status == http.StatusPreconditionFailed:
		if a, err := c.store.load(r.Context()); err == nil && a.mode == modeNormal {
			w.Header().Set("ETag", policyETag(a.confirmed))
		}
	}
	writeJSON(w, status, outcome)
}

// candidateView marks every candidate as not active: staging never changes
// the enforced policy.
type candidateView struct {
	candidateRecord
	Active   bool            `json:"active"`
	Document json.RawMessage `json:"document,omitempty"`
}

func (c *Controller) handleCandidates(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	records, err := c.store.candidates(r.Context(), cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	items := make([]candidateView, 0, len(records))
	for _, record := range records {
		items = append(items, candidateView{candidateRecord: record})
	}
	result := list{Items: items}
	if len(records) == limit {
		result.Next = strconv.FormatInt(records[len(records)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

func (c *Controller) handleCandidate(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	record, found, err := c.store.candidate(r.Context(), id)
	switch {
	case err != nil:
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
	case !found:
		adminError(w, http.StatusNotFound, "not_found", "")
	default:
		writeJSON(w, http.StatusOK, candidateView{candidateRecord: record, Document: record.Document})
	}
}

// handlePostCandidate validates and stores a complete policy candidate. The
// If-Match precondition names the confirmed revision it was prepared against.
func (c *Controller) handlePostCandidate(w http.ResponseWriter, r *http.Request, who caller) {
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if !decodeBody(w, r, maxCandidateBody, &body) {
		return
	}
	var compact bytes.Buffer
	if json.Compact(&compact, body.Policy) != nil {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	t := adminTransition(who, key, "candidate.create", "candidate", expected, compact.String())
	outcome, status := c.StageCandidate(r.Context(), t, body.Policy)
	if status != http.StatusOK {
		writeJSON(w, status, outcome)
		return
	}
	record, _, err := c.store.candidate(r.Context(), outcome.Candidate)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"receipt": outcome, "candidate": candidateView{candidateRecord: record}})
}

func (c *Controller) handleWithdrawCandidate(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	t := adminTransition(who, key, "candidate.withdraw", fmt.Sprintf("candidate %d", id), sql.NullInt64{}, nil)
	outcome, status := c.WithdrawCandidate(r.Context(), t, id)
	writeJSON(w, status, outcome)
}

// handleEstablish confirms a deny-all candidate as the first base (genesis)
// or the base that ends a recovery. The checksum shows the administrator
// confirmed the exact document.
func (c *Controller) handleEstablish(kind string) adminHandler {
	return func(w http.ResponseWriter, r *http.Request, who caller) {
		var body struct {
			Candidate int64  `json:"candidate"`
			Checksum  string `json:"checksum"`
		}
		if !decodeBody(w, r, maxAdminBody, &body) {
			return
		}
		outcome, status := c.EstablishBase(r.Context(), kind, who.admin.Name, body.Candidate, body.Checksum)
		writeJSON(w, status, outcome)
	}
}

// handleActivate makes a staged candidate the provisional active base. The
// If-Match precondition names the confirmed revision that becomes the
// rollback target; the checksum shows the administrator chose the exact
// document.
func (c *Controller) handleActivate(w http.ResponseWriter, r *http.Request, who caller) {
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Candidate            int64  `json:"candidate"`
		Checksum             string `json:"checksum"`
		ConfirmWithinSeconds int64  `json:"confirm_within_seconds"`
	}
	if !decodeBody(w, r, maxAdminBody, &body) {
		return
	}
	t := adminTransition(who, key, "cpa.activate", fmt.Sprintf("candidate %d", body.Candidate), expected, body)
	outcome, status := c.Activate(r.Context(), t, body.Candidate, body.Checksum, body.ConfirmWithinSeconds)
	c.writePolicyOutcome(w, r, outcome, status)
}

// handleResolve confirms or rolls back the provisional revision named in the
// body.
func (c *Controller) handleResolve(kind, disposition string) adminHandler {
	return func(w http.ResponseWriter, r *http.Request, who caller) {
		expected, ok := ifMatch(w, r)
		if !ok {
			return
		}
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		var body struct {
			Revision int64 `json:"revision"`
		}
		if !decodeBody(w, r, maxAdminBody, &body) {
			return
		}
		t := adminTransition(who, key, kind, fmt.Sprintf("revision %d", body.Revision), expected, body)
		outcome, status := c.Resolve(r.Context(), t, body.Revision, disposition)
		c.writePolicyOutcome(w, r, outcome, status)
	}
}

// handleProvisional reports the current provisional base: its activation
// record, the generation serving it and every control target it changes
// relative to its rollback target.
func (c *Controller) handleProvisional(w http.ResponseWriter, r *http.Request, who caller) {
	ctx := r.Context()
	a, err := c.store.load(ctx)
	if err != nil {
		c.logger.Error("reading authority", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	w.Header().Set("ETag", policyETag(a.confirmed))
	if a.provisional == 0 {
		adminError(w, http.StatusNotFound, "no_provisional", "")
		return
	}
	record, found, err := c.store.activation(ctx, "revision", a.provisional)
	if err == nil && !found {
		err = fmt.Errorf("activation of revision %d missing", a.provisional)
	}
	var before, after Policy
	if err == nil {
		before, err = c.storedPolicy(ctx, a.confirmed)
	}
	if err == nil {
		after, err = c.storedPolicy(ctx, a.provisional)
	}
	if err != nil {
		c.logger.Error("reading the provisional base", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	view := map[string]any{"activation": record, "changes": policyChanges(before, after)}
	if g := c.current.Load(); g != nil && g.Revision == a.provisional {
		view["generation"] = g.ID
	}
	writeJSON(w, http.StatusOK, view)
}

func (c *Controller) storedPolicy(ctx context.Context, id int64) (Policy, error) {
	r, found, err := c.store.revision(ctx, id)
	if err == nil && !found {
		err = fmt.Errorf("revision %d missing", id)
	}
	if err != nil {
		return Policy{}, err
	}
	return DecodePolicy(r.Document)
}

// handleActivations lists every provisional base with its final disposition.
func (c *Controller) handleActivations(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	items, err := c.store.activations(r.Context(), cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	result := list{Items: items}
	if len(items) == limit {
		result.Next = strconv.FormatInt(items[len(items)-1].Revision, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

// handleOverrides reports the overrides active now and the effective policy
// they belong to: the active base revision, the confirmed and provisional
// revisions and the serving generation, whose identity names the base and
// every applied override.
func (c *Controller) handleOverrides(w http.ResponseWriter, r *http.Request, who caller) {
	a, err := c.store.load(r.Context())
	if err != nil {
		c.logger.Error("reading authority", "error", err)
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	view := map[string]any{
		"revision":  a.revision,
		"confirmed": a.confirmed,
		"overrides": append([]override{}, a.overrides...),
	}
	if a.provisional != 0 {
		view["provisional"] = a.provisional
	}
	if g := c.current.Load(); g != nil {
		view["generation"] = g.ID
		view["generation_overrides"] = append([]int64{}, g.Overrides...)
	}
	w.Header().Set("ETag", policyETag(a.confirmed))
	writeJSON(w, http.StatusOK, view)
}

func (c *Controller) handleOverride(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, found, err := c.store.override(r.Context(), id)
	switch {
	case err != nil:
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
	case !found:
		adminError(w, http.StatusNotFound, "not_found", "")
	default:
		writeJSON(w, http.StatusOK, o)
	}
}

// handleOverrideHistory lists every override with its final disposition.
func (c *Controller) handleOverrideHistory(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	items, err := c.store.overrides(r.Context(), `WHERE id > ? ORDER BY id LIMIT ?`, cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	result := list{Items: items}
	if len(items) == limit {
		result.Next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

// handleValidateOverride answers whether a proposed override would be
// created now. Nothing is created or recorded in the history.
func (c *Controller) handleValidateOverride(w http.ResponseWriter, r *http.Request, who caller) {
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	var request OverrideRequest
	if !decodeBody(w, r, maxAdminBody, &request) {
		return
	}
	outcome, status := c.ValidateOverride(r.Context(), who.admin.Name, expected.Int64, request)
	writeJSON(w, status, outcome)
}

// handleCreateOverride creates a temporary override of the confirmed base.
// The If-Match precondition names the confirmed revision the administrator
// saw.
func (c *Controller) handleCreateOverride(w http.ResponseWriter, r *http.Request, who caller) {
	expected, ok := ifMatch(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var request OverrideRequest
	if !decodeBody(w, r, maxAdminBody, &request) {
		return
	}
	t := adminTransition(who, key, "tpo.create", "override", expected, request)
	outcome, status := c.CreateOverride(r.Context(), t, request)
	switch status {
	case http.StatusOK:
		status = http.StatusCreated
	case http.StatusPreconditionFailed:
		if a, err := c.store.load(r.Context()); err == nil && a.mode == modeNormal {
			w.Header().Set("ETag", policyETag(a.confirmed))
		}
	}
	writeJSON(w, status, outcome)
}

// handleRevokeOverride ends one active override.
func (c *Controller) handleRevokeOverride(w http.ResponseWriter, r *http.Request, who caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	t := adminTransition(who, key, "tpo.revoke", fmt.Sprintf("override %d", id), sql.NullInt64{}, nil)
	outcome, status := c.RevokeOverride(r.Context(), t, id)
	writeJSON(w, status, outcome)
}

func (c *Controller) handleHistory(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	items, err := c.store.history(r.Context(), cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	result := list{Items: items}
	if len(items) == limit {
		result.Next = strconv.FormatInt(items[len(items)-1].Seq, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

func (c *Controller) handleHistoryEntry(w http.ResponseWriter, r *http.Request, who caller) {
	entry, found, err := c.store.historyEntry(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
	case !found:
		adminError(w, http.StatusNotFound, "not_found", "")
	default:
		writeJSON(w, http.StatusOK, entry)
	}
}

// assetTypes are the only asset types served, each with a fixed content type
// rather than one guessed from the host's registry. The only page is the
// index.
var assetTypes = map[string]string{
	".js":  "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8",
	".svg": "image/svg+xml",
}

// uiPolicy lets a frontend, the dashboard or the website, load only its own
// scripts and styles and call only its own origin, and forbids framing, so
// injected markup cannot run script or exfiltrate data.
const uiPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// serveUI serves one dashboard file. The files hold no data, so no session is
// needed; everything the dashboard shows comes from the Admin API.
func (c *Controller) serveUI(w http.ResponseWriter, r *http.Request) {
	name, contentType := "index.html", "text/html; charset=utf-8"
	if file := r.PathValue("file"); file != "" {
		name, contentType = "assets/"+file, assetTypes[path.Ext(file)]
	}
	if contentType == "" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	serveFrontendFile(w, r, c.ui, name, contentType)
}

// serveFrontendFile answers with one file of a built frontend under the
// frontends' content policy, or 404 when it cannot be read.
func serveFrontendFile(w http.ResponseWriter, r *http.Request, files fs.FS, name, contentType string) {
	data, err := fs.ReadFile(files, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", uiPolicy)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
