package gateway

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// accessCheck asks whether a request would pass the checks that do not depend
// on live traffic or a real body: routing, the endpoint's access, the blocked
// accounts and the role permissions. Roles stand for a verified token's roles;
// Host and Headers stand for the request's, which conditions select by.
type accessCheck struct {
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Host      string            `json:"host"`
	Headers   map[string]string `json:"headers"`
	Roles     []string          `json:"roles"`
	Subject   *string           `json:"subject"`
	Candidate *int64            `json:"candidate"`
}

type accessAnswer struct {
	Decision  string  `json:"decision"`
	Code      *string `json:"code"`
	Check     *string `json:"check"`
	Operation *string `json:"operation"`
	API       *string `json:"api"`
	Public    bool    `json:"public"`
	// ForwardedPath is the path an allowed request reaches its upstream
	// with; null when the request is refused.
	ForwardedPath *string  `json:"forwarded_path"`
	Roles         []string `json:"roles"`
	IgnoredRoles  []string `json:"ignored_roles"`
	Generation    string   `json:"generation"`
	Candidate     *int64   `json:"candidate"`
	NotChecked    []string `json:"not_checked"`
}

// handleAccessCheck answers with the generation in force, or with a stored
// draft compiled with the active overrides, through the same matching and
// permission code as real requests. It changes nothing and is no second
// evaluator: every decision below is the request handler's own function.
func (c *Controller) handleAccessCheck(w http.ResponseWriter, r *http.Request, who caller) {
	var check accessCheck
	if !decodeBody(w, r, maxAdminBody, &check) {
		return
	}
	if !slices.Contains([]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}, check.Method) {
		adminError(w, http.StatusBadRequest, "bad_request", "method")
		return
	}
	path, ok := checkedPath(check.Path)
	if !ok {
		adminError(w, http.StatusBadRequest, "bad_request", "path")
		return
	}
	if len(check.Roles) > policy.MaxRoles || slices.ContainsFunc(check.Roles, func(role string) bool { return role == "" || len(role) > policy.MaxIDLength }) {
		adminError(w, http.StatusBadRequest, "bad_request", "roles")
		return
	}
	inbound, field := checkedInbound(check.Host, check.Headers)
	if field != "" {
		adminError(w, http.StatusBadRequest, "bad_request", field)
		return
	}
	if check.Subject != nil && (*check.Subject == "" || len(*check.Subject) > policy.MaxIDLength) {
		adminError(w, http.StatusBadRequest, "bad_request", "subject")
		return
	}

	g := c.Current()
	if check.Candidate != nil {
		var ok bool
		if _, g, ok = c.candidateGeneration(w, r, *check.Candidate); !ok {
			return
		}
	} else if g == nil || g.Expired(compile.TrustedNow()) {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	blocked := c.blocked.Load()
	if blocked == nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}

	answer := accessAnswer{Generation: g.ID, Candidate: check.Candidate, Roles: []string{}, IgnoredRoles: []string{}, NotChecked: []string{"rate", "body"}}
	refuse := func(check, code string) {
		answer.Decision, answer.Check, answer.Code = "refused", &check, &code
		writeJSON(w, http.StatusOK, answer)
	}
	target, rule := g.Match(path, check.Method, inbound)
	if target == nil {
		refuse("route", "not_found")
		return
	}
	if rule == nil {
		refuse("route", "method_not_allowed")
		return
	}
	operation, api := rule.Operation(), rule.API()
	answer.Operation, answer.API, answer.Public = &operation, &api, rule.Public()
	if !rule.Permitted() {
		refuse("access", "forbidden")
		return
	}
	if !rule.Public() {
		// A real request also needs a valid token from the trusted issuer;
		// the roles given here stand for its verified roles.
		answer.NotChecked = []string{"token", "rate", "body"}
		roles, ignored, reason := g.Authorise(check.Roles, rule)
		answer.Roles, answer.IgnoredRoles = nonNil(roles), nonNil(ignored)
		if check.Subject != nil {
			if _, found := (*blocked)[*check.Subject]; found {
				refuse("blocked", "account_blocked")
				return
			}
		}
		if reason != "" {
			refuse("role", "forbidden")
			return
		}
	}
	destination, ok := rule.Destination(path)
	if !ok {
		refuse("forward", "path_not_canonical")
		return
	}
	forwarded := destination.Path()
	answer.Decision, answer.ForwardedPath = "allowed", &forwarded
	writeJSON(w, http.StatusOK, answer)
}

// Bounds of the host and headers an access check may give: a host name with
// a port, and as many headers as conditions allow, each as long as one.
const (
	maxCheckHost    = 253 + len(":65535")
	maxCheckHeaders = 2 * policy.MaxWhenHeaders
	maxCheckValue   = 256
)

// checkedInbound bounds the host and headers given for conditions and puts
// the headers in request form; field names the refused member.
func checkedInbound(host string, headers map[string]string) (in compile.Inbound, field string) {
	if len(host) > maxCheckHost {
		return in, "host"
	}
	if len(headers) > maxCheckHeaders {
		return in, "headers"
	}
	header := http.Header{}
	for name, value := range headers {
		if name == "" || len(name) > policy.MaxHeaderName || len(value) > maxCheckValue {
			return in, "headers"
		}
		header.Set(name, value)
	}
	return compile.Inbound{Host: host, Header: header}, ""
}

// checkedPath applies the request handler's own canonical-path check to a
// path given as text, so the same targets are refused before routing.
func checkedPath(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") || len(text) > policy.MaxTargetLength || strings.ContainsAny(text, "?#") {
		return "", false
	}
	parsed, err := url.ParseRequestURI(text)
	if err != nil {
		return "", false
	}
	return compile.CanonicalPath(&http.Request{URL: parsed})
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
