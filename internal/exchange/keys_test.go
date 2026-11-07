// Tests of trusted public keys in the ledger: PEM RSA public keys only, one per key identifier.

package exchange

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// TestPublicKeys accepts RSA public keys and refuses private keys, other
// key types, extra text and a count that differs from the identifiers.
func TestPublicKeys(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public := func(key any) string {
		der, err := x509.MarshalPKIXPublicKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}))
	good := public(&rsaKey.PublicKey)
	cases := map[string]struct {
		keys []string
		ok   bool
	}{
		"rsa public key": {[]string{good}, true},
		"no keys":        {nil, true},
		"private key":    {[]string{private}, false},
		"ec key":         {[]string{public(&ecKey.PublicKey)}, false},
		"text after":     {[]string{good + "more"}, false},
		"two blocks":     {[]string{good + good}, false},
		"count differs":  {[]string{good, good}, false},
		"not pem":        {[]string{"MIIB"}, false},
		"oversized":      {[]string{good + strings.Repeat(" ", maxText)}, false},
	}
	for name, c := range cases {
		ledger := New(Source{Format: "kong-deck", FormatVersion: "3.0", SHA256: strings.Repeat("a", 64)})
		ledger.Trust.Issuers = []Issuer{{Issuer: "https://login.example", PublicKeyIDs: []string{"consumer:a:0"}, PublicKeys: c.keys}}
		if err := ledger.Check(); (err == nil) != c.ok {
			t.Errorf("%s: Check = %v, want ok %v", name, err, c.ok)
		}
	}
}
