package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchNamedKeySet(t *testing.T) {
	dir := t.TempDir()
	other := newTestIssuer(2048)
	first, second := filepath.Join(dir, "first.pem"), filepath.Join(dir, "second.pem")
	for path, key := range map[string]*testIssuer{first: issuer, second: other} {
		if err := os.WriteFile(path, key.publicPEM(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	document, err := json.Marshal(map[string]any{
		"issuer": testIssuerName, "audience": testAudience,
		"keys": []map[string]string{{"kid": "test-a", "public_key_file": first}, {"kid": "test-b", "public_key_file": second}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var config TrustConfig
	if err := json.Unmarshal(document, &config); err != nil {
		t.Fatal(err)
	}
	v, err := loadVerifier(config)
	if err != nil {
		t.Fatalf("several operator-configured keys must load together: %v", err)
	}
	for _, tc := range []struct {
		kid    string
		signer *testIssuer
		ok     bool
	}{
		{"test-a", issuer, true}, {"test-b", other, true}, {"test-a", other, false}, {"test-b", issuer, false}, {"unknown", issuer, false},
	} {
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": tc.kid})
		_, reason := v.verify(tc.signer.signRaw(string(header), claimsJSON(claimsAt(nil))), fixedNow)
		if got := reason == ""; got != tc.ok {
			t.Errorf("kid %s selected the wrong trust key: accepted=%t (%s)", tc.kid, got, reason)
		}
	}
}

func TestFetchJWTRequiredClaimsAndKid(t *testing.T) {
	v := testVerifier(t)
	for _, tc := range []struct {
		name, header string
		claims       map[string]any
		accept       bool
	}{
		{"trusted kid", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(nil), true},
		{"unknown kid", `{"alg":"RS256","typ":"JWT","kid":"unknown"}`, claimsAt(nil), false},
		{"missing kid", `{"alg":"RS256","typ":"JWT"}`, claimsAt(nil), false},
		{"missing issuer", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(map[string]any{"iss": nil}), false},
		{"missing audience", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(map[string]any{"aud": nil}), false},
		{"missing expiry", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(map[string]any{"exp": nil}), false},
		{"missing subject", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(map[string]any{"sub": nil}), false},
		{"roles as text", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(map[string]any{"roles": "OPERATIONS"}), false},
		{"mixed roles", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(map[string]any{"roles": []any{"CUSTOMER", 1}}), false},
		{"unsafe role", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(map[string]any{"roles": []string{"CUSTOMER\r\nX-Admin: true"}}), false},
		{"too many roles", `{"alg":"RS256","typ":"JWT","kid":"test-a"}`, claimsAt(map[string]any{"roles": strings.Fields(strings.Repeat("CUSTOMER ", 65))}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, reason := v.verify(issuer.signRaw(tc.header, claimsJSON(tc.claims)), fixedNow)
			if got := reason == ""; got != tc.accept {
				t.Fatalf("accepted=%t (reason %q), want %t", got, reason, tc.accept)
			}
		})
	}
}
