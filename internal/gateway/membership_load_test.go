// Blocked-account changes during requests and under load.

package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

func TestMembershipRemovalDuringRequest(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	holdFile, release := filepath.Join(e.dir, "hold"), filepath.Join(e.dir, "release")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/item", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()
	conn, captured := holdRequest(t, e, holdFile, issuer.tokenFor(t, "alice"))
	defer conn.Close()
	if captured != before.Generation {
		t.Fatalf("captured %s", captured)
	}
	if code, _, body := e.blockAccount(admin, "alice", "instant containment", "account-block-in-flight"); code != 200 {
		t.Fatalf("block: %d %s", code, body)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "true}")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 403 || !strings.Contains(string(data), "account_blocked") {
		t.Fatalf("held request bypassed containment: %d %s", response.StatusCode, data)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 403 {
		t.Fatal("new request bypassed containment")
	}
	e.assertReceipts(0, 0, 0)
	if after := e.status(); after.Generation != before.Generation {
		t.Fatal("containment changed captured policy")
	}
	if code, _ := e.unblockAccount(admin, "alice", "account-unblock-in-flight"); code != 200 {
		t.Fatal("unblock failed")
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 200 {
		t.Fatal("unblocked request failed")
	}
}

func TestMembershipChangesUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	writer, reader := e.mustLogin(harnessAdmin, harnessPassword), e.mustLogin(harnessAdmin, harnessPassword)
	generation := e.status().Generation
	token := issuer.tokenFor(t, "bob")
	stop := make(chan struct{})
	var unexpected, reads atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest("GET", "http://"+e.dataAddr+"/a/x", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				response, err := client.Do(req)
				if err != nil {
					unexpected.Add(1)
					continue
				}
				response.Body.Close()
				if response.StatusCode != 200 && response.StatusCode != 403 {
					unexpected.Add(1)
				}
			}
		})
	}
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			code, body, _ := e.adminSend("GET", "/admin/blocked-accounts", "", reader.headers(e)...)
			var page struct {
				Items []store.BlockedAccount `json:"items"`
			}
			if code != 200 || json.Unmarshal([]byte(body), &page) != nil || len(page.Items) > 1 {
				unexpected.Add(1)
				continue
			}
			if len(page.Items) == 1 && (page.Items[0].Subject != "bob" || page.Items[0].Reason != "contain under load" || page.Items[0].BlockedBy != harnessAdmin || page.Items[0].BlockedAt.IsZero()) {
				unexpected.Add(1)
			}
			reads.Add(1)
		}
	})
	for i := range 60 {
		key := fmt.Sprintf("account-load-toggle-%04d", i)
		if i%2 == 0 {
			if code, _, body := e.blockAccount(writer, "bob", "contain under load", key); code != 200 {
				t.Fatalf("block: %d %s", code, body)
			}
		} else {
			if code, body := e.unblockAccount(writer, "bob", key); code != 200 {
				t.Fatalf("unblock: %d %s", code, body)
			}
		}
	}
	close(stop)
	wg.Wait()
	if unexpected.Load() != 0 {
		t.Fatalf("%d invalid responses or torn reads", unexpected.Load())
	}
	for _, receipt := range e.a.receipts() {
		if receipt.Generation != generation || receipt.Headers.Get("X-Gateway-Roles") != "READER" {
			t.Fatalf("untrusted receipt %+v", receipt)
		}
	}
	e.stop()
	forwarded, refused := 0, 0
	for _, event := range e.auditEvents() {
		if event["kind"] == "request.forwarded" {
			forwarded++
		}
		if event["kind"] == "request.rejected" {
			refused++
			if event["error"] != "account_blocked" || event["reason"] != "account_blocked" {
				t.Fatalf("unexpected refusal %v", event)
			}
		}
	}
	if forwarded == 0 || refused == 0 || reads.Load() == 0 {
		t.Fatalf("load did not exercise all paths: %d allowed, %d refused, %d reads", forwarded, refused, reads.Load())
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE kind IN ('account.block','account.unblock') AND outcome='committed' AND audited_at IS NOT NULL`); n != 60 {
		t.Fatalf("durable toggles: %d", n)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM revision`); n != 1 {
		t.Fatalf("containment changed policy: %d revisions", n)
	}
}
