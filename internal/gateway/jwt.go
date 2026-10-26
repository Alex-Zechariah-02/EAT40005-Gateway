package gateway

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	josejson "github.com/go-jose/go-jose/v4/json"
)

const (
	maxTokenBytes   = 4096
	maxKeyFileBytes = 64 << 10
	maxTrustValue   = 256
	minRSABits      = 2048
	maxRSABits      = 8192
	// maxNumericDate is 9999-12-31T23:59:59Z, so every accepted time claim
	// converts to a time.Time without overflow.
	maxNumericDate = 253402300799
)

// TrustConfig names the one trusted token issuer, this gateway's identity as
// a token audience, and the file holding the issuer's RSA public key.
type TrustConfig struct {
	Issuer        string `json:"issuer"`
	Audience      string `json:"audience"`
	PublicKeyFile string `json:"public_key_file"`
}

// verifier checks bearer tokens against the single configured key under the
// controlled comparison profile: RS256 only, "sub" and "exp" required, "nbf"
// checked when present, no clock leeway, and "iss" and "aud" checked only
// when present. Accepting tokens without "iss" or "aud" is safe only while
// the key belongs to one dedicated issuer whose tokens are meant for this
// gateway alone; it gives no protection where an issuer or key serves several
// recipients.
type verifier struct {
	issuer      string
	audience    string
	key         *rsa.PublicKey
	fingerprint string // SHA-256 of the key's SubjectPublicKeyInfo
}

// loadVerifier reads and validates the trust configuration. The key file must
// hold exactly one PEM "PUBLIC KEY" block with an RSA key, so a private key or
// any other material is refused.
func loadVerifier(c TrustConfig) (*verifier, error) {
	if !isTrustValue(c.Issuer) || !isTrustValue(c.Audience) {
		return nil, fmt.Errorf("issuer and audience must each be 1 to %d printable ASCII characters", maxTrustValue)
	}
	file, err := os.Open(c.PublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("opening public key: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading public key: %w", err)
	}
	if len(data) > maxKeyFileBytes {
		return nil, errors.New("public key file is too large")
	}

	// pem.Decode skips text before the first block, so leading data is
	// checked separately.
	block, rest := pem.Decode(data)
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN ")) ||
		block == nil ||
		block.Type != "PUBLIC KEY" ||
		len(block.Headers) != 0 ||
		len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("public key file must hold exactly one PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing public key: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("public key is not an RSA key")
	}
	if bits := key.N.BitLen(); bits < minRSABits || bits > maxRSABits {
		return nil, fmt.Errorf("RSA key has %d bits, want %d to %d", bits, minRSABits, maxRSABits)
	}

	sum := sha256.Sum256(block.Bytes)
	return &verifier{
		issuer:      c.Issuer,
		audience:    c.Audience,
		key:         key,
		fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

func isTrustValue(value string) bool {
	if value == "" || len(value) > maxTrustValue {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
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
func (v *verifier) verify(token string, now time.Time) (string, string) {
	header, _, _ := strings.Cut(token, ".")
	if reason := checkHeader(header); reason != "" {
		return "", reason
	}
	signed, err := jose.ParseSignedCompact(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return "", "malformed_credential"
	}
	payload, err := signed.Verify(v.key)
	if err != nil {
		return "", "invalid_signature"
	}
	return v.checkClaims(payload, now)
}

// checkHeader allows only the members "alg" (exactly RS256), "typ" (JWT) and
// "kid". The key ID is never used to choose a key, and members that name or
// carry a key, such as "jku", "x5u", "x5c" or "jwk", or that add processing
// rules, such as "crit", refuse the token. go-jose's JSON decoder rejects
// repeated member names at any depth and matches names case-sensitively, so
// it reads the header exactly as the signature library does.
func checkHeader(segment string) string {
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return "malformed_credential"
	}
	var header map[string]any
	if err := josejson.Unmarshal(data, &header); err != nil || header == nil {
		return "malformed_credential"
	}
	if alg, _ := header["alg"].(string); alg != string(jose.RS256) {
		return "unsupported_header"
	}
	for name, value := range header {
		switch name {
		case "alg":
		case "typ":
			if typ, ok := value.(string); !ok || !strings.EqualFold(typ, "JWT") {
				return "unsupported_header"
			}
		case "kid":
			if _, ok := value.(string); !ok {
				return "unsupported_header"
			}
		default:
			return "unsupported_header"
		}
	}
	return ""
}

// checkClaims applies the claim profile to a verified payload. A repeated
// member name anywhere refuses the token. Claims other than sub, exp, nbf, iss
// and aud, including any role claims, are ignored: roles come only from the
// trusted membership in the policy.
func (v *verifier) checkClaims(payload []byte, now time.Time) (string, string) {
	var claims map[string]any
	if err := josejson.Unmarshal(payload, &claims); err != nil || claims == nil {
		return "", "invalid_claims"
	}

	subject, ok := claims["sub"].(string)
	if !ok || !isSubjectID(subject) {
		return "", "invalid_claims"
	}
	expires, ok := numericDate(claims["exp"])
	if !ok {
		return "", "invalid_claims"
	}
	var notBefore time.Time
	if value, present := claims["nbf"]; present {
		if notBefore, ok = numericDate(value); !ok {
			return "", "invalid_claims"
		}
	}

	if value, present := claims["iss"]; present {
		issuer, ok := value.(string)
		if !ok {
			return "", "invalid_claims"
		}
		if issuer != v.issuer {
			return "", "wrong_issuer"
		}
	}
	if value, present := claims["aud"]; present {
		if reason := v.checkAudience(value); reason != "" {
			return "", reason
		}
	}

	// RFC 7519: the token is valid before "exp" and from "nbf" onwards.
	if !now.Before(expires) {
		return "", "expired"
	}
	if now.Before(notBefore) {
		return "", "not_yet_valid"
	}
	return subject, ""
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
