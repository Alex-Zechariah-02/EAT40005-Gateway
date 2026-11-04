// JWT claim profile, time boundaries and ambiguous encodings.

package gateway

import (
	"strings"
	"testing"
	"time"
)

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
