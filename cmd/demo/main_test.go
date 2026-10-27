package main

import (
	"encoding/json"
	"slices"
	"testing"
)

// decoded is the part of a policy document these tests compare.
type decoded struct {
	Upstreams []struct {
		ID   string `json:"id"`
		Dial string `json:"dial"`
	} `json:"upstreams"`
	Operations []struct {
		ID     string `json:"id"`
		API    string `json:"api"`
		Method string `json:"method"`
		Target string `json:"target"`
		Access string `json:"access"`
		Auth   string `json:"auth"`
	} `json:"operations"`
	Permits []struct {
		Role      string `json:"role"`
		Operation string `json:"operation"`
	} `json:"permits"`
	SchemaVersion int `json:"schema_version"`
}

func decode(t *testing.T, policy map[string]any) decoded {
	t.Helper()
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	var d decoded
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// The demonstration policy matches the Fetch baseline: the same
// operations, each permitted to exactly the roles the catalogue intends, the
// same token roles and permits, and one upstream per service at the
// address the run command starts it on.
func TestBaselineMatchesCatalogue(t *testing.T) {
	checkFetchBaseline(t)
	p := decode(t, baselinePolicy())
	if _, exists := baselinePolicy()["subjects"]; exists {
		t.Fatal("v4 contains policy membership")
	}
	for i, service := range fetchServices {
		if p.Upstreams[i].ID != service.name+"-svc" || p.Upstreams[i].Dial != service.listen {
			t.Errorf("upstream %d does not match its service", i)
		}
	}
}

// The first policy must deny everything: every operation denied, no permits
// and no subjects, and otherwise the same operations as the baseline.
func TestDenyAllDeniesEverything(t *testing.T) {
	baseline := baselinePolicy()
	p := decode(t, denyAll(baseline))
	if _, exists := denyAll(baseline)["subjects"]; exists || len(p.Permits) != 0 {
		t.Errorf("deny-all grants or contains removed membership: %+v", p)
	}
	for _, op := range p.Operations {
		if op.Access != "deny" {
			t.Errorf("%s access %s", op.ID, op.Access)
		}
	}
	if decode(t, baseline).Operations[0].Access != "allow" {
		t.Error("deriving the deny-all policy changed the baseline")
	}
}

func TestValidSubject(t *testing.T) {
	for subject, want := range map[string]bool{
		"staff-01":                              true,
		"":                                      false,
		"has space":                             false,
		"tab\there":                             false,
		string(make([]byte, 129)):               false,
		"é":                                     false,
		"x" + string(rune(0x7f)):                false,
		string(slices.Repeat([]byte{'a'}, 128)): true,
	} {
		if validSubject(subject) != want {
			t.Errorf("validSubject(%q) = %v", subject, !want)
		}
	}
}

// up reads the enrolment secret from the line the gateway's install command
// prints, and only for the administrator it installed.
func TestEnrolmentSecret(t *testing.T) {
	install := "installed C:\\demo\\state.db (installation 0011, mode bootstrap_locked)\n" +
		"enrolment secret for root, valid until 2026-10-27T10:00:00Z: abc123secret\n"
	if secret, err := enrolmentSecret(install); err != nil || secret != "abc123secret" {
		t.Errorf("enrolmentSecret = %q, %v", secret, err)
	}
	for _, output := range []string{
		"",
		"enrolment secret for rooted, valid until 2026-10-27T10:00:00Z: other\n",
		"enrolment secret for root, valid until 2026-10-27T10:00:00Z: \n",
	} {
		if secret, err := enrolmentSecret(output); err == nil {
			t.Errorf("enrolmentSecret(%q) = %q, want an error", output, secret)
		}
	}
}
