package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exercise token verification directly, at a fixed time, with
// genuine RS256 signatures from the test issuer. The same verifier runs in
// the gateway; the authentication integration tests cover that path.

// fixedNow is the validation time of every vector below.
var fixedNow = time.Unix(1_800_000_000, 0)

func testTrust(issuer, audience, file string) TrustConfig {
	return TrustConfig{Issuer: issuer, Audience: audience, Keys: []VerificationKey{{Kid: "test-a", PublicKeyFile: file}}}
}

func testVerifier(t *testing.T) *verifier {
	t.Helper()
	path := filepath.Join(t.TempDir(), "issuer.pem")
	if err := os.WriteFile(path, issuer.publicPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := loadVerifier(testTrust(testIssuerName, testAudience, path))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// claimsAt is a valid claim set at fixedNow, with the given changes applied.
// A nil value removes the claim.
func claimsAt(changes map[string]any) map[string]any {
	claims := map[string]any{
		"sub": "staff-01",
		"iss": testIssuerName,
		"aud": testAudience,
		"exp": fixedNow.Unix() + 600,
	}
	return changed(claims, changes)
}

// authenticate runs the whole authentication stage on Authorization fields.
func authenticate(v *verifier, fields []string, now time.Time) (string, string) {
	token, reason := bearerToken(fields)
	if reason != "" {
		return "", reason
	}
	verified, reason := v.verify(token, now)
	return verified.subject, reason
}

func TestBearerCredentialExtraction(t *testing.T) {
	v := testVerifier(t)
	token := issuer.token(t, claimsAt(nil))
	cases := []struct {
		name   string
		fields []string
		reason string
	}{
		{"valid", []string{"Bearer " + token}, ""},
		{"scheme in lower case", []string{"bearer " + token}, ""},
		{"scheme in upper case", []string{"BEARER " + token}, ""},
		{"no Authorization field", nil, "missing_credential"},
		{"empty field", []string{""}, "malformed_credential"},
		{"scheme only", []string{"Bearer"}, "malformed_credential"},
		{"scheme and space", []string{"Bearer "}, "malformed_credential"},
		{"two spaces", []string{"Bearer  " + token}, "malformed_credential"},
		{"trailing text", []string{"Bearer " + token + " extra"}, "malformed_credential"},
		{"other scheme", []string{"Basic " + token}, "malformed_credential"},
		{"token scheme", []string{"Token " + token}, "malformed_credential"},
		{"two fields", []string{"Bearer " + token, "Bearer " + token}, "malformed_credential"},
		{"two credentials in one field", []string{"Bearer " + token + ",Bearer " + token}, "malformed_credential"},
		{"padding characters", []string{"Bearer " + token + "=="}, "malformed_credential"},
		{"standard base64 characters", []string{"Bearer " + strings.Replace(token, "-", "+", 1) + "/"}, "malformed_credential"},
		{"two segments", []string{"Bearer " + token[:strings.LastIndex(token, ".")]}, "malformed_credential"},
		{"four segments", []string{"Bearer " + token + ".x"}, "malformed_credential"},
		{"empty signature", []string{"Bearer " + token[:strings.LastIndex(token, ".")+1]}, "malformed_credential"},
		{"oversize", []string{"Bearer " + token + strings.Repeat("A", maxTokenBytes)}, "malformed_credential"},
		{"control character", []string{"Bearer " + token + "\x00"}, "malformed_credential"},
	}
	for _, tc := range cases {
		subject, reason := authenticate(v, tc.fields, fixedNow)
		if reason != tc.reason || (reason == "" && subject != "staff-01") {
			t.Errorf("%s: subject %q reason %q, want reason %q", tc.name, subject, reason, tc.reason)
		}
	}
}

func TestSignatureAndAlgorithm(t *testing.T) {
	v := testVerifier(t)
	payload := claimsJSON(claimsAt(nil))
	valid := issuer.signRaw(`{"alg":"RS256","typ":"JWT","kid":"test-a"}`, payload)
	segments := strings.Split(valid, ".")

	// Flip one bit of the signature, and swap in a payload signed for
	// nobody, keeping the original signature.
	signature, _ := base64.RawURLEncoding.DecodeString(segments[2])
	signature[10] ^= 0x01
	badSignature := segments[0] + "." + segments[1] + "." + base64.RawURLEncoding.EncodeToString(signature)
	elevated := claimsJSON(claimsAt(map[string]any{"sub": "mgr-01"}))
	swappedPayload := segments[0] + "." + encodeSegment(elevated) + "." + segments[2]

	other := newTestIssuer(2048)

	cases := []struct {
		name   string
		token  string
		reason string
	}{
		{"genuine RS256 token from a standard signer", issuer.token(t, claimsAt(nil)), ""},
		{"genuine RS256 token signed by hand", valid, ""},
		{"signature bit flipped", badSignature, "invalid_signature"},
		{"payload replaced after signing", swappedPayload, "invalid_signature"},
		{"untrusted key named by a trusted kid", other.signRaw(`{"alg":"RS256","kid":"test-a"}`, payload), "invalid_signature"},
		{"alg none with a fake signature", encodeSegment(`{"alg":"none"}`) + "." + encodeSegment(payload) + ".AAAA", "unsupported_header"},
		{"alg None", encodeSegment(`{"alg":"None"}`) + "." + encodeSegment(payload) + ".AAAA", "unsupported_header"},
		{"HS256 keyed with the public key", hmacToken(issuer.publicPEM(), payload), "unsupported_header"},
		{"HS256 keyed with the key's DER", hmacToken(publicDER(t), payload), "unsupported_header"},
		{"RS256 header over an HMAC signature", rs256HeaderOverHMAC(payload), "invalid_signature"},
		{"PS256", other.signRaw(`{"alg":"PS256"}`, payload), "unsupported_header"},
		{"RS512", issuer.signRaw(`{"alg":"RS512"}`, payload), "unsupported_header"},
		{"ES256", issuer.signRaw(`{"alg":"ES256"}`, payload), "unsupported_header"},
		{"alg in lower case", issuer.signRaw(`{"alg":"rs256"}`, payload), "unsupported_header"},
		{"alg missing", issuer.signRaw(`{"typ":"JWT"}`, payload), "unsupported_header"},
		{"alg not a string", issuer.signRaw(`{"alg":["RS256"]}`, payload), "unsupported_header"},
		{"header not JSON", issuer.signRaw(`alg=RS256`, payload), "malformed_credential"},
		{"header an array", issuer.signRaw(`["RS256"]`, payload), "malformed_credential"},
		{"header null", issuer.signRaw(`null`, payload), "malformed_credential"},
		{"corrupt header encoding", "e" + valid, "malformed_credential"},
		{"corrupt signature encoding", valid + "A", "invalid_signature"},
	}
	for _, tc := range cases {
		subject, reason := authenticate(v, []string{"Bearer " + tc.token}, fixedNow)
		if reason != tc.reason || (reason == "" && subject != "staff-01") {
			t.Errorf("%s: subject %q reason %q, want reason %q", tc.name, subject, reason, tc.reason)
		}
	}
}

func publicDER(t *testing.T) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&issuer.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// rs256HeaderOverHMAC claims RS256 but carries an HMAC signature keyed with
// the public key: the verifier must use RSA and fail.
func rs256HeaderOverHMAC(payload string) string {
	forged := hmacToken(issuer.publicPEM(), payload)
	segments := strings.Split(forged, ".")
	return encodeSegment(`{"alg":"RS256","kid":"test-a"}`) + "." + segments[1] + "." + segments[2]
}

func TestHeaderMembers(t *testing.T) {
	v := testVerifier(t)
	payload := claimsJSON(claimsAt(nil))

	// A key file that a kid could name: it must never be read.
	other := newTestIssuer(2048)
	otherKey := filepath.Join(t.TempDir(), "other.pem")
	os.WriteFile(otherKey, other.publicPEM(), 0o600)

	cases := []struct {
		name   string
		header string
		signer *testIssuer
		reason string
	}{
		{"typ JWT", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, issuer, ""},
		{"typ in lower case", `{"alg":"RS256","typ":"jwt","kid":"test-a"}`, issuer, ""},
		{"no typ", `{"alg":"RS256","kid":"test-a"}`, issuer, ""},
		{"unknown kid is refused", `{"alg":"RS256","kid":"key-1"}`, issuer, "unknown_kid"},
		{"kid naming a file", `{"alg":"RS256","kid":"` + filepath.ToSlash(otherKey) + `"}`, issuer, "unknown_kid"},
		{"kid naming a file, signed with that file's key", `{"alg":"RS256","kid":"` + filepath.ToSlash(otherKey) + `"}`, other, "unknown_kid"},
		{"kid traversal, signed with another key", `{"alg":"RS256","kid":"../../other.pem"}`, other, "unknown_kid"},
		{"kid not a string", `{"alg":"RS256","kid":7}`, issuer, "unsupported_header"},
		{"other typ", `{"alg":"RS256","kid":"test-a","typ":"at+jwt"}`, issuer, "unsupported_header"},
		{"typ not a string", `{"alg":"RS256","kid":"test-a","typ":null}`, issuer, "unsupported_header"},
		{"jku", `{"alg":"RS256","kid":"test-a","jku":"http://127.0.0.1:1/jwks.json"}`, issuer, "unsupported_header"},
		{"x5u", `{"alg":"RS256","kid":"test-a","x5u":"http://127.0.0.1:1/cert.pem"}`, issuer, "unsupported_header"},
		{"x5c", `{"alg":"RS256","kid":"test-a","x5c":["MIIB"]}`, issuer, "unsupported_header"},
		{"x5t#S256", `{"alg":"RS256","kid":"test-a","x5t#S256":"abc"}`, issuer, "unsupported_header"},
		{"embedded jwk", `{"alg":"RS256","kid":"test-a","jwk":{"kty":"RSA","n":"AQAB","e":"AQAB"}}`, issuer, "unsupported_header"},
		{"crit", `{"alg":"RS256","kid":"test-a","crit":["exp"],"exp":1}`, issuer, "unsupported_header"},
		{"unencoded payload", `{"alg":"RS256","kid":"test-a","b64":false,"crit":["b64"]}`, issuer, "unsupported_header"},
		{"nested token", `{"alg":"RS256","kid":"test-a","cty":"JWT"}`, issuer, "unsupported_header"},
		{"case variant of alg", `{"alg":"RS256","kid":"test-a","ALG":"none"}`, issuer, "unsupported_header"},
		{"repeated alg", `{"alg":"none","alg":"RS256"}`, issuer, "malformed_credential"},
		{"repeated alg, same value", `{"alg":"RS256","alg":"RS256"}`, issuer, "malformed_credential"},
	}
	for _, tc := range cases {
		subject, reason := v.verify(tc.signer.signRaw(tc.header, payload), fixedNow)
		if reason != tc.reason || (reason == "" && subject.subject != "staff-01") {
			t.Errorf("%s: subject %q reason %q, want reason %q", tc.name, subject, reason, tc.reason)
		}
	}
}

func TestClaimProfile(t *testing.T) {
	v := testVerifier(t)
	now := fixedNow.Unix()
	cases := []struct {
		name   string
		claims map[string]any
		reason string
	}{
		{"all claims present", claimsAt(nil), ""},
		{"no iss", claimsAt(map[string]any{"iss": nil}), "invalid_claims"},
		{"no aud", claimsAt(map[string]any{"aud": nil}), "invalid_claims"},
		{"neither iss nor aud", claimsAt(map[string]any{"iss": nil, "aud": nil}), "invalid_claims"},
		{"aud array naming this gateway", claimsAt(map[string]any{"aud": []string{"other", testAudience}}), ""},
		{"roles are asserted and other permission claims are ignored", claimsAt(map[string]any{"roles": []string{"ADMIN"}, "role": "MANAGER", "scope": "admin"}), ""},
		{"iat, jti and unknown claims are ignored", claimsAt(map[string]any{"iat": "yesterday", "jti": 5, "x": []any{}}), ""},

		{"wrong iss", claimsAt(map[string]any{"iss": "https://other.test"}), "wrong_issuer"},
		{"iss with a trailing slash", claimsAt(map[string]any{"iss": testIssuerName + "/"}), "wrong_issuer"},
		{"iss in another case", claimsAt(map[string]any{"iss": strings.ToUpper(testIssuerName)}), "wrong_issuer"},
		{"empty iss", claimsAt(map[string]any{"iss": ""}), "wrong_issuer"},
		{"iss not a string", claimsAt(map[string]any{"iss": 1}), "invalid_claims"},
		{"iss array", claimsAt(map[string]any{"iss": []string{testIssuerName}}), "invalid_claims"},

		{"aud for another recipient", claimsAt(map[string]any{"aud": "other-gateway"}), "wrong_audience"},
		{"aud array without this gateway", claimsAt(map[string]any{"aud": []string{"a", "b"}}), "wrong_audience"},
		{"empty aud array", claimsAt(map[string]any{"aud": []string{}}), "wrong_audience"},
		{"aud not a string", claimsAt(map[string]any{"aud": 1}), "invalid_claims"},
		{"aud array with a number", claimsAt(map[string]any{"aud": []any{testAudience, 1}}), "invalid_claims"},
		{"aud array with null", claimsAt(map[string]any{"aud": []any{testAudience, nil}}), "invalid_claims"},
		{"aud object", claimsAt(map[string]any{"aud": map[string]any{"aud": testAudience}}), "invalid_claims"},

		{"no sub", claimsAt(map[string]any{"sub": nil}), "invalid_claims"},
		{"empty sub", claimsAt(map[string]any{"sub": ""}), "invalid_claims"},
		{"sub a number", claimsAt(map[string]any{"sub": 1001}), "invalid_claims"},
		{"sub an array", claimsAt(map[string]any{"sub": []string{"staff-01"}}), "invalid_claims"},
		{"sub with a space", claimsAt(map[string]any{"sub": "staff 01"}), "invalid_claims"},
		{"sub with a newline", claimsAt(map[string]any{"sub": "staff-01\n"}), "invalid_claims"},
		{"sub outside ASCII", claimsAt(map[string]any{"sub": "staff-ö1"}), "invalid_claims"},
		{"overlong sub", claimsAt(map[string]any{"sub": strings.Repeat("s", maxSubjectLength+1)}), "invalid_claims"},

		{"no exp", claimsAt(map[string]any{"exp": nil}), "invalid_claims"},
		{"exp a string", claimsAt(map[string]any{"exp": "1800000600"}), "invalid_claims"},
		{"exp a boolean", claimsAt(map[string]any{"exp": true}), "invalid_claims"},
		{"exp negative", claimsAt(map[string]any{"exp": -1}), "invalid_claims"},
		{"exp after year 9999", claimsAt(map[string]any{"exp": maxNumericDate + 1}), "invalid_claims"},
		{"nbf a string", claimsAt(map[string]any{"nbf": "0"}), "invalid_claims"},

		// Zero leeway: valid strictly before exp, and from nbf onwards.
		{"exp one second ago", claimsAt(map[string]any{"exp": now - 1}), "expired"},
		{"exp exactly now", claimsAt(map[string]any{"exp": now}), "expired"},
		{"exp one second ahead", claimsAt(map[string]any{"exp": now + 1}), ""},
		{"exp half a second ahead", claimsAt(map[string]any{"exp": float64(now) + 0.5}), ""},
		{"exp half a second ago", claimsAt(map[string]any{"exp": float64(now) - 0.5}), "expired"},
		{"nbf in the past", claimsAt(map[string]any{"nbf": now - 60}), ""},
		{"nbf exactly now", claimsAt(map[string]any{"nbf": now}), ""},
		{"nbf one second ahead", claimsAt(map[string]any{"nbf": now + 1}), "not_yet_valid"},
		{"nbf half a second ahead", claimsAt(map[string]any{"nbf": float64(now) + 0.5}), "not_yet_valid"},
		{"nbf after exp", claimsAt(map[string]any{"nbf": now + 700}), "not_yet_valid"},
		{"exp zero", claimsAt(map[string]any{"exp": 0}), "expired"},
	}
	for _, tc := range cases {
		subject, reason := v.verify(issuer.token(t, tc.claims), fixedNow)
		if reason != tc.reason {
			t.Errorf("%s: subject %q reason %q, want reason %q", tc.name, subject, reason, tc.reason)
		}
		if reason == "" && subject.subject != tc.claims["sub"] {
			t.Errorf("%s: subject %q, want %v", tc.name, subject, tc.claims["sub"])
		}
	}
}

// The validity window is exact to the nanosecond at a fractional boundary.
func TestFractionalTimeBoundary(t *testing.T) {
	v := testVerifier(t)
	expires := time.Unix(fixedNow.Unix(), 500_000_000)
	token := issuer.signRaw(`{"alg":"RS256","kid":"test-a"}`, `{"iss":"https://issuer.test","aud":"gateway.test","sub":"staff-01","exp":1800000000.5,"nbf":1800000000.25}`)
	checks := []struct {
		at     time.Time
		reason string
	}{
		{time.Unix(fixedNow.Unix(), 249_999_999), "not_yet_valid"},
		{time.Unix(fixedNow.Unix(), 250_000_000), ""},
		{expires.Add(-time.Nanosecond), ""},
		{expires, "expired"},
	}
	for _, c := range checks {
		if _, reason := v.verify(token, c.at); reason != c.reason {
			t.Errorf("at %s: reason %q, want %q", c.at.Format(time.RFC3339Nano), reason, c.reason)
		}
	}
}

// Ambiguous or malformed claim encodings are refused, never resolved by
// picking one interpretation.
func TestAmbiguousClaimEncodings(t *testing.T) {
	v := testVerifier(t)
	exp := "1800000600"
	deep := strings.Repeat("[", 900) + strings.Repeat("]", 900)
	cases := []struct {
		name    string
		payload string
		reason  string
	}{
		{"repeated sub", `{"sub":"cust-001","sub":"staff-01","exp":` + exp + `}`, "invalid_claims"},
		{"repeated exp", `{"sub":"staff-01","exp":1,"exp":` + exp + `}`, "invalid_claims"},
		{"repeated iss", `{"sub":"staff-01","exp":` + exp + `,"iss":"x","iss":"` + testIssuerName + `"}`, "invalid_claims"},
		{"repeated member inside an ignored claim", `{"sub":"staff-01","exp":` + exp + `,"x":{"a":1,"a":2}}`, "invalid_claims"},
		{"sub only in another case", `{"SUB":"staff-01","exp":` + exp + `}`, "invalid_claims"},
		{"case variant beside sub", `{"Sub":"mgr-01","sub":"staff-01","exp":` + exp + `}`, ""},
		{"exp only in another case", `{"sub":"staff-01","EXP":` + exp + `}`, "invalid_claims"},
		{"sub null", `{"sub":null,"exp":` + exp + `}`, "invalid_claims"},
		{"exp null", `{"sub":"staff-01","exp":null}`, "invalid_claims"},
		{"nbf null", `{"sub":"staff-01","exp":` + exp + `,"nbf":null}`, "invalid_claims"},
		{"iss null", `{"sub":"staff-01","exp":` + exp + `,"iss":null}`, "invalid_claims"},
		{"exp out of float range", `{"sub":"staff-01","exp":1e400}`, "invalid_claims"},
		{"exp with exponent", `{"sub":"staff-01","exp":1.8000006e9}`, ""},
		{"invalid UTF-8 in sub", "{\"sub\":\"staff-01\xff\",\"exp\":" + exp + "}", "invalid_claims"},
		{"escaped control character in sub", `{"sub":"staff-01\u0000","exp":` + exp + `}`, "invalid_claims"},
		{"payload an array", `[{"sub":"staff-01","exp":` + exp + `}]`, "invalid_claims"},
		{"payload null", `null`, "invalid_claims"},
		{"payload not JSON", `sub=staff-01`, "invalid_claims"},
		{"trailing data after claims", `{"sub":"staff-01","exp":` + exp + `}{}`, "invalid_claims"},
		{"deeply nested ignored claim", `{"sub":"staff-01","exp":` + exp + `,"x":` + deep + `}`, ""},
	}
	for _, tc := range cases {
		started := time.Now()
		// Add the now-mandatory trust claims without decoding away the exact
		// malformed or duplicate representation this vector exercises.
		payload := tc.payload
		if strings.HasPrefix(payload, "{") {
			if !strings.Contains(payload, `"iss":`) {
				payload = `{"iss":"https://issuer.test",` + payload[1:]
			}
			if !strings.Contains(payload, `"aud":`) {
				payload = `{"aud":"gateway.test",` + payload[1:]
			}
		}
		_, reason := v.verify(issuer.signRaw(`{"alg":"RS256","kid":"test-a"}`, payload), fixedNow)
		if reason != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.name, reason, tc.reason)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("%s: took %v", tc.name, elapsed)
		}
	}
}

// The gateway accepts only a single RSA public key of adequate size, and
// refuses to run with anything else, including the issuer's private key.
func TestTrustMaterialValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	smallDER, _ := x509.MarshalPKIXPublicKey(&small.PublicKey)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	privateDER, _ := x509.MarshalPKCS8PrivateKey(issuer.key)
	public := issuer.publicPEM()

	valid := write("valid.pem", public)
	cases := []struct {
		name   string
		config TrustConfig
	}{
		{"missing file", testTrust(testIssuerName, testAudience, filepath.Join(dir, "absent.pem"))},
		{"empty file", testTrust(testIssuerName, testAudience, write("empty.pem", nil))},
		{"not PEM", testTrust(testIssuerName, testAudience, write("garbage.pem", []byte("not a key")))},
		{"corrupted key", testTrust(testIssuerName, testAudience, write("corrupt.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("corrupt")})))},
		{"PKCS#8 private key", testTrust(testIssuerName, testAudience, write("private.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})))},
		{"PKCS#1 private key", testTrust(testIssuerName, testAudience, write("rsa-private.pem", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(issuer.key)})))},
		{"PKCS#1 public key block", testTrust(testIssuerName, testAudience, write("pkcs1.pem", pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&issuer.key.PublicKey)})))},
		{"EC key", testTrust(testIssuerName, testAudience, write("ec.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: ecDER})))},
		{"1024-bit RSA key", testTrust(testIssuerName, testAudience, write("small.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: smallDER})))},
		{"two keys in one PEM file", testTrust(testIssuerName, testAudience, write("two.pem", append(append([]byte{}, public...), newTestIssuer(2048).publicPEM()...)))},
		{"key followed by its private key", testTrust(testIssuerName, testAudience, write("pair.pem", append(append([]byte{}, public...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})...)))},
		{"text before the key", testTrust(testIssuerName, testAudience, write("leading.pem", append([]byte("comment\n"), public...)))},
		{"text after the key", testTrust(testIssuerName, testAudience, write("trailing.pem", append(append([]byte{}, public...), "comment\n"...)))},
		{"PEM headers", testTrust(testIssuerName, testAudience, write("headers.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: publicDER(t)})))},
		{"oversize file", testTrust(testIssuerName, testAudience, write("big.pem", append(append([]byte{}, public...), make([]byte, maxKeyFileBytes)...)))},
		{"no key file configured", testTrust(testIssuerName, testAudience, "")},
		{"no issuer", testTrust("", testAudience, valid)},
		{"no audience", testTrust(testIssuerName, "", valid)},
		{"issuer with a space", testTrust("issuer one", testAudience, valid)},
		{"overlong audience", testTrust(testIssuerName, strings.Repeat("a", maxTrustValue+1), valid)},
	}
	for _, tc := range cases {
		if v, err := loadVerifier(tc.config); err == nil {
			t.Errorf("%s: accepted (key %s)", tc.name, v.fingerprint)
		} else if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "MII") {
			t.Errorf("%s: error discloses key material: %v", tc.name, err)
		}
	}

	v, err := loadVerifier(testTrust(testIssuerName, testAudience, valid))
	if err != nil {
		t.Fatalf("valid trust configuration rejected: %v", err)
	}
	sum := sha256.Sum256(publicDER(t))
	if len(v.info) != 1 || v.info[0].Kid != "test-a" || v.info[0].Fingerprint != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("key metadata %+v, want kid and SHA-256 of the key's SubjectPublicKeyInfo", v.info)
	}
}
