// Admin API enrolment, sign-in, session and password handlers.

package gateway

import (
	"net/http"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

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
	if !store.IsAdminName(body.Name) || !validPassword(body.Password) {
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
	e, found, err := c.store.Enrolment(r.Context(), tokenHash(body.Secret))
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
	writeJSON(w, http.StatusCreated, map[string]any{"name": e.Name, "permissions": e.Permissions.Names()})
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
	admin, found, err := c.store.Administrator(r.Context(), body.Name)
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

func sessionView(admin store.Administrator, s session) map[string]any {
	return map[string]any{
		"name":        admin.Name,
		"permissions": admin.Permissions.Names(),
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
