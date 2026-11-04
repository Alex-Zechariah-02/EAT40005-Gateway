// Trust material at startup.

package gateway

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gateway does not start without valid trust material, and changing a
// key file alone changes nothing: keys are replaced by a reload or restart.
func TestTrustMaterialAtStartup(t *testing.T) {
	other := newTestIssuer(2048)
	privateKey := filepath.Join(t.TempDir(), "private.pem")
	der, _ := x509.MarshalPKCS8PrivateKey(other.key)
	os.WriteFile(privateKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)

	trust := func(e *env, changes map[string]any) map[string]any {
		config := map[string]any{"issuer": testIssuerName, "audience": testAudience, "keys": []VerificationKey{{Kid: "test-a", PublicKeyFile: e.publicKey}}}
		return changed(config, changes)
	}
	// cause is the part of the startup error that shows why it was refused.
	// The gateway logs the error as JSON, so quotes inside it appear escaped.
	cases := []struct {
		name    string
		changes func(e *env) any
		cause   string
	}{
		{"no trust configuration", func(e *env) any { return map[string]any{} }, "gateway.jwt.issuer is required"},
		{"missing key file", func(e *env) any {
			return trust(e, map[string]any{"keys": []VerificationKey{{Kid: "test-a", PublicKeyFile: filepath.Join(e.dir, "absent.pem")}}})
		}, "jwt: opening public key"},
		{"private key given as the key", func(e *env) any {
			return trust(e, map[string]any{"keys": []VerificationKey{{Kid: "test-a", PublicKeyFile: privateKey}}})
		}, "exactly one PEM PUBLIC KEY block"},
		{"no issuer", func(e *env) any { return trust(e, map[string]any{"issuer": nil}) }, "gateway.jwt.issuer is required"},
		{"no audience", func(e *env) any { return trust(e, map[string]any{"audience": nil}) }, "gateway.jwt.audience is required"},
		{"several audiences", func(e *env) any { return trust(e, map[string]any{"audience": []string{testAudience, "other"}}) }, "gateway.jwt.audience: a JSON array is not a valid value"},
		{"remote key set", func(e *env) any { return trust(e, map[string]any{"jwks_url": "http://127.0.0.1:1/jwks"}) }, `unknown field \"jwks_url\"`},
		{"second issuer", func(e *env) any { return trust(e, map[string]any{"issuers": []string{"https://other.test"}}) }, `unknown field \"issuers\"`},
		{"clock leeway", func(e *env) any { return trust(e, map[string]any{"leeway": "30s"}) }, `unknown field \"leeway\"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.writeConfig(configOptions{jwt: tc.changes(e)})
			e.launch(binaries.plain, []string{"run", "--config", e.config})
			if code := e.waitExit(); code == 0 {
				t.Fatalf("gateway ran without valid trust material\n%s", e.output)
			}
			if !strings.Contains(e.output.String(), tc.cause) {
				t.Fatalf("refused for another reason, want %q:\n%s", tc.cause, e.output)
			}
			if dialable(e.dataAddr) || dialable(e.admin) {
				t.Fatal("a listener is open after the refused start")
			}
			if strings.Contains(e.output.String(), "PRIVATE KEY") || strings.Contains(e.output.String(), "MII") {
				t.Fatalf("startup output discloses key material:\n%s", e.output)
			}
		})
	}

	t.Run("key replaced by restart", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.protectedPolicy(), seedOptions{})
		e.start(binaries.plain)
		oldToken, newToken := issuer.tokenFor(t, "alice"), other.tokenFor(t, "alice")
		before := e.status().JWTKey
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+oldToken); code != http.StatusOK {
			t.Fatalf("token from the configured key: %d", code)
		}
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+newToken); code != http.StatusUnauthorized {
			t.Fatalf("token from the replacement key before restart: %d", code)
		}

		// Replacing the file changes nothing until the gateway restarts.
		if err := os.WriteFile(e.publicKey, other.publicPEM(), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+newToken); code != http.StatusUnauthorized || e.status().JWTKey != before {
			t.Fatalf("replacement key used without a restart: %d", code)
		}

		e.start(binaries.plain)
		if e.status().JWTKey == before || len(e.status().JWTKey) != 64 {
			t.Fatalf("fingerprint %q after restart, was %q", e.status().JWTKey, before)
		}
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+oldToken); code != http.StatusUnauthorized {
			t.Fatalf("token from the old key after restart: %d", code)
		}
		if code, _ := e.get("/a/x", "Authorization", "Bearer "+newToken); code != http.StatusOK {
			t.Fatalf("token from the new key after restart: %d", code)
		}
		e.assertReceipts(2, 0, 0)
	})
}
