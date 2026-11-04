// Trust material validation.

package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
