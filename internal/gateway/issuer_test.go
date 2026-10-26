package gateway

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"maps"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// The test process acts as the external token issuer. Its signing keys are
// generated in memory for each run and never written anywhere: the gateway
// receives only the public key, through a file in the test environment.
const (
	testIssuerName = "https://issuer.test"
	testAudience   = "gateway.test"
)

type testIssuer struct {
	key *rsa.PrivateKey
}

func newTestIssuer(bits int) *testIssuer {
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		panic(err)
	}
	return &testIssuer{key: key}
}

// publicPEM is the verification key in the form the gateway accepts.
func (i *testIssuer) publicPEM() []byte {
	der, err := x509.MarshalPKIXPublicKey(&i.key.PublicKey)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// changed returns a copy of base with changes applied; a nil value removes
// that member.
func changed(base, changes map[string]any) map[string]any {
	result := maps.Clone(base)
	for name, value := range changes {
		if value == nil {
			delete(result, name)
			continue
		}
		result[name] = value
	}
	return result
}

// validClaims is a claim set the gateway accepts for subject, valid for ten
// minutes from now.
func validClaims(subject string) map[string]any {
	now := time.Now()
	return map[string]any{
		"sub": subject,
		"iss": testIssuerName,
		"aud": testAudience,
		"iat": now.Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
	}
}

// token signs claims with go-jose's own signer, as an ordinary issuer would.
func (i *testIssuer) token(t testing.TB, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: i.key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

// tokenFor is a valid token for subject.
func (i *testIssuer) tokenFor(t testing.TB, subject string) string {
	t.Helper()
	return i.token(t, validClaims(subject))
}

// signRaw signs a hand-written header and payload with RS256, so tests can
// produce tokens that no well-behaved signer would: repeated members, wrong
// claim types, unusual headers.
func (i *testIssuer) signRaw(header, payload string) string {
	input := encodeSegment(header) + "." + encodeSegment(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// hmacToken signs with HS256, using secret as the HMAC key. With the
// gateway's public key bytes as the secret this is the classic RSA/HMAC
// confusion attack.
func hmacToken(secret []byte, payload string) string {
	input := encodeSegment(`{"alg":"HS256","typ":"JWT"}`) + "." + encodeSegment(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func encodeSegment(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func claimsJSON(claims map[string]any) string {
	data, _ := json.Marshal(claims)
	return string(data)
}
