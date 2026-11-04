// Fetch impact estimates.

package gateway

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestFetchImpactOfNewRole(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	token := issuer.token(t, changed(validClaims("alien-account"), map[string]any{"roles": []string{"ALIEN"}}))
	if code, _ := e.get("/a/new-role", "Authorization", "Bearer "+token); code != 403 {
		t.Fatalf("unknown role: %d", code)
	}
	p.Roles = append(p.Roles, Role{ID: "ALIEN"})
	p.Permits = append(p.Permits, Permit{Role: "ALIEN", Operation: "read-a"})
	code, staged, body := e.stage(admin, p, `"r1"`, "impact-new-role-candidate")
	if code != 201 {
		t.Fatalf("stage: %d %s", code, body)
	}
	code, body, _ = e.as(admin, "POST", fmt.Sprintf("/admin/candidates/%d/impact", staged.Candidate.ID), "")
	var impact struct {
		Flips []struct {
			Before, After string
			Count         int
			Roles         []string
		} `json:"flips"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &impact) != nil || len(impact.Flips) != 1 || impact.Flips[0].Before != "refused" || impact.Flips[0].After != "allowed" || impact.Flips[0].Count != 1 || !reflect.DeepEqual(impact.Flips[0].Roles, []string{"ALIEN"}) {
		t.Fatalf("new role impact: %d %s", code, body)
	}
	e.assertReceipts(0, 0, 0)
}

func TestFetchImpact(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	for range 3 {
		e.get("/a/impact", "Authorization", "Bearer "+issuer.tokenFor(t, "alice"))
	}
	p.Operations[0].Access = "deny"
	stageCode, candidate, stageBody := e.stage(admin, p, `"r1"`, "fetch-impact-candidate")
	if stageCode != 201 {
		t.Fatalf("stage: %d %s", stageCode, stageBody)
	}
	before := e.status()
	code, body, _ := e.as(admin, "POST", fmt.Sprintf("/admin/candidates/%d/impact", candidate.Candidate.ID), "")
	var impact struct {
		Window struct {
			Requests        int `json:"requests"`
			Previewed       int `json:"previewed"`
			IdentityUnknown int `json:"identity_unknown"`
		} `json:"window"`
		Flips []struct {
			Before, After, AfterCode string
			Count                    int
		} `json:"flips"`
		NotPreviewed []string `json:"not_previewed"`
	}
	if code != 200 {
		t.Fatalf("impact: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &impact); err != nil || impact.Window.Requests != 3 || impact.Window.Previewed != 3 || impact.Window.IdentityUnknown != 0 || len(impact.Flips) != 1 || impact.Flips[0].Before != "allowed" || impact.Flips[0].After != "refused" || impact.Flips[0].Count != 3 || !reflect.DeepEqual(impact.NotPreviewed, []string{"rate", "body"}) {
		t.Fatalf("impact: %s (%v)", body, err)
	}
	if after := e.status(); after.Generation != before.Generation || after.Revision != before.Revision {
		t.Fatal("preview changed authority")
	}
	for _, tc := range []struct {
		path string
		want int
	}{{"/admin/candidates/99999/impact", 404}, {"/admin/candidates/no/impact", 404}} {
		if code, _, _ := e.as(admin, "POST", tc.path, ""); code != tc.want {
			t.Errorf("%s: %d", tc.path, code)
		}
	}
}
