package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFetchArrayBodiesReachOnlyValidatedUpstream(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	p.Operations[1].Body = jsonBody(256, `{"type":"array","minItems":1,"maxItems":2,"items":{"type":"object","additionalProperties":false,"required":["item","quantity"],"properties":{"item":{"type":"string","minLength":7,"maxLength":7},"quantity":{"type":"integer","minimum":1,"maximum":50}}}}`)
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	for _, tc := range []struct {
		body, contentType string
		status            int
	}{
		{`[ {"item":"ITM-001", "quantity":2} ]`, "application/json", 200},
		{`[]`, "application/json", 400},
		{`[null]`, "application/json", 400},
		{`[{"item":"ITM-001","quantity":51}]`, "application/json", 400},
		{`[{"item":"ITM-001","quantity":2,"price_sen":1}]`, "application/json", 400},
		{`[{"item":"ITM-001","quantity":2,"quantity":1}]`, "application/json", 400},
		{`[{"item":"ITM-001","quantity":1},{"item":"ITM-002","quantity":1},{"item":"ITM-003","quantity":1}]`, "application/json", 400},
		{strings.Repeat(" ", 257), "application/json", 413},
		{`[]`, "text/plain", 415},
	} {
		before := len(e.a.receipts())
		code, body := e.request("POST", "/a/item", tc.contentType, tc.body, bearer(t, "alice")...)
		if code != tc.status {
			t.Errorf("array %s: %d %s", tc.body, code, body)
		}
		if tc.status == 200 {
			receipts := e.a.receipts()
			if len(receipts) != before+1 || receipts[len(receipts)-1].Body != tc.body {
				t.Errorf("validated representation changed: %+v", receipts)
			}
		} else if len(e.a.receipts()) != before {
			t.Errorf("invalid array was forwarded: %s", tc.body)
		}
	}
	e.assertReceipts(1, 0, 0)
}

func TestFetchArraySchemas(t *testing.T) {
	const items = `{"type":"array","minItems":1,"maxItems":2,"items":{"type":"object","additionalProperties":false,"required":["item","quantity"],"properties":{"item":{"type":"string","minLength":7,"maxLength":7},"quantity":{"type":"integer","minimum":1,"maximum":50}}}}`
	for _, wrapped := range []bool{false, true} {
		schema := items
		if wrapped {
			schema = `{"type":"object","additionalProperties":false,"required":["items"],"properties":{"items":` + items + `}}`
		}
		p := schemaPolicy(schema)
		g, err := Compile(p, 1, time.Time{})
		if err != nil {
			t.Errorf("wrapped=%t: array schema refused: %v", wrapped, err)
			continue
		}
		_, operation := g.match("/orders", "POST")
		for _, tc := range []struct {
			body string
			ok   bool
		}{
			{`[{"item":"ITM-001","quantity":1}]`, true},
			{`[{"item":"ITM-001","quantity":1},{"item":"ITM-002","quantity":50}]`, true},
			{`[]`, false},
			{`[{"item":"ITM-001","quantity":1},{"item":"ITM-001","quantity":1},{"item":"ITM-001","quantity":1}]`, false},
			{`[{"item":"ITM-001","quantity":0}]`, false},
			{`[{"item":"ITM-001","quantity":51}]`, false},
			{`[{"item":"ITM-001","quantity":1,"price_sen":1}]`, false},
			{`[{"item":"ITM-001"}]`, false},
			{`[null]`, false},
			{`{}`, false},
		} {
			body := tc.body
			if wrapped {
				body = `{"items":` + body + `}`
			}
			document, reason := decodeJSON([]byte(body))
			if reason != "" {
				t.Fatal(reason)
			}
			if got := validateRequest(nil, document, operation) == ""; got != tc.ok {
				t.Errorf("wrapped=%t body %s: valid=%t, want %t", wrapped, body, got, tc.ok)
			}
		}
	}
}

func TestFetchArraySchemaRefusals(t *testing.T) {
	for _, tc := range []struct{ schema, code string }{
		{`{"type":"array"}`, "INVALID_VALUE"},
		{`{"type":"array","items":true}`, "UNSUPPORTED_FEATURE"},
		{`{"type":"array","items":{"type":"string","pattern":"x"}}`, "UNSUPPORTED_FEATURE"},
		{`{"type":"array","items":{"type":"string"},"minItems":-1}`, "INVALID_VALUE"},
		{`{"type":"array","items":{"type":"string"},"maxItems":1.5}`, "INVALID_VALUE"},
		{`{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":1}`, "INVALID_VALUE"},
		{`{"type":"array","items":{"type":"string"},"maxItems":4097}`, "INVALID_VALUE"},
		{`{"type":"array","items":{"type":"string"},"uniqueItems":true}`, "UNSUPPORTED_FEATURE"},
		{`{"type":"object","additionalProperties":true,"items":{"type":"string"}}`, "INVALID_VALUE"},
	} {
		_, _, err := compileSchema(json.RawMessage(tc.schema), schemaBody, "body")
		if code := errorCode(err); code != tc.code {
			t.Errorf("%s: %v, want %s", tc.schema, err, tc.code)
		}
	}
	if _, _, err := compileSchema(json.RawMessage(`{"type":"array","items":{"type":"string"}}`), schemaHeaders, "headers"); err == nil {
		t.Fatal("header root accepted an array")
	}
}
