package gateway

import (
	"slices"
	"testing"
)

func TestFetchRolesOnlyFromVerifiedToken(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	for _, tc := range []struct {
		subject   string
		roles     any
		status    int
		forwarded string
	}{
		{"alice", nil, 403, ""},
		{"outside-policy-membership", []string{"WRITER", "UNKNOWN"}, 200, "WRITER"},
		{"alice", []string{"READER"}, 403, ""},
		{"bob", []string{"WRITER", "READER", "WRITER"}, 200, "READER,WRITER"},
		{"outside-policy-membership", []string{"UNKNOWN"}, 403, ""},
	} {
		claims := changed(validClaims(tc.subject), map[string]any{"roles": tc.roles})
		resp, body := e.exchange("POST", "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+issuer.token(t, claims), "X-Gateway-Roles", "WRITER")
		if resp.StatusCode != tc.status {
			t.Errorf("%s %v: %d %s", tc.subject, tc.roles, resp.StatusCode, body)
			continue
		}
		if tc.status == 200 {
			receipts := e.a.receipts()
			if receipts[len(receipts)-1].Headers.Get("X-Gateway-Roles") != tc.forwarded {
				t.Errorf("forwarded roles: %+v", receipts[len(receipts)-1])
			}
		}
	}
	e.assertReceipts(2, 0, 0)
	droppedUnknown := func(events []map[string]any) bool {
		for _, event := range events {
			roles, _ := event["ignored_roles"].([]any)
			if event["subject"] == "outside-policy-membership" && slices.Contains(roles, any("UNKNOWN")) {
				return true
			}
		}
		return false
	}
	if !droppedUnknown(e.eventsWhen(droppedUnknown)) {
		t.Fatal("the dropped UNKNOWN role was not recorded")
	}
}

func TestFetchRejectsLegacyPolicyAndSubjects(t *testing.T) {
	for document, want := range map[string]string{
		`{"schema_version":3,"profile":"core"}`:               "SCHEMA_VERSION_UNSUPPORTED",
		`{"schema_version":4,"profile":"core","subjects":[]}`: "UNKNOWN_FIELD",
	} {
		if _, err := DecodePolicy([]byte(document)); errorCode(err) != want {
			t.Errorf("%s: %s, want %s", document, errorCode(err), want)
		}
	}
}
