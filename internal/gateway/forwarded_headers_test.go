// Validated headers reach the upstream intact.

package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// These tests check, on the native proxy and real loopback upstreams, that
// every end-to-end header property the gateway validated reaches the
// upstream as validated, or that the request is refused before anything is
// forwarded. They use keywordPolicy: GET /a/probe requires X-Probe "on";
// POST /a/item requires Idempotency-Key, allows X-Mode "safe" or "fast" and
// validates a JSON body.

const keywordItem = `{"kind":"alpha","count":1}`

// probeRequest is GET /a/probe with the given extra header lines and a valid
// X-Probe.
func probeRequest(lines string) string {
	return "GET /a/probe HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n" + lines + "X-Probe: on\r\n\r\n"
}

// itemRequest is a valid POST /a/item with the given extra header lines.
func itemRequest(lines string) string {
	return fmt.Sprintf("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\n"+
		"Idempotency-Key: k-123456\r\n%sContent-Length: %d\r\n\r\n%s", lines, len(keywordItem), keywordItem)
}

// Validated headers and the validated media type reach the upstream with the
// values the gateway checked, whatever the case of their names. Missing,
// invalid and repeated values are refused, and a comma-separated value is
// one value, checked as a whole, as before.
func TestValidatedHeadersReachUpstream(t *testing.T) {
	e := newEnv(t)
	e.serveNative(e.keywordPolicy(), forwardTimeouts)

	admitted := []struct {
		name, request string
		want          map[string]string
	}{
		{"required header", probeRequest(""), map[string]string{"X-Probe": "on"}},
		{"lower-case name", strings.Replace(probeRequest(""), "X-Probe:", "x-probe:", 1), map[string]string{"X-Probe": "on"}},
		{"upper-case name", strings.Replace(probeRequest(""), "X-Probe:", "X-PROBE:", 1), map[string]string{"X-Probe": "on"}},
		{"write with every validated field", itemRequest("X-Mode: fast\r\n"),
			map[string]string{"Content-Type": "application/json", "Idempotency-Key": "k-123456", "X-Mode": "fast"}},
		{"media type with its parameter", strings.Replace(itemRequest(""), "application/json", "Application/JSON; charset=UTF-8", 1),
			map[string]string{"Content-Type": "Application/JSON; charset=UTF-8", "Idempotency-Key": "k-123456"}},
	}
	for _, tc := range admitted {
		before := e.a.count()
		if status, body := e.rawExchange(tc.request, false); status != http.StatusOK || e.a.count() != before+1 {
			t.Errorf("%s: %d %s with %d new receipts", tc.name, status, body, e.a.count()-before)
			continue
		}
		got := e.a.receipts()[before]
		for name, value := range tc.want {
			if values := got.Headers.Values(name); len(values) != 1 || values[0] != value {
				t.Errorf("%s: upstream received %s %q, want %q", tc.name, name, values, value)
			}
		}
		t.Logf("%s: upstream received %s %s with X-Probe %q, Content-Type %q, Idempotency-Key %q, X-Mode %q",
			tc.name, got.Method, got.Target, got.Headers.Get("X-Probe"), got.Headers.Get("Content-Type"), got.Headers.Get("Idempotency-Key"), got.Headers.Get("X-Mode"))
	}

	refused := []struct {
		name, request string
		status        int
		stage, reason string
	}{
		{"required header missing", strings.Replace(probeRequest(""), "X-Probe: on\r\n", "", 1), 400, "VALIDATION", "header_mismatch"},
		{"value outside the schema", strings.Replace(probeRequest(""), "X-Probe: on", "X-Probe: off", 1), 400, "VALIDATION", "header_mismatch"},
		{"repeated field", probeRequest("X-Probe: on\r\n"), 400, "VALIDATION", "repeated_header"},
		{"repeated field of another case", probeRequest("x-probe: on\r\n"), 400, "VALIDATION", "repeated_header"},
		{"comma-separated values", itemRequest("X-Mode: safe, fast\r\n"), 400, "VALIDATION", "header_mismatch"},
		{"optional header repeated", itemRequest("X-Mode: safe\r\nX-Mode: safe\r\n"), 400, "VALIDATION", "repeated_header"},
		{"required header of the write missing", strings.Replace(itemRequest(""), "Idempotency-Key: k-123456\r\n", "", 1), 400, "VALIDATION", "header_mismatch"},
		{"media type repeated", itemRequest("Content-Type: application/json\r\n"), 415, "BODY", "media_type"},
		{"other media type", strings.Replace(itemRequest(""), "application/json", "text/plain", 1), 415, "BODY", "media_type"},
	}
	before := e.a.count()
	for _, tc := range refused {
		status, body := e.rawExchange(tc.request, false)
		_, id := requestID(&http.Response{StatusCode: status}, body)
		ev := e.eventOf(id, 1)
		reason, _ := ev["reason"].(string)
		if status != tc.status || ev["stage"] != tc.stage || reason != tc.reason {
			t.Errorf("%s: %d %s, event %v, want %d at %s (%s)", tc.name, status, body, ev, tc.status, tc.stage, tc.reason)
		}
		t.Logf("%s: %d, %s at %s (%s)", tc.name, status, ev["kind"], ev["stage"], reason)
	}
	if e.a.count() != before || e.b.count()+e.c.count() != 0 {
		t.Fatalf("a refused request reached an upstream: receipts A %d -> %d", before, e.a.count())
	}
}

// A Connection field that names a header the operation validates, or
// Content-Type when it validates a body, would have the proxy remove that
// field after it was checked. Such a request is refused at VALIDATION before
// anything is forwarded, however the name is written, split, repeated or
// spaced, and whatever else the field names. Names that are not field names
// to the proxy, connection options and the gateway's own forwarding fields
// leave every validated value in place, and those requests are forwarded with
// the values checked.
func TestConnectionCannotRemoveValidatedFields(t *testing.T) {
	e := newEnv(t)
	e.serveNative(e.keywordPolicy(), forwardTimeouts)

	nominations := []struct {
		name, request string
	}{
		{"validated header", probeRequest("Connection: X-Probe\r\n")},
		{"after a connection option", probeRequest("Connection: close, X-Probe\r\n")},
		{"lower case", probeRequest("Connection: x-probe\r\n")},
		{"upper case", probeRequest("Connection: X-PROBE\r\n")},
		{"in a second field line", probeRequest("Connection: keep-alive\r\nConnection: X-Probe\r\n")},
		{"named twice", probeRequest("Connection: X-Probe, x-probe\r\n")},
		{"surrounded by spaces", probeRequest("Connection:   X-Probe  ,close\r\n")},
		{"surrounded by tabs", probeRequest("Connection: \tX-Probe\t\r\n")},
		{"between empty elements", probeRequest("Connection: ,, X-Probe ,,\r\n")},
		{"with an ordinary hop-by-hop field", probeRequest("Keep-Alive: timeout=5\r\nConnection: Keep-Alive, X-Probe\r\n")},
		{"with forwarding fields", probeRequest("Connection: X-Forwarded-For, Via, X-Probe\r\n")},
		{"required header of a write", itemRequest("Connection: Idempotency-Key\r\n")},
		{"optional header present", itemRequest("X-Mode: safe\r\nConnection: X-Mode\r\n")},
		{"optional header absent", itemRequest("Connection: x-mode\r\n")},
		{"media type of a validated body", itemRequest("Connection: Content-Type\r\n")},
		{"media type, other case, second line", itemRequest("Connection: close\r\nConnection: content-TYPE\r\n")},
	}
	for _, tc := range nominations {
		status, body := e.rawExchange(tc.request, false)
		_, id := requestID(&http.Response{StatusCode: status}, body)
		ev := e.eventOf(id, 1)
		reason, _ := ev["reason"].(string)
		if status != http.StatusBadRequest || !strings.Contains(body, `"bad_request"`) ||
			ev["kind"] != "request.rejected" || ev["stage"] != "VALIDATION" || reason != "connection_nominated" {
			t.Errorf("%s: %d %s, event %v", tc.name, status, body, ev)
		}
		t.Logf("%s: %d, %s at %s (%s), receipts A %d", tc.name, status, ev["kind"], ev["stage"], reason, e.a.count())
	}
	e.assertReceipts(0, 0, 0)

	forwarded := []struct {
		name, request string
		check         func(r receipt) bool
	}{
		{"connection options only", probeRequest("Connection: keep-alive, close\r\n"),
			func(r receipt) bool { return r.Headers.Get("X-Probe") == "on" }},
		{"another field named", probeRequest("X-Other: o\r\nConnection: X-Other\r\n"),
			func(r receipt) bool { return r.Headers.Get("X-Probe") == "on" && r.Headers.Get("X-Other") == "" }},
		{"a parameter after the name", probeRequest("Connection: X-Probe;q=1\r\n"),
			func(r receipt) bool { return r.Headers.Get("X-Probe") == "on" }},
		{"a quoted name", probeRequest("Connection: \"X-Probe\"\r\n"),
			func(r receipt) bool { return r.Headers.Get("X-Probe") == "on" }},
		{"a name with a space inside", probeRequest("Connection: X-Probe x\r\n"),
			func(r receipt) bool { return r.Headers.Get("X-Probe") == "on" }},
		{"media type of an operation without a body", probeRequest("Content-Type: text/plain\r\nConnection: Content-Type\r\n"),
			func(r receipt) bool { return r.Headers.Get("X-Probe") == "on" && r.Headers.Get("Content-Type") == "" }},
		{"the gateway's forwarding fields", probeRequest("X-Forwarded-For: 203.0.113.9\r\nVia: 1.0 spoof\r\nConnection: X-Forwarded-For, Via, X-Forwarded-Host, X-Forwarded-Proto\r\n"),
			func(r receipt) bool {
				return r.Headers.Get("X-Probe") == "on" && strings.Join(r.Headers.Values("X-Forwarded-For"), "|") == "127.0.0.1" &&
					strings.Join(r.Headers.Values("Via"), "|") == "1.1 gateway" && r.Headers.Get("X-Forwarded-Host") == "gw" &&
					r.Headers.Get("X-Forwarded-Proto") == "http"
			}},
		{"an unvalidated field of a write", itemRequest("X-Trace: t\r\nConnection: X-Trace\r\n"),
			func(r receipt) bool {
				return r.Body == keywordItem && r.Headers.Get("Content-Type") == "application/json" &&
					r.Headers.Get("Idempotency-Key") == "k-123456" && r.Headers.Get("X-Trace") == ""
			}},
	}
	for i, tc := range forwarded {
		status, body := e.rawExchange(tc.request, false)
		if status != http.StatusOK || e.a.count() != i+1 {
			t.Errorf("%s: %d %s with %d receipts, want 200 and %d", tc.name, status, body, e.a.count(), i+1)
			continue
		}
		got := e.a.receipts()[i]
		if !tc.check(got) {
			t.Errorf("%s: upstream received %v", tc.name, got.Headers)
		}
		t.Logf("%s: %d, upstream received X-Probe %q, Content-Type %q, Idempotency-Key %q, Connection %q, X-Forwarded-For %q, Via %q",
			tc.name, status, got.Headers.Get("X-Probe"), got.Headers.Get("Content-Type"), got.Headers.Get("Idempotency-Key"),
			got.Headers.Get("Connection"), got.Headers.Values("X-Forwarded-For"), got.Headers.Values("Via"))
	}

	// A gateway header named in Connection is refused at ingress, as before.
	if code, _ := e.raw(probeRequest("Connection: X-Gateway-Generation\r\n")); code != http.StatusBadRequest {
		t.Errorf("gateway header named in Connection: %d", code)
	}
	if e.a.count() != len(forwarded) || e.b.count()+e.c.count() != 0 {
		t.Fatalf("receipts A=%d B=%d C=%d, want %d at A only", e.a.count(), e.b.count(), e.c.count(), len(forwarded))
	}
}
