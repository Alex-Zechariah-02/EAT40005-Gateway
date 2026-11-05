// Body ceilings at fixtures, trailers and interrupted bodies.

package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
)

// Through the real orders fixture: a valid order padded to exactly the
// effective ceiling creates an order from exactly the bytes sent, and one
// byte more is refused with 413, after authentication, authorisation and the
// rate limit when it is refused at the body stage, leaving no receipt and no
// state change. This holds with the global ceiling below, above and equal to
// the operation's, for every framing. Neither the token nor any body content
// is recorded in the audit log.
func TestBodyCeilingAtFixtures(t *testing.T) {
	fixtures := startFixtures(t)
	orders := fixtures["orders"]
	token := issuer.tokenFor(t, "cust-001")
	order := `{"customer":"cust-001","item":"ITM-001","quantity":1}`
	authenticated := []string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS", "JWT", "RBAC", "RATE"}

	configs := []struct {
		name              string
		global, operation int
	}{
		{"global below the operation", 1024, 2048},
		{"operation below the global", 16384, 1024},
		{"equal ceilings", 2048, 2048},
	}
	for _, c := range configs {
		e := newEnv(t)
		e.writeConfig(configOptions{maxBodyBytes: int64(c.global)})
		p := fixturePolicy(fixtures)
		for i := range p.Operations {
			if p.Operations[i].ID == "orders.create" {
				p.Operations[i].Body = jsonBody(int64(c.operation), ordersCreateSchema)
			}
		}
		seed(t, e.database, p, seedOptions{})
		e.start(binaries.plain)
		ceiling := min(c.global, c.operation)

		for _, framing := range bodyFramings {
			for _, size := range []int{ceiling, ceiling + 1} {
				name := fmt.Sprintf("%s (global %d, operation %d), %s, %d bytes", c.name, c.global, c.operation, framing, size)
				body := order + strings.Repeat(" ", size-len(order))
				before, state := len(orders.receipts()), orders.state().SHA256
				status, reply := e.sendBody(framing, "/orders", body, "Authorization", "Bearer "+token)
				receipts := orders.receipts()
				if size == ceiling {
					if status != http.StatusCreated || len(receipts) != before+1 {
						t.Fatalf("%s: status %d (%s), %d new receipts, want 201 and one", name, status, reply, len(receipts)-before)
					}
					last := receipts[len(receipts)-1]
					if last.Operation != "orders.create" || last.BodyBytes != size || last.BodySHA256 != sha256Hex(body) ||
						!strings.HasPrefix(last.Effect, "created ORD-") || last.Authorization || last.Subject != "cust-001" {
						t.Errorf("%s: receipt %+v", name, last)
					}
					continue
				}

				stage, _ := sizeRefusal(framing, size, c.global)
				want := expectedStages(nil, "INGRESS", audit.StageReject)
				if stage == "BODY" {
					want = expectedStages(authenticated, "BODY", audit.StageReject)
				}
				_, id := requestID(&http.Response{StatusCode: status}, reply)
				events := e.requestEvents(id, 1)
				if status != http.StatusRequestEntityTooLarge || len(events) != 1 || events[0]["stage"] != stage || !sameStages(events[0]["stages"], want) {
					t.Errorf("%s: status %d (%s), events %v, want 413 at %s", name, status, reply, events, stage)
				}
				if len(receipts) != before || orders.state().SHA256 != state {
					t.Fatalf("%s: a refused body reached the fixture (%d new receipts)", name, len(receipts)-before)
				}
			}
		}

		log, err := os.ReadFile(e.auditLog)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(log), token) || strings.Contains(string(log), "quantity") || strings.Contains(string(log), "ITM-001") {
			t.Errorf("%s: the audit log records the token or body content", c.name)
		}
		e.stop()
	}
	accepted := len(configs) * len(bodyFramings)
	if counts := receiptCounts(fixtures); counts["orders"] != accepted || counts["inventory"] != 0 || counts["reports"] != 0 {
		t.Fatalf("receipts %v, want the %d accepted orders only", counts, accepted)
	}
}

// A chunked body's trailer fields are not forwarded: the gateway reads the
// whole body and forwards it with a declared length. A client that waits for
// 100 Continue receives it once the gateway reads an admitted request's body.
// The body then arrives intact with its Expect field, and the upstream's own
// 100 Continue is passed back, so the client receives two before the final
// response.
func TestRequestTrailersAndExpectContinue(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)
	body := `{"n":1}`

	status, reply := e.rawExchange("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\n"+
		"Transfer-Encoding: chunked\r\nTrailer: X-Trailer\r\n\r\n"+fmt.Sprintf("%x\r\n%s\r\n", len(body), body)+"0\r\nX-Trailer: t\r\n\r\n", false)
	if status != http.StatusOK {
		t.Fatalf("chunked request with a trailer: %d (%s)", status, reply)
	}
	first := e.a.receipts()[0]
	if first.Body != body || first.Headers.Get("Content-Length") != "7" || len(first.Trailer) != 0 || first.Headers.Get("Trailer") != "" {
		t.Errorf("forwarded body %q, Content-Length %q, trailers %v, Trailer field %q",
			first.Body, first.Headers.Get("Content-Length"), first.Trailer, first.Headers.Get("Trailer"))
	}

	conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nContent-Length: 7\r\n\r\n")
	reader := bufio.NewReader(conn)
	interim, err := http.ReadResponse(reader, nil)
	if err != nil || interim.StatusCode != http.StatusContinue {
		t.Fatalf("interim response %v: %v", interim, err)
	}
	io.WriteString(conn, body)
	interims := 1
	final, err := http.ReadResponse(reader, nil)
	for err == nil && final.StatusCode == http.StatusContinue {
		interims++
		final, err = http.ReadResponse(reader, nil)
	}
	if err != nil || final.StatusCode != http.StatusOK || interims != 2 {
		t.Fatalf("final response %v after %d interim responses: %v", final, interims, err)
	}
	second := e.a.receipts()[1]
	if second.Body != body || second.Headers.Get("Expect") != "100-continue" {
		t.Errorf("forwarded body %q with Expect %q", second.Body, second.Headers.Get("Expect"))
	}
	e.assertReceipts(2, 0, 0)
}

// A client that stops sending part-way, or disconnects, never causes any
// part of its body to be forwarded.
func TestInterruptedBodiesAreNeverForwarded(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)

	head := "POST /a/item HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n"
	// Disconnect after part of the body.
	conn, err := net.Dial("tcp", e.dataAddr)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, head+`{"a":"`+strings.Repeat("x", 50))
	time.Sleep(200 * time.Millisecond)
	conn.Close()
	// Stop sending, keep the connection open, and let the server's read
	// timeout end the request.
	stalled, err := net.Dial("tcp", e.dataAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	io.WriteString(stalled, head+`{"a":`)
	// A chunked body that ends without its final chunk.
	status, _ := e.rawExchange("POST /a/item HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n5\r\n{\"a\":\r\n", true)
	if status != http.StatusBadRequest {
		t.Errorf("chunked body without its end: %d, want 400", status)
	}

	// The stalled request ends at the server's 10 s read timeout.
	failed := 0
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		failed = 0
		for _, ev := range e.auditEvents() {
			if ev["kind"] == "request.rejected" && ev["stage"] == "BODY" && ev["reason"] == "read_failed" {
				failed++
			}
		}
		if failed >= 3 {
			break
		}
	}
	if failed != 3 {
		t.Errorf("%d body read failures recorded, want 3 (disconnect, stalled, chunked)", failed)
	}
	e.assertReceipts(0, 0, 0)
}
