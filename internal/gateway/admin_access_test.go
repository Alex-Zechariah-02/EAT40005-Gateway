package gateway

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// The access checker answers with the request handler's own matching and
// permission code, for the generation in force or a stored draft, checks the
// blocked accounts for a subject, and says what it could not check.
func TestAdminAccessCheck(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	type answer struct {
		Decision     string   `json:"decision"`
		Code         *string  `json:"code"`
		Check        *string  `json:"check"`
		Operation    *string  `json:"operation"`
		IgnoredRoles []string `json:"ignored_roles"`
		Candidate    *int64   `json:"candidate"`
		NotChecked   []string `json:"not_checked"`
		Forwarded    *string  `json:"forwarded_path"`
	}
	ask := func(body string) answer {
		t.Helper()
		code, data, _ := e.as(admin, "POST", "/admin/access-check", body, "Content-Type", "application/json")
		var reply answer
		if code != 200 || json.Unmarshal([]byte(data), &reply) != nil {
			t.Fatalf("access check %s: %d %s", body, code, data)
		}
		return reply
	}
	text := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	for _, tc := range []struct {
		body, decision, check, code, operation string
	}{
		{`{"method":"GET","path":"/a/x","roles":["READER"]}`, "allowed", "", "", "read-a"},
		{`{"method":"POST","path":"/a/item","roles":["READER"]}`, "refused", "role", "forbidden", "write-a"},
		{`{"method":"POST","path":"/a/item","roles":["WRITER","GHOST"]}`, "allowed", "", "", "write-a"},
		{`{"method":"GET","path":"/d","roles":["WRITER"]}`, "refused", "access", "forbidden", "denied"},
		{`{"method":"GET","path":"/nowhere","roles":["WRITER"]}`, "refused", "route", "not_found", ""},
		{`{"method":"DELETE","path":"/a/item","roles":["WRITER"]}`, "refused", "route", "method_not_allowed", ""},
	} {
		got := ask(tc.body)
		if got.Decision != tc.decision || text(got.Check) != tc.check || text(got.Code) != tc.code || text(got.Operation) != tc.operation {
			t.Errorf("%s: %+v", tc.body, got)
		}
	}
	if got := ask(`{"method":"POST","path":"/a/item","roles":["WRITER","GHOST"]}`); !slices.Equal(got.IgnoredRoles, []string{"GHOST"}) || !slices.Equal(got.NotChecked, []string{"token", "rate", "body"}) {
		t.Errorf("ignored roles and unchecked steps: %+v", got)
	}

	// A subject is also checked against the blocked accounts.
	if code, _, body := e.blockAccount(admin, "alice", "access check", "access-check-block-1"); code != 200 {
		t.Fatalf("block: %d %s", code, body)
	}
	if got := ask(`{"method":"GET","path":"/a/x","roles":["WRITER"],"subject":"alice"}`); got.Decision != "refused" || text(got.Check) != "blocked" || text(got.Code) != "account_blocked" {
		t.Errorf("blocked subject: %+v", got)
	}

	// A draft is compiled and checked without being activated.
	widened := e.protectedPolicy()
	widened.Permits = append(widened.Permits, policy.Permit{Role: "READER", Operation: "write-a"})
	code, staged, body := e.stage(admin, widened, `"r1"`, "access-check-stage-1")
	if code != 201 {
		t.Fatalf("stage: %d %s", code, body)
	}
	draft := fmt.Sprintf(`{"method":"POST","path":"/a/item","roles":["READER"],"candidate":%d}`, staged.Candidate.ID)
	if got := ask(draft); got.Decision != "allowed" || got.Candidate == nil || *got.Candidate != staged.Candidate.ID {
		t.Errorf("draft grants READER write-a: %+v", got)
	}

	// The answer names the path the upstream would receive.
	rewritten := e.protectedPolicy()
	rewritten.APIs[0].Forward = &policy.Forward{StripPrefix: "/a", AddPrefix: "/v1"}
	if code, staged, body = e.stage(admin, rewritten, `"r1"`, "access-check-stage-2"); code != 201 {
		t.Fatalf("stage: %d %s", code, body)
	}
	if got := ask(fmt.Sprintf(`{"method":"POST","path":"/a/item","roles":["WRITER"],"candidate":%d}`, staged.Candidate.ID)); text(got.Forwarded) != "/v1/item" {
		t.Errorf("forwarded path of a draft with a forward rule: %+v", got)
	}
	if got := ask(`{"method":"POST","path":"/a/item","roles":["READER"]}`); got.Decision != "refused" {
		t.Errorf("the policy in force still refuses: %+v", got)
	}

	for _, tc := range []struct{ body, field string }{
		{`{"method":"GET","path":"/a/%2fx","roles":[]}`, "path"},
		{`{"method":"GET","path":"a/x","roles":[]}`, "path"},
		{`{"method":"GET","path":"/a/x?y=1","roles":[]}`, "path"},
		{`{"method":"BREW","path":"/a/x","roles":[]}`, "method"},
		{`{"method":"GET","path":"/a/x","roles":[""]}`, "roles"},
	} {
		if code, data, _ := e.as(admin, "POST", "/admin/access-check", tc.body, "Content-Type", "application/json"); code != 400 || !json.Valid([]byte(data)) || !strings.Contains(data, `"field":"`+tc.field+`"`) {
			t.Errorf("%s: %d %s", tc.body, code, data)
		}
	}
	if code, data, _ := e.as(admin, "POST", "/admin/access-check", `{"method":"GET","path":"/a/x","roles":[],"candidate":999}`, "Content-Type", "application/json"); code != 404 {
		t.Errorf("unknown draft: %d %s", code, data)
	}
}
