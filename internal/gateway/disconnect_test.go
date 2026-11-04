package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// An administrator who disconnects while a committed change waits for its
// outcome record does not stop the gateway: the change finishes, its outcome
// is recorded and protected requests are still admitted.
func TestDisconnectDuringOutcomeKeepsAudit(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=cancel-before-outcome")
	admin := e.mustLogin(harnessAdmin, harnessPassword)

	req, err := http.NewRequest(http.MethodPut, "http://"+e.admin+"/admin/blocked-accounts/someone", strings.NewReader(`{"reason":"disconnect test"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	headers := append(admin.headers(e), "Idempotency-Key", "disconnect-block")
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	// The client gives up while the change waits for its outcome record.
	if resp, err := (&http.Client{Timeout: 500 * time.Millisecond}).Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("the change was answered (%d) before the client gave up", resp.StatusCode)
	}

	transition := "admin:" + harnessAdmin + ":disconnect-block"
	e.eventsWhen(func(events []map[string]any) bool {
		for _, ev := range events {
			if ev["kind"] == "account.block" && ev["transition"] == transition {
				return true
			}
		}
		return false
	})
	if s := e.status(); s.AuditFailed {
		t.Fatal("a client disconnect failed the audit log")
	}
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusOK {
		t.Fatalf("protected request after the disconnect: %d", code)
	}
}
