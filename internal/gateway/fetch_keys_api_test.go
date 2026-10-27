package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFetchKeyReload(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	s := e.mustLogin(harnessAdmin, harnessPassword)
	code, body, _ := e.as(s, http.MethodGet, "/admin/keys", "")
	if code != http.StatusOK {
		t.Fatalf("keys list: %d %s", code, body)
	}
	var before struct {
		Items []keyInfo `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &before); err != nil || len(before.Items) != 1 || before.Items[0].Kid != "test-a" || !strings.HasPrefix(before.Items[0].Fingerprint, "sha256:") || before.Items[0].LoadedAt.IsZero() {
		t.Fatalf("key metadata: %s (%v)", body, err)
	}
	other := newTestIssuer(2048)
	keyFile := filepath.Join(e.dir, "next.pem")
	if err := os.WriteFile(keyFile, other.publicPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	config := e.nativeSettings()
	trust := testTrust(testIssuerName, testAudience, e.publicKey)
	trust.Keys = append(trust.Keys, VerificationKey{Kid: "test-b", PublicKeyFile: keyFile})
	section(config, "gateway")["jwt"] = trust
	e.writeNative(config)
	post := func(key string) (int, string) {
		code, body, _ := e.as(s, http.MethodPost, "/admin/keys/reload", "", "Idempotency-Key", key)
		return code, body
	}
	code, reloaded := post("keys-reload-accept-01")
	if code != http.StatusOK {
		t.Fatalf("valid reload: %d %s", code, reloaded)
	}
	oldToken := issuer.tokenFor(t, "alice")
	newToken := other.signRaw(`{"alg":"RS256","typ":"JWT","kid":"test-b"}`, claimsJSON(validClaims("alice")))
	for _, token := range []string{oldToken, newToken} {
		if code, body := e.get("/a/x", "Authorization", "Bearer "+token); code != http.StatusOK {
			t.Fatalf("rotated keys: %d %s", code, body)
		}
	}
	// A bad on-disk key cannot replace the validated whole set. Replaying the
	// accepted mutation must return its original list without re-reading disk.
	if err := os.WriteFile(keyFile, []byte("invalid key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, replayed := post("keys-reload-accept-01"); code != 200 || replayed != reloaded {
		t.Fatalf("reload replay changed its result: %d %s", code, replayed)
	}
	if code, body := post("keys-reload-invalid-01"); code != http.StatusUnprocessableEntity || !strings.Contains(body, `"keys_invalid"`) {
		t.Fatalf("invalid reload: %d %s", code, body)
	}
	if code, body := e.get("/a/x", "Authorization", "Bearer "+newToken); code != 200 {
		t.Fatalf("failed reload lost the old set: %d %s", code, body)
	}
	// Retiring a key is independent of policy rollback.
	trust.Keys = []VerificationKey{{Kid: "test-b", PublicKeyFile: keyFile}}
	if err := os.WriteFile(keyFile, other.publicPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	section(config, "gateway")["jwt"] = trust
	e.writeNative(config)
	if code, body := post("keys-retire-old-01"); code != 200 {
		t.Fatalf("retirement: %d %s", code, body)
	}
	_, provisional := e.provisionalFrom(s, e.protectedPolicy(), 1, 60, "keys-policy-trial")
	if code, _, body := e.resolve(s, "rollback", provisional.Provisional, `"r1"`, "keys-policy-rollback"); code != 200 {
		t.Fatalf("rollback: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+oldToken); code != 401 {
		t.Fatalf("rollback re-trusted retired key: %d", code)
	}
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+newToken); code != 200 {
		t.Fatalf("rollback lost current key: %d", code)
	}
}

func TestFetchKeyReloadPermissionsAndHTTPMaterial(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	_, password := e.addAdmin(binaries.plain, "reader", "read")
	e.start(binaries.plain)
	reader := e.mustLogin("reader", password)
	if code, _, _ := e.adminSend(http.MethodGet, "/admin/keys", ""); code != 401 {
		t.Fatalf("unauthenticated key list: %d", code)
	}
	if code, body, _ := e.as(reader, http.MethodGet, "/admin/keys", ""); code != 200 {
		t.Fatalf("reader cannot list: %d %s", code, body)
	}
	if code, _, _ := e.as(reader, http.MethodPost, "/admin/keys/reload", "", "Idempotency-Key", "keys-reader-forbidden"); code != 403 {
		t.Fatalf("reader reloaded keys: %d", code)
	}
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	if code, _, _ := e.as(admin, http.MethodPost, "/admin/keys/reload", `{"keys":[]}`, "Idempotency-Key", "keys-http-material-01"); code != 400 {
		t.Fatalf("reload accepted HTTP material: %d", code)
	}
	if code, _, _ := e.as(admin, http.MethodPost, "/admin/keys/reload", ""); code != 400 {
		t.Fatalf("reload accepted no idempotency key: %d", code)
	}
}

// Repeated whole-set reloads while requests verify must not publish a
// partial set or race. Both keys remain configured through this test.
func TestFetchConcurrentKeyReload(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	other := newTestIssuer(2048)
	keyFile := filepath.Join(e.dir, "concurrent.pem")
	if err := os.WriteFile(keyFile, other.publicPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	config := e.nativeSettings()
	trust := testTrust(testIssuerName, testAudience, e.publicKey)
	trust.Keys = append(trust.Keys, VerificationKey{Kid: "test-b", PublicKeyFile: keyFile})
	section(config, "gateway")["jwt"] = trust
	e.writeNative(config)
	e.start(binaries.plain)
	s := e.mustLogin(harnessAdmin, harnessPassword)
	tokens := []string{issuer.tokenFor(t, "alice"), other.signRaw(`{"alg":"RS256","typ":"JWT","kid":"test-b"}`, claimsJSON(validClaims("alice")))}
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for range 10 {
				if code, _ := e.get("/a/x", "Authorization", "Bearer "+tokens[worker%2]); code != 200 {
					t.Errorf("worker %d saw partial trust: %d", worker, code)
				}
			}
		})
	}
	for range 5 {
		code, body, _ := e.as(s, http.MethodPost, "/admin/keys/reload", "", "Idempotency-Key", "keys-concurrent-"+newToken()[:16])
		if code != 200 {
			t.Errorf("reload: %d %s", code, body)
		}
	}
	wg.Wait()
}
