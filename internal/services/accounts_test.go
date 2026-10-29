package services

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func accountService(t *testing.T) (*Service, *rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	private := filepath.Join(dir, "private.pem")
	settings := filepath.Join(dir, "accounts.json")
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(private, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	document, _ := json.Marshal(map[string]string{"kid": "fetch-test-a", "private_key_file": private})
	if err = os.WriteFile(settings, document, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Name: "accounts", Secret: testSecret, SigningSettingsFile: settings})
	if err != nil {
		t.Fatal(err)
	}
	return s, key, settings
}

func TestAccountsTokensAndProfiles(t *testing.T) {
	s, key, _ := accountService(t)
	for _, user := range Users() {
		body, _ := json.Marshal(map[string]string{"email": user.Email, "password": "FetchDemo2026!"})
		w := serviceRequest(s, "POST", "/accounts/login", string(body), "", "", "")
		if w.Code != 200 || w.Header().Get("X-Service") != "accounts" {
			t.Fatalf("login %s: status %d, service %q", user.ID, w.Code, w.Header().Get("X-Service"))
		}
		var reply struct {
			Token   string    `json:"token"`
			Expires time.Time `json:"expires_at"`
			User    User      `json:"user"`
		}
		if json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.User.ID != user.ID || reply.User.Home != user.Home {
			t.Fatalf("incorrect profile fields for %s", user.ID)
		}
		signed, err := jose.ParseSignedCompact(reply.Token, []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := signed.Verify(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			Issuer   string   `json:"iss"`
			Audience string   `json:"aud"`
			Subject  string   `json:"sub"`
			Roles    []string `json:"roles"`
			Issued   int64    `json:"iat"`
			Before   int64    `json:"nbf"`
			Expires  int64    `json:"exp"`
		}
		if json.Unmarshal(payload, &claims) != nil || claims.Issuer != "fetch-accounts" || claims.Audience != "fetch-gateway" || claims.Subject != user.ID || !slices.Equal(claims.Roles, user.Roles) || claims.Expires-claims.Issued != 900 || claims.Before != claims.Issued || reply.Expires.Unix() != claims.Expires || signed.Signatures[0].Header.KeyID != "fetch-test-a" {
			t.Fatalf("token profile for %s is invalid", user.ID)
		}
		for _, path := range []string{"/accounts/refresh", "/accounts/me"} {
			method := "GET"
			if strings.HasSuffix(path, "refresh") {
				method = "POST"
			}
			response := serviceRequest(s, method, path, "", user.ID, strings.Join(user.Roles, ","), "")
			if response.Code != 200 {
				t.Fatalf("%s %s: %d %s", user.ID, path, response.Code, response.Body)
			}
		}
	}
	state, _ := json.Marshal(s.state())
	receipts, _ := json.Marshal(s.receipts)
	for _, secret := range []string{"FetchDemo2026!", "PRIVATE KEY", `"token":`, `"password":`} {
		if strings.Contains(string(state), secret) || strings.Contains(string(receipts), secret) {
			t.Fatalf("Accounts control data leaked %q", secret)
		}
	}
}

func TestAccountsCredentialFailures(t *testing.T) {
	s, _, _ := accountService(t)
	unknown := serviceRequest(s, "POST", "/accounts/login", `{"email":"nobody@fetch.test","password":"wrong-password"}`, "", "", "")
	wrong := serviceRequest(s, "POST", "/accounts/login", `{"email":"customer1@fetch.test","password":"wrong-password"}`, "", "", "")
	if unknown.Code != 401 || wrong.Code != 401 || unknown.Body.String() != wrong.Body.String() || !strings.Contains(unknown.Body.String(), "invalid_credentials") {
		t.Fatalf("credential enumeration: %d %s / %d %s", unknown.Code, unknown.Body, wrong.Code, wrong.Body)
	}
	for _, path := range []string{"/accounts/refresh", "/accounts/me"} {
		method := "GET"
		if strings.HasSuffix(path, "refresh") {
			method = "POST"
		}
		if w := serviceRequest(s, method, path, "", "", "", ""); w.Code != 401 {
			t.Errorf("anonymous %s: %d", path, w.Code)
		}
	}
	if w := serviceRequest(s, "POST", "/accounts/login", strings.Repeat(" ", 512)+`{}`, "", "", ""); w.Code != 413 {
		t.Errorf("login body limit: %d", w.Code)
	}
}
