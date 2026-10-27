package gateway

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestFetchImpactPreservesIndependentControls(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked_%t", blocked), func(t *testing.T) {
			e := newEnv(t)
			policy := e.protectedPolicy()
			seed(t, e.database, policy, seedOptions{})
			e.start(binaries.plain)
			admin := e.mustLogin(harnessAdmin, harnessPassword)
			if blocked {
				if code, _, body := e.blockAccount(admin, "alice", "independent containment", "impact-independent-block"); code != 200 {
					t.Fatalf("block: %d %s", code, body)
				}
				if code, _ := e.get("/a/independent", bearer(t, "alice")...); code != 403 {
					t.Fatalf("blocked probe: %d", code)
				}
				policy = withoutRole(policy, "WRITER")
			} else {
				e.mustOverride(admin, permitOverride(overrideException, "READER", "write-a", 60), `"r1"`, "impact-independent-override")
				if code, _ := e.request("POST", "/a/item", "application/json", `{"n":1}`, bearer(t, "bob")...); code != 200 {
					t.Fatalf("override probe: %d", code)
				}
				policy.Roles = append(policy.Roles, Role{ID: "OTHER"})
			}
			code, staged, body := e.stage(admin, policy, `"r1"`, "impact-independent-candidate")
			if code != 201 {
				t.Fatalf("stage: %d %s", code, body)
			}
			before := e.status()
			code, body, _ = e.as(admin, "POST", fmt.Sprintf("/admin/candidates/%d/impact", staged.Candidate.ID), "")
			var impact struct {
				Flips []json.RawMessage `json:"flips"`
			}
			if code != 200 || json.Unmarshal([]byte(body), &impact) != nil || len(impact.Flips) != 0 {
				t.Errorf("preview lost an independent control: %d %s", code, body)
			}
			if after := e.status(); after.Generation != before.Generation || after.Revision != before.Revision {
				t.Fatal("preview changed authority")
			}
			code, _, body = e.activate(admin, staged.Candidate.candidateRecord, 60, `"r1"`, "impact-independent-activate")
			if code != 200 {
				t.Fatalf("actual activation: %d %s", code, body)
			}
			if blocked {
				if code, _ := e.get("/a/independent", bearer(t, "alice")...); code != 403 {
					t.Fatalf("actual blocked decision: %d", code)
				}
			} else {
				if code, _ := e.request("POST", "/a/item", "application/json", `{"n":1}`, bearer(t, "bob")...); code != 200 {
					t.Fatalf("actual overridden decision: %d", code)
				}
			}
		})
	}
}

func TestFetchImpactAccessGrantHasKnownOutcome(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	p.Operations[0].Access = "deny"
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	if code, _ := e.get("/a/access-preview", bearer(t, "alice")...); code != 403 {
		t.Fatalf("denied baseline: %d", code)
	}
	p.Operations[0].Access = "allow"
	code, staged, body := e.stage(admin, p, `"r1"`, "impact-access-candidate")
	if code != 201 {
		t.Fatalf("stage: %d %s", code, body)
	}
	code, body, _ = e.as(admin, "POST", fmt.Sprintf("/admin/candidates/%d/impact", staged.Candidate.ID), "")
	var impact struct {
		Window struct {
			Requests, Previewed int
			IdentityUnknown     int `json:"identity_unknown"`
		} `json:"window"`
		Flips []struct {
			Before, After string
			Count         int
		} `json:"flips"`
		NotPreviewed []string `json:"not_previewed"`
	}
	// The token was not evaluated before the access refusal. The preview must
	// disclose that uncertainty rather than guess an authentication result.
	if code != 200 || json.Unmarshal([]byte(body), &impact) != nil || impact.Window.Requests != 1 || impact.Window.Previewed != 0 || impact.Window.IdentityUnknown != 1 || len(impact.Flips) != 0 || !slices.Equal(impact.NotPreviewed, []string{"rate", "body", "unobserved_identity"}) {
		t.Errorf("preview did not disclose unobserved identity: %d %s", code, body)
	}
	code, activation, body := e.activate(admin, staged.Candidate.candidateRecord, 60, `"r1"`, "impact-access-activate")
	if code != 200 {
		t.Fatalf("activate: %d %s", code, body)
	}
	if code, _, body := e.resolve(admin, "confirm", activation.Provisional, `"r1"`, "impact-access-confirm"); code != 200 {
		t.Fatalf("confirm: %d %s", code, body)
	}
	if code, _ := e.get("/a/access-preview", bearer(t, "alice")...); code != 200 {
		t.Fatalf("actual decision after grant: %d", code)
	}
	e.assertReceipts(1, 0, 0)
}

func TestFetchImpactIdentityCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, auth, access, credential, afterAuth string
		unknown, flips, actual                    int
	}{
		{"absent-before-access", "required", "deny", "", "required", 0, 0, 401},
		{"unread-public-token", "none", "allow", "valid", "required", 1, 0, 200},
		{"anonymous-public", "none", "allow", "", "required", 0, 1, 401},
		{"no-identity-needed", "required", "deny", "valid", "none", 0, 1, 200},
		{"invalid-identity-observed", "required", "allow", "invalid", "none", 0, 1, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			policy := e.protectedPolicy()
			policy.Operations[0].Auth, policy.Operations[0].Access = tc.auth, tc.access
			if tc.auth == "none" {
				policy.Permits = slices.DeleteFunc(policy.Permits, func(p Permit) bool { return p.Operation == "read-a" })
			}
			seed(t, e.database, policy, seedOptions{})
			e.start(binaries.plain)
			admin := e.mustLogin(harnessAdmin, harnessPassword)
			headers := []string{}
			if tc.credential == "valid" {
				headers = bearer(t, "alice")
			} else if tc.credential == "invalid" {
				headers = []string{"Authorization", "Bearer invalid"}
			}
			e.get("/a/identity-coverage", headers...)
			candidate := e.protectedPolicy()
			candidate.Operations[0].Auth = tc.afterAuth
			if tc.afterAuth == "none" {
				candidate.Permits = slices.DeleteFunc(candidate.Permits, func(p Permit) bool { return p.Operation == "read-a" })
			}
			code, staged, body := e.stage(admin, candidate, `"r1"`, "impact-coverage-candidate")
			if code != 201 {
				t.Fatalf("stage: %d %s", code, body)
			}
			code, body, _ = e.as(admin, "POST", fmt.Sprintf("/admin/candidates/%d/impact", staged.Candidate.ID), "")
			var result struct {
				Window struct {
					Requests, Previewed int
					IdentityUnknown     int `json:"identity_unknown"`
				} `json:"window"`
				Flips        []json.RawMessage `json:"flips"`
				NotPreviewed []string          `json:"not_previewed"`
			}
			if code != 200 || json.Unmarshal([]byte(body), &result) != nil || result.Window.Requests != 1 || result.Window.IdentityUnknown != tc.unknown || result.Window.Previewed != 1-tc.unknown || len(result.Flips) != tc.flips || slices.Contains(result.NotPreviewed, "unobserved_identity") != (tc.unknown > 0) {
				t.Errorf("coverage: %d %s", code, body)
			}
			code, _, body = e.activate(admin, staged.Candidate.candidateRecord, 60, `"r1"`, "impact-coverage-activate")
			if code != 200 {
				t.Fatalf("activate: %d %s", code, body)
			}
			if code, _ := e.get("/a/identity-coverage", headers...); code != tc.actual {
				t.Fatalf("actual decision: %d, want %d", code, tc.actual)
			}
		})
	}
}
