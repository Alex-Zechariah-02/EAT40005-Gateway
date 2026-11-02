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
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-jose/go-jose/v4"
	josejson "github.com/go-jose/go-jose/v4/json"
)

const (
	maxTokenBytes       = 4096
	maxKeyFileBytes     = 64 << 10
	maxTrustValue       = 256
	minRSABits          = 2048
	maxRSABits          = 8192
	maxVerificationKeys = 64
	// maxNumericDate is 9999-12-31T23:59:59Z, so every accepted time claim
	// converts to a time.Time without overflow.
	maxNumericDate = 253402300799
)

// TrustConfig names the trusted issuer, audience and locally configured public
// keys. Token headers can select only a key already in this set.
type TrustConfig struct {
	Issuer   string            `json:"issuer"`
	Audience string            `json:"audience"`
	Keys     []VerificationKey `json:"keys"`
}

type VerificationKey struct {
	Kid           string `json:"kid"`
	PublicKeyFile string `json:"public_key_file"`
}

type keyInfo struct {
	Kid         string    `json:"kid"`
	Fingerprint string    `json:"fingerprint"`
	LoadedAt    time.Time `json:"loaded_at"`
}

// verifiedToken is constructed only after signature and claim checks succeed.
// Its asserted roles still need filtering through the captured policy.
type verifiedToken struct {
	subject string
	roles   []string
}

// A verifier is an immutable whole key set. Replacing one never mutates keys
// that a request has already captured.
type verifier struct {
	issuer      string
	audience    string
	keys        map[string]*rsa.PublicKey
	info        []keyInfo
	fingerprint string // SHA-256 of the complete named key set
	// uses counts verified tokens per key since the gateway started. The map
	// is fixed when the verifier is built; a reload carries a kept key's
	// counter over (adoptUses), so only the counters' atomics change.
	uses map[string]*keyUse
}

// keyUse counts the tokens one key verified and when it last did. It is read
// by the keys report and written by verify with atomics only, so counting
// adds no lock to a request.
type keyUse struct {
	verified atomic.Uint64
	lastNano atomic.Int64
}

// adoptUses gives v the counters of the keys it shares with previous, before
// v is published, so a reload keeps their counts.
func (v *verifier) adoptUses(previous *verifier) {
	for kid := range v.uses {
		if use := previous.uses[kid]; use != nil {
			v.uses[kid] = use
		}
	}
}

// loadVerifier reads and validates the trust configuration. The key file must
// hold exactly one PEM "PUBLIC KEY" block with an RSA key, so a private key or
// any other material is refused.
func loadVerifier(c TrustConfig) (*verifier, error) {
	if !isTrustValue(c.Issuer) || !isTrustValue(c.Audience) {
		return nil, fmt.Errorf("issuer and audience must each be 1 to %d printable ASCII characters", maxTrustValue)
	}
	if len(c.Keys) == 0 || len(c.Keys) > maxVerificationKeys {
		return nil, fmt.Errorf("keys must contain 1 to %d named public keys", maxVerificationKeys)
	}
	v := &verifier{issuer: c.Issuer, audience: c.Audience, keys: map[string]*rsa.PublicKey{}, info: []keyInfo{}, uses: map[string]*keyUse{}}
	loaded := time.Now().UTC()
	for _, entry := range c.Keys {
		if checkID(entry.Kid, "kid") != nil || v.keys[entry.Kid] != nil {
			return nil, errors.New("key IDs must be valid and distinct")
		}
		key, fingerprint, err := readVerificationKey(entry.PublicKeyFile)
		if err != nil {
			return nil, err
		}
		v.keys[entry.Kid] = key
		v.uses[entry.Kid] = &keyUse{}
		v.info = append(v.info, keyInfo{Kid: entry.Kid, Fingerprint: "sha256:" + fingerprint, LoadedAt: loaded})
	}
	slices.SortFunc(v.info, func(a, b keyInfo) int { return strings.Compare(a.Kid, b.Kid) })
	hash := sha256.New()
	for _, info := range v.info {
		fmt.Fprintf(hash, "%s:%s\n", info.Kid, info.Fingerprint)
	}
	v.fingerprint = hex.EncodeToString(hash.Sum(nil))
	return v, nil
}

func readVerificationKey(path string) (*rsa.PublicKey, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("opening public key: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading public key: %w", err)
	}
	if len(data) > maxKeyFileBytes {
		return nil, "", errors.New("public key file is too large")
	}

	// pem.Decode skips text before the first block, so leading data is
	// checked separately.
	block, rest := pem.Decode(data)
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN ")) ||
		block == nil ||
		block.Type != "PUBLIC KEY" ||
		len(block.Headers) != 0 ||
		len(bytes.TrimSpace(rest)) != 0 {
		return nil, "", errors.New("public key file must hold exactly one PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parsing public key: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, "", errors.New("public key is not an RSA key")
	}
	if bits := key.N.BitLen(); bits < minRSABits || bits > maxRSABits {
		return nil, "", fmt.Errorf("RSA key has %d bits, want %d to %d", bits, minRSABits, maxRSABits)
	}

	sum := sha256.Sum256(block.Bytes)
	return key, hex.EncodeToString(sum[:]), nil
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
	if !ok || !isSubjectID(subject) {
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
		if !ok || len(items) > maxRoles {
			return verifiedToken{}, "invalid_claims"
		}
		for _, item := range items {
			role, ok := item.(string)
			if !ok || checkID(role, "roles") != nil {
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
