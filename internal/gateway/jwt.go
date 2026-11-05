// Bearer token extraction and RS256 JWT verification.

package gateway

import (
	"encoding/base64"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/go-jose/go-jose/v4"
	josejson "github.com/go-jose/go-jose/v4/json"
)

// verifiedToken is constructed only after signature and claim checks succeed.
// Its asserted roles still need filtering through the captured policy.
type verifiedToken struct {
	subject string
	roles   []string
}

// bearerToken extracts the token from the request's Authorization fields.
// Exactly one field of the form "Bearer <token>" is accepted. Anything else,
// including a repeated field, is ambiguous and refused rather than
// interpreted. It returns the token, or the refusal reason.
func bearerToken(fields []string) (string, string) {
	if len(fields) == 0 {
		return "", "missing_credential"
	}
	if len(fields) > 1 {
		return "", "malformed_credential"
	}
	scheme, token, found := strings.Cut(fields[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || !isCompactToken(token) {
		return "", "malformed_credential"
	}
	return token, ""
}

// isCompactToken accepts a bounded string of three non-empty base64url
// segments separated by dots.
func isCompactToken(token string) bool {
	if len(token) > maxTokenBytes {
		return false
	}
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return false
	}
	for _, segment := range segments {
		if segment == "" {
			return false
		}
		for i := 0; i < len(segment); i++ {
			c := segment[i]
			alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !alphanumeric && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// verify checks a compact token at the given time and returns its subject,
// or the refusal reason. Nothing in the token is trusted until the signature
// has been verified with the configured key; the header is inspected before
// that only to refuse it.
func (v *verifier) verify(token string, now time.Time) (verifiedToken, string) {
	if !isCompactToken(token) {
		return verifiedToken{}, "malformed_credential"
	}
	header, _, _ := strings.Cut(token, ".")
	kid, reason := checkHeader(header)
	if reason != "" {
		return verifiedToken{}, reason
	}
	key := v.keys[kid]
	if key == nil {
		return verifiedToken{}, "unknown_kid"
	}
	signed, err := jose.ParseSignedCompact(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return verifiedToken{}, "malformed_credential"
	}
	payload, err := signed.Verify(key)
	if err != nil {
		return verifiedToken{}, "invalid_signature"
	}
	verified, reason := v.checkClaims(payload, now)
	if reason == "" {
		use := v.uses[kid]
		use.verified.Add(1)
		use.lastNano.Store(now.UnixNano())
	}
	return verified, reason
}

// checkHeader allows only the members "alg" (exactly RS256), "typ" (JWT) and
// "kid" (required). The key ID selects only a configured key; members that name or
// carry a key, such as "jku", "x5u", "x5c" or "jwk", or that add processing
// rules, such as "crit", refuse the token. go-jose's JSON decoder rejects
// repeated member names at any depth and matches names case-sensitively, so
// it reads the header exactly as the signature library does. It returns the
// key ID, or the refusal reason.
func checkHeader(segment string) (string, string) {
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return "", "malformed_credential"
	}
	var header map[string]any
	if err := josejson.Unmarshal(data, &header); err != nil || header == nil {
		return "", "malformed_credential"
	}
	if alg, _ := header["alg"].(string); alg != string(jose.RS256) {
		return "", "unsupported_header"
	}
	kid, ok := header["kid"].(string)
	if !ok || !isTrustValue(kid) {
		return "", "unsupported_header"
	}
	for name, value := range header {
		switch name {
		case "alg", "kid":
		case "typ":
			if typ, ok := value.(string); !ok || !strings.EqualFold(typ, "JWT") {
				return "", "unsupported_header"
			}
		default:
			return "", "unsupported_header"
		}
	}
	return kid, ""
}

// checkClaims applies the claim profile to a verified payload. A repeated
// member name anywhere refuses the token. Roles are assertions by the verified
// issuer, never policy permissions.
func (v *verifier) checkClaims(payload []byte, now time.Time) (verifiedToken, string) {
	var claims map[string]any
	if err := josejson.Unmarshal(payload, &claims); err != nil || claims == nil {
		return verifiedToken{}, "invalid_claims"
	}

	subject, ok := claims["sub"].(string)
	if !ok || !policy.IsSubjectID(subject) {
		return verifiedToken{}, "invalid_claims"
	}
	expires, ok := numericDate(claims["exp"])
	if !ok {
		return verifiedToken{}, "invalid_claims"
	}
	var notBefore time.Time
	if value, present := claims["nbf"]; present {
		if notBefore, ok = numericDate(value); !ok {
			return verifiedToken{}, "invalid_claims"
		}
	}

	issuer, ok := claims["iss"].(string)
	if !ok {
		return verifiedToken{}, "invalid_claims"
	}
	if issuer != v.issuer {
		return verifiedToken{}, "wrong_issuer"
	}
	if reason := v.checkAudience(claims["aud"]); reason != "" {
		return verifiedToken{}, reason
	}
	roles := []string{}
	if value, present := claims["roles"]; present {
		items, ok := value.([]any)
		if !ok || len(items) > policy.MaxRoles {
			return verifiedToken{}, "invalid_claims"
		}
		for _, item := range items {
			role, ok := item.(string)
			if !ok || policy.CheckID(role, "roles") != nil {
				return verifiedToken{}, "invalid_claims"
			}
			roles = append(roles, role)
		}
		slices.Sort(roles)
		roles = slices.Compact(roles)
	}

	// RFC 7519: the token is valid before "exp" and from "nbf" onwards.
	if !now.Before(expires) {
		return verifiedToken{}, "expired"
	}
	if now.Before(notBefore) {
		return verifiedToken{}, "not_yet_valid"
	}
	return verifiedToken{subject: subject, roles: roles}, ""
}

// checkAudience accepts a string or a non-empty array of strings that names
// this gateway. RFC 7519 requires rejecting a token whose audience does not
// include the recipient.
func (v *verifier) checkAudience(value any) string {
	switch audience := value.(type) {
	case string:
		if audience != v.audience {
			return "wrong_audience"
		}
		return ""
	case []any:
		named := false
		for _, item := range audience {
			name, ok := item.(string)
			if !ok {
				return "invalid_claims"
			}
			named = named || name == v.audience
		}
		if !named {
			return "wrong_audience"
		}
		return ""
	default:
		return "invalid_claims"
	}
}

// numericDate converts an RFC 7519 NumericDate, a JSON number of seconds since
// the epoch, possibly fractional, to a time with nanosecond precision. Any
// other JSON type, including null, is refused rather than converted.
func numericDate(value any) (time.Time, bool) {
	seconds, ok := value.(float64)
	if !ok || seconds < 0 || seconds > maxNumericDate {
		return time.Time{}, false
	}
	whole := math.Floor(seconds)
	nanos := math.Round((seconds - whole) * 1e9)
	return time.Unix(int64(whole), int64(nanos)), true
}
