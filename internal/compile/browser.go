// Browser access (CORS) and response header rules of an API: the compiler's
// checks and the headers the gateway answers with. Neither ever decides
// whether a request is admitted.

package compile

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// hstsValue is the Strict-Transport-Security value: one year.
const hstsValue = "max-age=31536000"

// CORS is an API's compiled browser access.
type CORS struct {
	origins     map[string]bool
	any         bool            // origins ["*"]
	methods     map[string]bool // exact method names
	headers     map[string]bool // lower-case request header names
	allowed     string          // Access-Control-Allow-Methods
	listed      string          // Access-Control-Allow-Headers
	expose      string          // Access-Control-Expose-Headers
	credentials bool
	maxAge      string
}

// compileCORS checks an API's cors member (field apis[i].cors). The checks
// that need the API's operations are in checkAPIs.
func compileCORS(c *policy.CORS, i int) (*CORS, error) {
	if c == nil {
		return nil, nil
	}
	field := "apis[" + strconv.Itoa(i) + "].cors"
	out := &CORS{origins: map[string]bool{}, methods: map[string]bool{}, headers: map[string]bool{}, credentials: c.Credentials}
	if len(c.Origins) < 1 || len(c.Origins) > policy.MaxCORSOrigins {
		return nil, policy.Invalid("INVALID_VALUE", "%s.origins", field)
	}
	for j, origin := range c.Origins {
		if origin == "*" && len(c.Origins) == 1 {
			out.any = true
			continue
		}
		if !isOrigin(origin) || out.origins[origin] {
			return nil, policy.Invalid("INVALID_VALUE", "%s.origins[%d]", field, j)
		}
		out.origins[origin] = true
	}
	if len(c.Methods) < 1 || len(c.Methods) > len(policy.AllowedMethods) {
		return nil, policy.Invalid("INVALID_VALUE", "%s.methods", field)
	}
	for j, method := range c.Methods {
		if !policy.AllowedMethods[method] || out.methods[method] {
			return nil, policy.Invalid("INVALID_VALUE", "%s.methods[%d]", field, j)
		}
		out.methods[method] = true
	}
	var err error
	if out.listed, err = headerList(c.Headers, policy.MaxCORSHeaders, out.headers, field+".headers"); err != nil {
		return nil, err
	}
	if out.expose, err = headerList(c.Expose, policy.MaxCORSExpose, map[string]bool{}, field+".expose"); err != nil {
		return nil, err
	}
	maxAge := int64(policy.DefaultCORSMaxAge)
	if c.MaxAgeSeconds != nil {
		maxAge = *c.MaxAgeSeconds
	}
	if maxAge < 0 || maxAge > policy.MaxCORSMaxAge {
		return nil, policy.Invalid("INVALID_VALUE", "%s.max_age_seconds", field)
	}
	out.maxAge = strconv.FormatInt(maxAge, 10)
	out.allowed = strings.Join(slices.Sorted(maps.Keys(out.methods)), ", ")
	return out, nil
}

// headerList checks a list of at most limit distinct header names, adds
// their lower-case forms to seen and returns them joined for a header value.
func headerList(names []string, limit int, seen map[string]bool, field string) (string, error) {
	if len(names) > limit {
		return "", policy.Invalid("INVALID_VALUE", "%s", field)
	}
	for j, name := range names {
		lower := strings.ToLower(name)
		if !isToken(name) || len(name) > policy.MaxHeaderName || seen[lower] {
			return "", policy.Invalid("INVALID_VALUE", "%s[%d]", field, j)
		}
		seen[lower] = true
	}
	return strings.Join(names, ", "), nil
}

// isOrigin accepts a lower-case http or https origin without path, query,
// user information or a default port, the form browsers send.
func isOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return false
	}
	defaultPort := (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443")
	return !defaultPort && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" &&
		strings.ToLower(origin) == origin && u.String() == origin
}

// isToken reports whether name is an RFC 9110 token.
func isToken(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alphanumeric && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

// Preflight returns the headers that allow a preflight from origin for
// method with the requested headers (Access-Control-Request-Headers field
// lines), or nil when the origin, the method or any requested header is not
// allowed.
func (c *CORS) Preflight(origin, method string, requested []string) http.Header {
	if !c.methods[method] {
		return nil
	}
	for _, line := range requested {
		for name := range strings.SplitSeq(line, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" && !c.headers[name] {
				return nil
			}
		}
	}
	h := c.Response(origin)
	if h == nil {
		return nil
	}
	h.Del("Access-Control-Expose-Headers")
	h.Set("Access-Control-Allow-Methods", c.allowed)
	if c.listed != "" {
		h.Set("Access-Control-Allow-Headers", c.listed)
	}
	h.Set("Access-Control-Max-Age", c.maxAge)
	return h
}

// Response returns the headers an actual request from origin gets, or nil
// when the origin is not allowed.
func (c *CORS) Response(origin string) http.Header {
	if !c.any && !c.origins[origin] {
		return nil
	}
	h := http.Header{}
	h.Set("Access-Control-Allow-Origin", origin)
	if c.any {
		h.Set("Access-Control-Allow-Origin", "*")
	}
	h.Set("Vary", "Origin")
	if c.expose != "" {
		h.Set("Access-Control-Expose-Headers", c.expose)
	}
	if c.credentials {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	return h
}

// ResponseRule is an API's compiled response header rule.
type ResponseRule struct {
	hsts, nosniff   bool
	frame, referrer string
	remove          []string
}

var (
	frameValues    = map[string]string{"deny": "DENY", "sameorigin": "SAMEORIGIN"}
	referrerValues = map[string]bool{"no-referrer": true, "strict-origin-when-cross-origin": true}
	// keptHeaders are never removed: the gateway's own headers and the
	// response's framing.
	keptHeaders = map[string]bool{"Content-Length": true, "Transfer-Encoding": true, "Connection": true}
)

// compileResponse checks an API's response_headers member.
func compileResponse(h *policy.ResponseHeaders, i int) (*ResponseRule, error) {
	if h == nil {
		return nil, nil
	}
	field := "apis[" + strconv.Itoa(i) + "].response_headers"
	out := &ResponseRule{hsts: h.HSTS, nosniff: h.Nosniff, frame: frameValues[h.Frame], referrer: h.Referrer}
	if h.Frame != "" && out.frame == "" {
		return nil, policy.Invalid("INVALID_VALUE", "%s.frame", field)
	}
	if h.Referrer != "" && !referrerValues[h.Referrer] {
		return nil, policy.Invalid("INVALID_VALUE", "%s.referrer", field)
	}
	if _, err := headerList(h.Remove, policy.MaxRemoveHeaders, map[string]bool{}, field+".remove"); err != nil {
		return nil, err
	}
	for j, name := range h.Remove {
		name = http.CanonicalHeaderKey(name)
		if IsGatewayHeader(name) || keptHeaders[name] || strings.HasPrefix(name, "Access-Control-") {
			return nil, policy.Invalid("INVALID_VALUE", "%s.remove[%d]", field, j)
		}
		out.remove = append(out.remove, name)
	}
	return out, nil
}

// Apply removes the listed headers and adds each security header the
// response does not already have. Strict-Transport-Security is added only
// to a response sent over TLS.
func (r *ResponseRule) Apply(h http.Header, tls bool) {
	for _, name := range r.remove {
		h.Del(name)
	}
	add := func(on bool, name, value string) {
		if on && h.Get(name) == "" {
			h.Set(name, value)
		}
	}
	add(r.hsts && tls, "Strict-Transport-Security", hstsValue)
	add(r.nosniff, "X-Content-Type-Options", "nosniff")
	add(r.frame != "", "X-Frame-Options", r.frame)
	add(r.referrer != "", "Referrer-Policy", r.referrer)
}
