package gateway

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// schemaPolicy is threeAPIPolicy with the given body schema on orders.create.
func schemaPolicy(schema string) Policy {
	p := threeAPIPolicy()
	p.Operations[2].Body = jsonBody(1024, schema)
	return p
}

// Supported schemas compile into the operation's rule; anything outside the
// subset, or inconsistent, rejects the whole policy and is never ignored.
func TestSchemaCompilation(t *testing.T) {
	valid := []string{
		ordersCreateSchema,
		inventoryAdjustSchema,
		anyObject,
		`{"type":"object","additionalProperties":false}`,
		`{"type":"object","additionalProperties":false,"properties":{"note":{"type":["string","null"],"maxLength":8}}}`,
		`{"type":"object","additionalProperties":false,"properties":{"kind":{"type":"string","enum":["a","b"]},"n":{"type":"number","minimum":-1.5,"maximum":1e3}}}`,
		`{"type":"object","additionalProperties":true,"properties":{"meta":{"type":"object","additionalProperties":false,"required":["x"],"properties":{"x":{"type":"boolean"}}}}}`,
		`{"type":"object","additionalProperties":true,"properties":{"v":{"type":["integer","null"],"enum":[1,2,null]}}}`,
	}
	for _, schema := range valid {
		g, err := Compile(schemaPolicy(schema), 1, time.Time{})
		if err != nil {
			t.Errorf("schema %s rejected: %v", schema, err)
			continue
		}
		if _, r := g.match("/orders", "POST"); r.body == nil || r.body.schema == nil || r.body.maxBytes != 1024 {
			t.Errorf("schema %s not bound to its operation", schema)
		}
		if _, r := g.match("/orders", "GET"); r.body != nil {
			t.Errorf("schema %s bound to another operation", schema)
		}
	}

	field := "operations[2].body.schema"
	invalid := []struct {
		name, schema, code, field string
	}{
		{"missing", ``, "INVALID_VALUE", field},
		{"null", `null`, "INVALID_VALUE", field + ".type"},
		{"malformed", `{"type":"object"`, "SCHEMA_INVALID", field},
		{"repeated keyword", `{"type":"object","type":"string","additionalProperties":true}`, "SCHEMA_INVALID", field},
		{"boolean schema", `true`, "INVALID_VALUE", field + ".type"},
		{"root not an object", `{"type":"string"}`, "INVALID_VALUE", field + ".type"},
		{"root allowing null", `{"type":["object","null"],"additionalProperties":true}`, "INVALID_VALUE", field + ".type"},
		{"root without type", `{"additionalProperties":true}`, "INVALID_VALUE", field + ".type"},
		{"no additionalProperties", `{"type":"object"}`, "INVALID_VALUE", field + ".additionalProperties"},
		{"additionalProperties as a schema", `{"type":"object","additionalProperties":{"type":"string"}}`, "INVALID_VALUE", field + ".additionalProperties"},
		{"reference", `{"type":"object","additionalProperties":true,"$ref":"#/x"}`, "UNSUPPORTED_FEATURE", field + ".$ref"},
		{"external reference", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","$ref":"http://127.0.0.1:1/s.json"}}}`, "UNSUPPORTED_FEATURE", field + ".properties.a.$ref"},
		{"dialect", `{"$schema":"http://json-schema.org/draft-04/schema#","type":"object","additionalProperties":true}`, "UNSUPPORTED_FEATURE", field + ".$schema"},
		{"identifier", `{"$id":"urn:x","type":"object","additionalProperties":true}`, "UNSUPPORTED_FEATURE", field + ".$id"},
		{"definitions", `{"$defs":{},"type":"object","additionalProperties":true}`, "UNSUPPORTED_FEATURE", field + ".$defs"},
		{"pattern", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","pattern":"^a+$"}}}`, "UNSUPPORTED_FEATURE", field + ".properties.a.pattern"},
		{"format", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","format":"email"}}}`, "UNSUPPORTED_FEATURE", field + ".properties.a.format"},
		{"array without item schema", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"array"}}}`, "INVALID_VALUE", field + ".properties.a.items"},
		{"array keyword on an object", `{"type":"object","additionalProperties":true,"items":{}}`, "INVALID_VALUE", field + ".items"},
		{"combinator", `{"type":"object","additionalProperties":true,"anyOf":[]}`, "UNSUPPORTED_FEATURE", field + ".anyOf"},
		{"condition", `{"type":"object","additionalProperties":true,"if":{}}`, "UNSUPPORTED_FEATURE", field + ".if"},
		{"const", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","const":"x"}}}`, "UNSUPPORTED_FEATURE", field + ".properties.a.const"},
		{"exclusive bound", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"integer","exclusiveMinimum":0}}}`, "UNSUPPORTED_FEATURE", field + ".properties.a.exclusiveMinimum"},
		{"annotation", `{"type":"object","additionalProperties":true,"title":"x"}`, "UNSUPPORTED_FEATURE", field + ".title"},
		{"case variant keyword", `{"type":"object","additionalproperties":true}`, "UNSUPPORTED_FEATURE", field + ".additionalproperties"},
		{"unknown type", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"date"}}}`, "UNSUPPORTED_FEATURE", field + ".properties.a.type"},
		{"repeated type", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":["string","string"]}}}`, "INVALID_VALUE", field + ".properties.a.type"},
		{"empty type list", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":[]}}}`, "INVALID_VALUE", field + ".properties.a.type"},
		{"property schema not an object", `{"type":"object","additionalProperties":true,"properties":{"a":true}}`, "UNSUPPORTED_FEATURE", field + ".properties.a"},
		{"string keyword on a number", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"integer","maxLength":3}}}`, "INVALID_VALUE", field + ".properties.a.maxLength"},
		{"numeric keyword on a string", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","minimum":3}}}`, "INVALID_VALUE", field + ".properties.a.minimum"},
		{"object keyword on a string", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","properties":{}}}}`, "INVALID_VALUE", field + ".properties.a.properties"},
		{"required unknown property", `{"type":"object","additionalProperties":true,"required":["a"]}`, "INVALID_VALUE", field + ".required"},
		{"required twice", `{"type":"object","additionalProperties":true,"required":["a","a"],"properties":{"a":{"type":"string"}}}`, "INVALID_VALUE", field + ".required"},
		{"required not a list", `{"type":"object","additionalProperties":true,"required":"a","properties":{"a":{"type":"string"}}}`, "INVALID_VALUE", field + ".required"},
		{"negative length", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","minLength":-1}}}`, "INVALID_VALUE", field + ".properties.a.minLength"},
		{"fractional length", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","maxLength":1.5}}}`, "INVALID_VALUE", field + ".properties.a.maxLength"},
		{"length above the bound", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","maxLength":1048577}}}`, "INVALID_VALUE", field + ".properties.a.maxLength"},
		{"inverted lengths", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","minLength":5,"maxLength":4}}}`, "INVALID_VALUE", field + ".properties.a.minLength"},
		{"inverted range", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"integer","minimum":5,"maximum":4.5}}}`, "INVALID_VALUE", field + ".properties.a.minimum"},
		{"bound as text", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"integer","minimum":"1"}}}`, "INVALID_VALUE", field + ".properties.a.minimum"},
		{"unbounded number literal", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"integer","maximum":1e99999}}}`, "SCHEMA_INVALID", field},
		{"empty enum", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string","enum":[]}}}`, "INVALID_VALUE", field + ".properties.a.enum"},
		{"enum of objects", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"object","additionalProperties":true,"enum":[{}]}}}`, "UNSUPPORTED_FEATURE", field + ".properties.a.enum"},
		{"too many enum values", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"integer","enum":[` + numbers(maxEnumValues+1) + `]}}}`, "INVALID_VALUE", field + ".properties.a.enum"},
		{"too many properties", `{"type":"object","additionalProperties":true,"properties":{` + properties(maxSchemaProperties+1) + `}}`, "INVALID_VALUE", field + ".properties"},
	}
	for _, tc := range invalid {
		started := time.Now()
		g, err := Compile(schemaPolicy(tc.schema), 1, time.Time{})
		policyErr, _ := err.(*PolicyError)
		if g != nil || policyErr == nil || policyErr.Code != tc.code || policyErr.Field != tc.field {
			t.Errorf("%s: generation %v error %v, want %s at %s", tc.name, g != nil, err, tc.code, tc.field)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("%s: took %v", tc.name, elapsed)
		}
	}

	headerField := "operations[2].headers"
	headers := []struct {
		name, schema, code, field string
	}{
		{"canonical name", `{"type":"object","additionalProperties":true,"required":["Idempotency-Key"],"properties":{"Idempotency-Key":{"type":"string","minLength":8}}}`, "", ""},
		{"lower-case name", `{"type":"object","additionalProperties":true,"properties":{"idempotency-key":{"type":"string"}}}`, "INVALID_VALUE", headerField + ".properties.idempotency-key"},
		{"credential header", `{"type":"object","additionalProperties":true,"properties":{"Authorization":{"type":"string"}}}`, "INVALID_VALUE", headerField + ".properties.Authorization"},
		{"gateway header", `{"type":"object","additionalProperties":true,"properties":{"X-Gateway-Subject":{"type":"string"}}}`, "INVALID_VALUE", headerField + ".properties.X-Gateway-Subject"},
		{"request identifier", `{"type":"object","additionalProperties":true,"properties":{"X-Request-Id":{"type":"string"}}}`, "INVALID_VALUE", headerField + ".properties.X-Request-Id"},
		{"invalid name character", `{"type":"object","additionalProperties":true,"properties":{"X_Key":{"type":"string"}}}`, "INVALID_VALUE", headerField + ".properties.X_Key"},
		{"non-string value", `{"type":"object","additionalProperties":true,"properties":{"X-Count":{"type":"integer"}}}`, "INVALID_VALUE", headerField + ".properties.X-Count.type"},
		{"unsupported keyword", `{"type":"object","additionalProperties":true,"properties":{"X-Key":{"type":"string","pattern":"x"}}}`, "UNSUPPORTED_FEATURE", headerField + ".properties.X-Key.pattern"},
		{"media type", `{"type":"object","additionalProperties":true,"required":["Content-Type"],"properties":{"Content-Type":{"type":"string"}}}`, "", ""},
		{"end-to-end fields", `{"type":"object","additionalProperties":true,"properties":{"Accept":{"type":"string"},"Expect":{"type":"string"},"Cookie":{"type":"string"},"User-Agent":{"type":"string"}}}`, "", ""},
	}
	// Fields whose validated value the upstream would not receive: hop-by-hop
	// fields, which the proxy removes; Host and the body length, which
	// net/http keeps outside the header map or recomputes; the forwarding
	// fields, which the proxy removes, replaces or extends.
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te",
		"Trailer", "Transfer-Encoding", "Upgrade", "Host", "Content-Length", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host",
		"X-Forwarded-Proto", "Via"} {
		headers = append(headers, struct{ name, schema, code, field string }{
			name + " not forwarded as validated",
			`{"type":"object","additionalProperties":true,"properties":{"` + name + `":{"type":"string"}}}`,
			"INVALID_VALUE", headerField + ".properties." + name,
		})
	}
	for _, tc := range headers {
		p := threeAPIPolicy()
		p.Operations[2].Headers = json.RawMessage(tc.schema)
		_, err := Compile(p, 1, time.Time{})
		policyErr, _ := err.(*PolicyError)
		if tc.code == "" && err != nil || tc.code != "" && (policyErr == nil || policyErr.Code != tc.code || policyErr.Field != tc.field) {
			t.Errorf("%s: %v, want %q at %s", tc.name, err, tc.code, tc.field)
		}
	}

	// Through the document decoder: schema keywords and property names are
	// case-sensitive and kept as written, while member names outside a schema
	// keep the lower-case rule.
	document := func(schema string) string {
		return `{"schema_version":4,"profile":"core","upstreams":[{"id":"u","dial":"127.0.0.1:1"}],"apis":[{"id":"a","upstream":"u"}],` +
			`"operations":[{"id":"x","api":"a","method":"POST","target":"/x","access":"allow","auth":"none","body":{"max_bytes":100,"schema":` + schema + `}}]}`
	}
	documents := []struct {
		name, document, code string
	}{
		{"camel-case keywords", document(`{"type":"object","additionalProperties":false,"properties":{"MixedCase":{"type":"string","maxLength":3}}}`), ""},
		{"repeated schema member", document(`{"type":"object","additionalProperties":false,"additionalProperties":true}`), "DUPLICATE_MEMBER"},
		{"repeated property name", document(`{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string"},"a":{"type":"integer"}}}`), "DUPLICATE_MEMBER"},
		{"upper-case member outside a schema", strings.Replace(document(anyObject), `"max_bytes"`, `"Max_bytes"`, 1), "UNKNOWN_FIELD"},
		// nested(n) places its innermost schema at policy depth 5+2n.
		{"nesting above the policy bound", document(nested(6)), "LIMIT_EXCEEDED"},
		{"nesting within the bound", document(nested(5)), ""},
	}
	for _, tc := range documents {
		if got := errorCode(compileJSON(t, tc.document)); got != tc.code {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.code)
		}
	}
}

func numbers(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprint(i)
	}
	return strings.Join(items, ",")
}

func properties(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`"p%d":{"type":"string"}`, i)
	}
	return strings.Join(items, ",")
}

// nested returns an object schema with object properties nested levels deep.
func nested(levels int) string {
	schema := `{"type":"string"}`
	for range levels {
		schema = `{"type":"object","additionalProperties":false,"properties":{"n":` + schema + `}}`
	}
	return schema
}

// The body decoder accepts exactly one bounded, unambiguous JSON value.
func TestBodyDecoding(t *testing.T) {
	deep := func(n int) string { return strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n) }
	cases := []struct {
		name, body, reason string
	}{
		{"object", `{"a":1,"b":[true,null,"x"],"c":{"d":-1.5e3}}`, ""},
		{"surrounding whitespace", " \r\n\t{} \n", ""},
		{"escaped control character", `{"a":"\u0000\n"}`, ""},
		{"surrogate pair", `{"a":"😀"}`, ""},
		{"escaped backslash before u", `{"a":"\\ud800"}`, ""},
		{"empty", ``, "malformed_json"},
		{"whitespace only", "  ", "malformed_json"},
		{"truncated", `{"a":`, "malformed_json"},
		{"single quotes", `{'a':1}`, "malformed_json"},
		{"comment", `{"a":1}/*x*/`, "trailing_data"},
		{"raw control character", "{\"a\":\"\x01\"}", "malformed_json"},
		{"not a number", `{"a":NaN}`, "malformed_json"},
		{"byte order mark", "\xef\xbb\xbf{}", "malformed_json"},
		{"second value", `{}{}`, "trailing_data"},
		{"trailing text", `{} x`, "trailing_data"},
		{"trailing brace", `{}}`, "trailing_data"},
		{"repeated member", `{"a":1,"a":2}`, "duplicate_member"},
		{"repeated nested member", `{"a":{"b":1,"b":1}}`, "duplicate_member"},
		{"names differing in case", `{"a":1,"A":2}`, ""},
		{"invalid UTF-8", "{\"a\":\"\xff\"}", "invalid_encoding"},
		{"overlong UTF-8", "{\"a\":\"\xc0\xaf\"}", "invalid_encoding"},
		{"lone high surrogate", `{"a":"\ud800"}`, "invalid_encoding"},
		{"lone low surrogate", `{"a":"\udc00x"}`, "invalid_encoding"},
		{"reversed pair", `{"a":"\udc00\ud800"}`, "invalid_encoding"},
		{"nesting at the bound", deep(maxJSONDepth), ""},
		{"nesting above the bound", deep(maxJSONDepth + 1), "limit_exceeded"},
		{"values at the bound", "[" + strings.TrimSuffix(strings.Repeat("1,", maxJSONValues-1), ",") + "]", ""},
		{"values above the bound", "[" + strings.TrimSuffix(strings.Repeat("1,", maxJSONValues), ",") + "]", "limit_exceeded"},
		{"long number", `{"a":` + strings.Repeat("1", maxNumberLength+1) + `}`, "limit_exceeded"},
		{"large exponent", `{"a":1e999}`, ""},
		{"exponent above the bound", `{"a":1e1000}`, "limit_exceeded"},
		{"negative exponent above the bound", `{"a":1E-1000}`, "limit_exceeded"},
	}
	for _, tc := range cases {
		started := time.Now()
		_, reason := decodeJSON([]byte(tc.body))
		if reason != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.name, reason, tc.reason)
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Errorf("%s: took %v", tc.name, elapsed)
		}
	}

	// The decoded value keeps numbers exact and every member.
	value, _ := decodeJSON([]byte(`{"n":12345678901234567890,"e":[],"o":{}}`))
	object := value.(map[string]any)
	if object["n"] != json.Number("12345678901234567890") || len(object["e"].([]any)) != 0 || len(object["o"].(map[string]any)) != 0 {
		t.Fatalf("decoded %#v", value)
	}
}

func TestMediaTypes(t *testing.T) {
	cases := []struct {
		fields []string
		ok     bool
	}{
		{[]string{"application/json"}, true},
		{[]string{"Application/JSON"}, true},
		{[]string{"application/json; charset=utf-8"}, true},
		{[]string{"application/json;charset=UTF-8"}, true},
		{[]string{"application/json; charset=\"utf-8\""}, true},
		{nil, false},
		{[]string{""}, false},
		{[]string{"application/json", "application/json"}, false},
		{[]string{"application/json; charset=latin1"}, false},
		{[]string{"application/json; charset=utf-16"}, false},
		{[]string{"application/json; boundary=x"}, false},
		// mime.ParseMediaType accepts a parameter repeated with the same
		// value, which is unambiguous, and refuses conflicting repeats.
		{[]string{"application/json; charset=utf-8; charset=utf-8"}, true},
		{[]string{"application/json; charset=utf-8; charset=latin1"}, false},
		{[]string{"application/json; charset=latin1; charset=utf-8"}, false},
		{[]string{"application/json;;"}, false},
		{[]string{"application/json, text/plain"}, false},
		{[]string{"text/json"}, false},
		{[]string{"text/plain"}, false},
		{[]string{"application/x-www-form-urlencoded"}, false},
		{[]string{"application/json-patch+json"}, false},
		{[]string{"application/problem+json"}, false},
		{[]string{"multipart/form-data; boundary=x"}, false},
	}
	for _, tc := range cases {
		if got := isJSONMediaType(tc.fields); got != tc.ok {
			t.Errorf("%q: %v, want %v", tc.fields, got, tc.ok)
		}
	}
}

// rawExchange sends a request exactly as given and returns the status and
// body of the reply. With halfClose the client then closes its sending side,
// so a body shorter than its declared length ends early.
func (e *env) rawExchange(request string, halfClose bool) (int, string) {
	e.t.Helper()
	conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		e.t.Fatal(err)
	}
	if halfClose {
		conn.(*net.TCPConn).CloseWrite()
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// Every invalid write is refused before the fixture: no receipt anywhere, no
// state change, and an event at the stage that refused it. Valid writes,
// including boundary values, reach the intended fixture with the exact bytes
// sent.
func TestValidationThroughGateway(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	orders, inventory := fixtures["orders"], fixtures["inventory"]
	customer := "Authorization: Bearer " + issuer.tokenFor(t, "cust-001") + "\r\n"
	staff := "Authorization: Bearer " + issuer.tokenFor(t, "staff-01") + "\r\n"

	request := func(method, path, credential, headers, body string) string {
		return fmt.Sprintf("%s %s HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n%s%sContent-Length: %d\r\n\r\n%s",
			method, path, credential, headers, len(body), body)
	}
	create := func(headers, body string) string { return request("POST", "/orders", customer, headers, body) }
	adjust := func(body string) string {
		return request("PATCH", "/inventory/items/ITM-001", staff, "Content-Type: application/json\r\n", body)
	}
	jsonType := "Content-Type: application/json\r\n"
	order := func(fields string) string {
		return create(jsonType, `{"customer":"cust-001","item":"ITM-001",`+fields+`}`)
	}

	refused := []struct {
		name, request string
		halfClose     bool
		status        int
		stage, reason string
	}{
		{"malformed JSON", create(jsonType, `{"cu`), false, 400, "BODY", "malformed_json"},
		{"empty body", create(jsonType, ``), false, 400, "BODY", "malformed_json"},
		{"truncated body", strings.Replace(create(jsonType, `{"customer":"cust-001"`), "Content-Length: 22", "Content-Length: 60", 1), true, 400, "BODY", "read_failed"},
		{"second JSON value", order(`"quantity":1}{"quantity":40`), false, 400, "BODY", "trailing_data"},
		{"trailing text", create(jsonType, `{"customer":"cust-001","item":"ITM-001","quantity":1} x`), false, 400, "BODY", "trailing_data"},
		{"invalid UTF-8", create(jsonType, "{\"customer\":\"cust-001\xff\",\"item\":\"ITM-001\",\"quantity\":1}"), false, 400, "BODY", "invalid_encoding"},
		{"lone surrogate", order(`"quantity":1,"x":"\ud800"`), false, 400, "BODY", "invalid_encoding"},
		{"repeated member", order(`"quantity":1,"quantity":40`), false, 400, "BODY", "duplicate_member"},
		{"nesting above the bound", create(jsonType, strings.Repeat(`{"a":`, 17)+"1"+strings.Repeat("}", 17)), false, 400, "BODY", "limit_exceeded"},
		{"unbounded number", order(`"quantity":1e1000`), false, 400, "BODY", "limit_exceeded"},
		{"no media type", create("", `{"customer":"cust-001","item":"ITM-001","quantity":1}`), false, 415, "BODY", "media_type"},
		{"text media type", create("Content-Type: text/plain\r\n", `{}`), false, 415, "BODY", "media_type"},
		{"other charset", create("Content-Type: application/json; charset=iso-8859-1\r\n", `{}`), false, 415, "BODY", "media_type"},
		{"unknown parameter", create("Content-Type: application/json; v=2\r\n", `{}`), false, 415, "BODY", "media_type"},
		{"malformed media type", create("Content-Type: application/json;;\r\n", `{}`), false, 415, "BODY", "media_type"},
		{"two media types", create(jsonType+jsonType, `{}`), false, 415, "BODY", "media_type"},
		{"declared length above the limit", create(jsonType, strings.Repeat(" ", 1025)), false, 413, "BODY", "too_large"},
		{"chunked body above the limit", "POST /orders HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n" + customer + jsonType +
			"Transfer-Encoding: chunked\r\n\r\n401\r\n" + strings.Repeat(" ", 1025) + "\r\n0\r\n\r\n", false, 413, "BODY", "too_large"},
		{"missing field", create(jsonType, `{"customer":"cust-001","quantity":1}`), false, 400, "VALIDATION", "schema_mismatch"},
		{"extra field", order(`"quantity":1,"price":0`), false, 400, "VALIDATION", "schema_mismatch"},
		{"quantity as text", order(`"quantity":"1"`), false, 400, "VALIDATION", "schema_mismatch"},
		{"fractional quantity", order(`"quantity":1.5`), false, 400, "VALIDATION", "schema_mismatch"},
		{"null quantity", order(`"quantity":null`), false, 400, "VALIDATION", "schema_mismatch"},
		{"object as customer", create(jsonType, `{"customer":{"id":"cust-001"},"item":"ITM-001","quantity":1}`), false, 400, "VALIDATION", "schema_mismatch"},
		{"empty customer", create(jsonType, `{"customer":"","item":"ITM-001","quantity":1}`), false, 400, "VALIDATION", "schema_mismatch"},
		{"customer above the length bound", create(jsonType, `{"customer":"`+strings.Repeat("c", 65)+`","item":"ITM-001","quantity":1}`), false, 400, "VALIDATION", "schema_mismatch"},
		{"array root", create(jsonType, `[{"customer":"cust-001","item":"ITM-001","quantity":1}]`), false, 400, "VALIDATION", "schema_mismatch"},
		{"null root", create(jsonType, `null`), false, 400, "VALIDATION", "schema_mismatch"},
		{"string root", create(jsonType, `"order"`), false, 400, "VALIDATION", "schema_mismatch"},
		{"empty object", create(jsonType, `{}`), false, 400, "VALIDATION", "schema_mismatch"},
		{"delta above the range", adjust(`{"delta":101}`), false, 400, "VALIDATION", "schema_mismatch"},
		{"delta below the range", adjust(`{"delta":-101}`), false, 400, "VALIDATION", "schema_mismatch"},
		{"body on a bodyless operation", request("GET", "/orders", staff, jsonType, `{}`), false, 400, "BODY", "unexpected_body"},
	}
	states := map[string]string{"orders": orders.state().SHA256, "inventory": inventory.state().SHA256}
	for _, tc := range refused {
		status, body := e.rawExchange(tc.request, tc.halfClose)
		var reply struct {
			Error     string `json:"error"`
			RequestID string `json:"request_id"`
		}
		json.Unmarshal([]byte(body), &reply)
		if status != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, status, tc.status, body)
			continue
		}
		events := e.requestEvents(reply.RequestID, 1)
		if len(events) != 1 || events[0]["kind"] != "request.rejected" || events[0]["stage"] != tc.stage || events[0]["reason"] != tc.reason {
			t.Errorf("%s: events %v, want a %s rejection with reason %s", tc.name, events, tc.stage, tc.reason)
		}
		// The client sees only the code, never the input or the validator's
		// message.
		if strings.Contains(body, "cust-001") || strings.Contains(body, "quantity") || strings.Contains(body, "schema") {
			t.Errorf("%s: reply %s echoes input or validator detail", tc.name, body)
		}
	}
	for name, n := range receiptCounts(fixtures) {
		if n != 0 {
			t.Errorf("fixture %s received %d refused requests", name, n)
		}
	}
	if orders.state().SHA256 != states["orders"] || inventory.state().SHA256 != states["inventory"] {
		t.Fatal("a refused request changed fixture state")
	}

	// Accepted requests: the fixture receives exactly the bytes sent, with a
	// matching Content-Length, and runs the operation.
	accepted := []struct {
		name, request, body, fixture, effect string
		status                               int
	}{
		{"valid order", "", `{"customer":"cust-001","item":"ITM-002","quantity":2}`, "orders", "created ORD-1004", 201},
		{"layout and charset kept", "charset", "{ \"customer\" : \"cust-001\",\n \"item\":\"ITM-003\", \"quantity\" : 1 }\n", "orders", "created ORD-1005", 201},
		// Forwarded: the fixture's own ownership rule then refuses it.
		{"customer at the length bound", "", `{"customer":"` + strings.Repeat("c", 64) + `","item":"ITM-001","quantity":1}`, "orders", "", 403},
		{"delta at the upper bound", "adjust", `{"delta":100}`, "inventory", "ITM-001 stock 40->140", 200},
		{"delta at the lower bound", "adjust", `{"delta":-100}`, "inventory", "ITM-001 stock 140->40", 200},
	}
	for _, tc := range accepted {
		var raw string
		switch tc.request {
		case "adjust":
			raw = adjust(tc.body)
		case "charset":
			raw = create("Content-Type: Application/JSON; Charset=UTF-8\r\n", tc.body)
		default:
			raw = create(jsonType, tc.body)
		}
		status, reply := e.rawExchange(raw, false)
		target := fixtures[tc.fixture]
		receipts := target.receipts()
		if status != tc.status || len(receipts) == 0 {
			t.Fatalf("%s: status %d (%s), %d receipts", tc.name, status, reply, len(receipts))
		}
		last := receipts[len(receipts)-1]
		if last.BodySHA256 != sha256Hex(tc.body) || last.BodyBytes != len(tc.body) || last.Effect != tc.effect {
			t.Errorf("%s: fixture received %d bytes %s effect %q, want %d bytes %s effect %q",
				tc.name, last.BodyBytes, last.BodySHA256, last.Effect, len(tc.body), sha256Hex(tc.body), tc.effect)
		}
	}
	if counts := receiptCounts(fixtures); counts["orders"] != 3 || counts["inventory"] != 2 || counts["reports"] != 0 {
		t.Fatalf("receipts %v, want orders 3, inventory 2", counts)
	}
}

// keywordPolicy is the receiver test policy with a richer schema and header
// schema on POST /a/item, a header-only bodyless operation, and a large-body
// operation for decoder limits.
func (e *env) keywordPolicy() Policy {
	p := e.policy(e.a, false)
	for i := range p.Operations {
		if p.Operations[i].ID == "write-a" {
			p.Operations[i].Body = jsonBody(1024, `{"type":"object","additionalProperties":false,"required":["kind","count"],"properties":{`+
				`"kind":{"type":"string","enum":["alpha","beta"]},`+
				`"count":{"type":"integer","minimum":1,"maximum":5},`+
				`"note":{"type":["string","null"],"maxLength":8},`+
				`"meta":{"type":"object","additionalProperties":false,"properties":{"tag":{"type":"boolean"}}}}}`)
			p.Operations[i].Headers = json.RawMessage(`{"type":"object","additionalProperties":true,"required":["Idempotency-Key"],"properties":{` +
				`"Idempotency-Key":{"type":"string","minLength":8,"maxLength":64},"X-Mode":{"type":"string","enum":["safe","fast"]}}}`)
		}
	}
	p.Operations = append(p.Operations,
		Operation{ID: "probe", API: "a", Method: "GET", Target: "/a/probe", Access: "allow", Auth: "none",
			Headers: json.RawMessage(`{"type":"object","additionalProperties":true,"required":["X-Probe"],"properties":{"X-Probe":{"type":"string","enum":["on"]}}}`)},
		Operation{ID: "bulk", API: "a", Method: "POST", Target: "/a/bulk", Access: "allow", Auth: "none", Body: jsonBody(16000, anyObject)},
	)
	return p
}

// Enumerations, ranges, lengths, null unions, nested objects and declared
// headers behave as the schema states, with the valid boundary admitted; the
// decoder limits hold at their bounds.
func TestSchemaKeywordsThroughGateway(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.keywordPolicy(), seedOptions{})
	e.start(binaries.plain)

	key := "Idempotency-Key: k-123456\r\n"
	item := func(headers, body string) string {
		return fmt.Sprintf("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\n%sContent-Length: %d\r\n\r\n%s", headers, len(body), body)
	}
	cases := []struct {
		name, request string
		status        int
		reason        string
	}{
		{"valid", item(key, `{"kind":"alpha","count":1}`), 200, ""},
		{"all fields at their bounds", item(key+"X-Mode: fast\r\n", `{"kind":"beta","count":5,"note":"12345678","meta":{"tag":false}}`), 200, ""},
		{"null where allowed", item(key, `{"kind":"alpha","count":3,"note":null}`), 200, ""},
		{"integer written with a fraction", item(key, `{"kind":"alpha","count":2.0}`), 200, ""},
		{"header name in another case", item("idempotency-KEY: k-123456\r\n", `{"kind":"alpha","count":1}`), 200, ""},
		{"value outside the enumeration", item(key, `{"kind":"gamma","count":1}`), 400, "schema_mismatch"},
		{"enumeration in another case", item(key, `{"kind":"Alpha","count":1}`), 400, "schema_mismatch"},
		{"count below the range", item(key, `{"kind":"alpha","count":0}`), 400, "schema_mismatch"},
		{"count above the range", item(key, `{"kind":"alpha","count":6}`), 400, "schema_mismatch"},
		{"count as boolean", item(key, `{"kind":"alpha","count":true}`), 400, "schema_mismatch"},
		{"note above the length bound", item(key, `{"kind":"alpha","count":1,"note":"123456789"}`), 400, "schema_mismatch"},
		{"note as number", item(key, `{"kind":"alpha","count":1,"note":7}`), 400, "schema_mismatch"},
		{"nested unknown field", item(key, `{"kind":"alpha","count":1,"meta":{"tag":true,"x":1}}`), 400, "schema_mismatch"},
		{"nested wrong type", item(key, `{"kind":"alpha","count":1,"meta":{"tag":"yes"}}`), 400, "schema_mismatch"},
		{"missing required header", item("", `{"kind":"alpha","count":1}`), 400, "header_mismatch"},
		{"header below the length bound", item("Idempotency-Key: short\r\n", `{"kind":"alpha","count":1}`), 400, "header_mismatch"},
		{"header outside its enumeration", item(key+"X-Mode: slow\r\n", `{"kind":"alpha","count":1}`), 400, "header_mismatch"},
		{"repeated declared header", item(key+key, `{"kind":"alpha","count":1}`), 400, "repeated_header"},
		{"header-only operation without the header", "GET /a/probe HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n", 400, "header_mismatch"},
		{"header-only operation", "GET /a/probe HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nX-Probe: on\r\n\r\n", 200, ""},
		{"values at the decoder bound", bulk(maxJSONValues - 1), 200, ""},
		{"values above the decoder bound", bulk(maxJSONValues), 400, "limit_exceeded"},
	}
	forwarded := 0
	for _, tc := range cases {
		started := time.Now()
		status, body := e.rawExchange(tc.request, false)
		elapsed := time.Since(started)
		if status != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, status, tc.status, body)
			continue
		}
		if elapsed > 2*time.Second {
			t.Errorf("%s: took %v", tc.name, elapsed)
		}
		if tc.status == 200 {
			forwarded++
			continue
		}
		_, id := requestID(&http.Response{StatusCode: status}, body)
		if events := e.requestEvents(id, 1); len(events) != 1 || events[0]["reason"] != tc.reason {
			t.Errorf("%s: events %v, want reason %s", tc.name, events, tc.reason)
		}
	}
	e.assertReceipts(forwarded, 0, 0)
}

// bulk is a request for /a/bulk whose body holds values JSON values: an
// object with one array member.
func bulk(values int) string {
	body := `{"v":[` + strings.TrimSuffix(strings.Repeat("1,", values-1), ",") + `]}`
	return fmt.Sprintf("POST /a/bulk HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

// The proxy removes every field the client names in Connection, as an HTTP
// intermediary must, so a request naming a header the operation validates,
// or Content-Type when it validates a body, is refused before it is
// forwarded: the upstream would not receive what was checked. The same
// request without the nomination is forwarded with the header.
func TestConnectionCannotRemoveValidatedHeaders(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.keywordPolicy(), seedOptions{})
	e.start(binaries.plain)

	probe := func(connection string) string {
		return "GET /a/probe HTTP/1.1\r\nHost: gw\r\n" + connection + "X-Probe: on\r\n\r\n"
	}
	item := func(connection string) string {
		body := `{"kind":"alpha","count":1}`
		return fmt.Sprintf("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: %s\r\nContent-Type: application/json\r\nIdempotency-Key: k-123456\r\nContent-Length: %d\r\n\r\n%s",
			connection, len(body), body)
	}
	if status, body := e.rawExchange(probe("Connection: close\r\n"), false); status != http.StatusOK || e.a.count() != 1 || e.a.receipts()[0].Headers.Get("X-Probe") != "on" {
		t.Fatalf("validated header not named: status %d (%s), receipts %+v", status, body, e.a.receipts())
	}
	cases := []struct {
		name, request string
	}{
		{"validated header named", probe("Connection: close, X-Probe\r\n")},
		{"named in a second field and another case", probe("Connection: close\r\nConnection: x-probe\r\n")},
		{"required header named", item("close, Idempotency-Key")},
		{"media type named", item("close, Content-Type")},
	}
	for _, tc := range cases {
		status, body := e.rawExchange(tc.request, false)
		_, id := requestID(&http.Response{StatusCode: status}, body)
		events := e.requestEvents(id, 1)
		if status != http.StatusBadRequest || len(events) != 1 || events[0]["stage"] != "VALIDATION" || events[0]["reason"] != "connection_nominated" {
			t.Errorf("%s: status %d (%s), events %v", tc.name, status, body, events)
		}
	}
	e.assertReceipts(1, 0, 0)
}

// sizedBody returns a JSON object of exactly size bytes whose only member
// starts with tag, so that concurrent bodies can be told apart.
func sizedBody(tag string, size int) string {
	return `{"p":"` + tag + strings.Repeat("x", size-8-len(tag)) + `"}`
}

// chunkedBody encodes body in chunks of at most size bytes, followed by the
// last chunk.
func chunkedBody(body string, size int) string {
	var b strings.Builder
	for len(body) > 0 {
		n := min(size, len(body))
		fmt.Fprintf(&b, "%x\r\n%s\r\n", n, body[:n])
		body = body[n:]
	}
	b.WriteString("0\r\n\r\n")
	return b.String()
}

// sendBody posts a JSON body to path with the named framing and returns the
// status and reply. "declared" and "chunked" are HTTP/1.1 requests written
// byte for byte, the chunked body in chunks of at most 4096 bytes. header
// holds name/value pairs.
func (e *env) sendBody(framing, path, body string, header ...string) (int, string) {
	e.t.Helper()
	head := "POST " + path + " HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\n"
	for i := 0; i+1 < len(header); i += 2 {
		head += header[i] + ": " + header[i+1] + "\r\n"
	}
	if framing == "declared" {
		return e.rawExchange(fmt.Sprintf("%sContent-Length: %d\r\n\r\n%s", head, len(body), body), false)
	}
	return e.rawExchange(head+"Transfer-Encoding: chunked\r\n\r\n"+chunkedBody(body, 4096), false)
}

// bodyFramings are the request framings the data plane serves for a body:
// HTTP/1.1 with a declared length or chunked.
var bodyFramings = []string{"declared", "chunked"}

// sizeRefusal returns the stage and reason expected when a body one byte over
// the effective ceiling is refused: a declared length above the global
// ceiling is refused at ingress, before any policy work; every other excess
// when the body is read.
func sizeRefusal(framing string, size, global int) (string, string) {
	if framing == "declared" && size > global {
		return "INGRESS", ""
	}
	return "BODY", "too_large"
}

// The effective body ceiling is the smaller of the global ceiling (16384
// bytes in the test configuration) and the operation's own, and it bounds the
// bytes actually read whatever the framing: a declared length or a chunked
// body. A body of exactly the ceiling reaches the upstream whole, with a
// declared length; one byte more is refused with 413 and reaches no upstream.
func TestBodyCeilingAppliesToActualBytes(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.Operations = append(p.Operations,
		Operation{ID: "large", API: "a", Method: "POST", Target: "/a/large", Access: "allow", Auth: "none", Body: jsonBody(32768, anyObject)},
		Operation{ID: "small", API: "a", Method: "POST", Target: "/a/small", Access: "allow", Auth: "none", Body: jsonBody(8192, anyObject)},
		Operation{ID: "equal", API: "a", Method: "POST", Target: "/a/equal", Access: "allow", Auth: "none", Body: jsonBody(16384, anyObject)},
	)
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	const global = 16384
	routed := []string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS"}

	operations := []struct {
		name, path string
		ceiling    int
	}{
		{"global below the operation", "/a/large", global},
		{"operation below the global", "/a/small", 8192},
		{"equal ceilings", "/a/equal", global},
	}
	forwarded := 0
	for _, op := range operations {
		for _, framing := range bodyFramings {
			for _, size := range []int{op.ceiling, op.ceiling + 1} {
				name := fmt.Sprintf("%s, %s, %d bytes", op.name, framing, size)
				body := sizedBody("", size)
				status, reply := e.sendBody(framing, op.path, body)
				if size == op.ceiling {
					forwarded++
					receipts := e.a.receipts()
					if status != http.StatusOK || len(receipts) != forwarded {
						t.Fatalf("%s: status %d (%s), %d receipts, want 200 and %d", name, status, reply, len(receipts), forwarded)
					}
					last := receipts[forwarded-1]
					if last.Body != body || last.Headers.Get("Content-Length") != strconv.Itoa(size) || len(last.Trailer) != 0 {
						t.Errorf("%s: upstream received %d bytes (intact %v) with Content-Length %q",
							name, len(last.Body), last.Body == body, last.Headers.Get("Content-Length"))
					}
					continue
				}

				stage, reason := sizeRefusal(framing, size, global)
				want := expectedStages(nil, "INGRESS", stageReject)
				if stage == "BODY" {
					want = public(expectedStages(routed, "BODY", stageReject))
				}
				_, id := requestID(&http.Response{StatusCode: status}, reply)
				events := e.requestEvents(id, 1)
				if status != http.StatusRequestEntityTooLarge || len(events) != 1 || events[0]["kind"] != "request.rejected" ||
					events[0]["stage"] != stage || events[0]["status"] != float64(413) || events[0]["error"] != "payload_too_large" ||
					!sameStages(events[0]["stages"], want) {
					t.Errorf("%s: status %d (%s), events %v, want 413 at %s", name, status, reply, events, stage)
				} else if got, _ := events[0]["reason"].(string); got != reason {
					t.Errorf("%s: reason %q, want %q", name, got, reason)
				}
				if n := e.a.count(); n != forwarded {
					t.Fatalf("%s: %d receipts, want %d: a refused body was forwarded", name, n, forwarded)
				}
			}
		}
	}

	// A chunked body that announces more than the ceiling and stalls one byte
	// past it is refused at once: the read stops at the ceiling instead of
	// waiting for the rest, and the connection is closed.
	conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	started := time.Now()
	io.WriteString(conn, "POST /a/large HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"+
		fmt.Sprintf("%x\r\n", 20000)+sizedBody("", 20000)[:global+1])
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("stalled chunked body: %v", err)
	}
	resp.Body.Close()
	if elapsed := time.Since(started); resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close || elapsed > 5*time.Second {
		t.Errorf("stalled chunked body: %d after %v, connection closed %v; want 413 at once with the connection closed", resp.StatusCode, elapsed, resp.Close)
	}

	// Framing as the HTTP server reads it: an invalid chunk fails the read; a
	// chunked body overrides a declared length (RFC 9112 section 6.3), so a
	// small declared length cannot hide a large body; and an HTTP/1.0 request
	// has no chunked framing, so its body is empty. None is forwarded.
	head := "POST /a/large HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\n"
	framingCases := []struct {
		name, request string
		status        int
		reason        string
	}{
		{"invalid chunk size", head + "Transfer-Encoding: chunked\r\n\r\nzz\r\n{}\r\n0\r\n\r\n", 400, "read_failed"},
		{"declared length with a chunked body above the ceiling",
			head + "Content-Length: 10\r\nTransfer-Encoding: chunked\r\n\r\n" + chunkedBody(sizedBody("", global+1), 4096), 413, "too_large"},
		{"HTTP/1.0 chunked body above the ceiling", strings.Replace(head, "HTTP/1.1", "HTTP/1.0", 1) +
			"Transfer-Encoding: chunked\r\n\r\n" + chunkedBody(sizedBody("", global+1), 4096), 400, "malformed_json"},
	}
	for _, tc := range framingCases {
		status, reply := e.rawExchange(tc.request, false)
		_, id := requestID(&http.Response{StatusCode: status}, reply)
		events := e.requestEvents(id, 1)
		if status != tc.status || len(events) != 1 || events[0]["stage"] != "BODY" || events[0]["reason"] != tc.reason {
			t.Errorf("%s: status %d (%s), events %v, want %d with reason %s", tc.name, status, reply, events, tc.status, tc.reason)
		}
	}
	if n := e.a.count(); n != forwarded {
		t.Fatalf("%d receipts after the framing cases, want %d", n, forwarded)
	}

	// Concurrent chunked requests at and one byte above the ceiling, whose
	// length is known only as they are read: each accepted body arrives once
	// and intact, and no refused body arrives.
	e.resetReceivers()
	var mu sync.Mutex
	var accepted []string
	var wg sync.WaitGroup
	for client := range 8 {
		wg.Go(func() {
			for i := range 8 {
				size := global + i%2
				body := sizedBody(fmt.Sprintf("c%d-%d-", client, i), size)
				status, reply := e.sendBody("chunked", "/a/large", body)
				want := http.StatusOK
				if size > global {
					want = http.StatusRequestEntityTooLarge
				}
				if status != want {
					t.Errorf("concurrent chunked %d bytes: status %d (%s), want %d", size, status, reply, want)
				}
				if status == http.StatusOK {
					mu.Lock()
					accepted = append(accepted, body)
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	var received []string
	for _, r := range e.a.receipts() {
		received = append(received, r.Body)
	}
	slices.Sort(accepted)
	slices.Sort(received)
	if len(accepted) != 32 || !slices.Equal(received, accepted) {
		t.Fatalf("concurrent: %d accepted, %d received, equal %v", len(accepted), len(received), slices.Equal(received, accepted))
	}
	e.assertReceipts(32, 0, 0)
}

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
				want := expectedStages(nil, "INGRESS", stageReject)
				if stage == "BODY" {
					want = expectedStages(authenticated, "BODY", stageReject)
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

// A request keeps the schema of the generation it captured, and under
// repeated schema publications every decision matches the schema of the
// generation it reports.
func TestSchemaFollowsCapturedGeneration(t *testing.T) {
	e := newEnv(t)
	textSchema := `{"type":"object","additionalProperties":false,"required":["v"],"properties":{"v":{"type":"string"}}}`
	numberSchema := `{"type":"object","additionalProperties":false,"required":["v"],"properties":{"v":{"type":"integer"}}}`
	withSchema := func(schema string) Policy {
		p := e.policy(e.a, false)
		for i := range p.Operations {
			if p.Operations[i].ID == "write-a" {
				p.Operations[i].Body = jsonBody(1024, schema)
			}
		}
		return p
	}
	seed(t, e.database, withSchema(textSchema), seedOptions{})
	dir := t.TempDir()
	holdFile, release := filepath.Join(dir, "captured"), filepath.Join(dir, "release")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/item", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	first := e.status().Generation

	// Valid under the captured generation, invalid under the next one.
	result := make(chan int, 1)
	go func() {
		code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"v":"text"}`)
		result <- code
	}()
	waitForFile(t, holdFile)
	outcome, code := e.publish(Change{ID: "numbers", Expected: 1, Policy: mustJSON(withSchema(numberSchema))})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}
	os.WriteFile(release, nil, 0o600)
	if code := <-result; code != http.StatusOK {
		t.Fatalf("request captured under the text schema: %d, want 200", code)
	}
	if r := e.a.receipts(); len(r) != 1 || r[0].Generation != first || r[0].Body != `{"v":"text"}` {
		t.Fatalf("receipts %+v, want one under %s", r, first)
	}
	// New requests use the new schema.
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"v":"text"}`); code != http.StatusBadRequest {
		t.Fatalf("text value under the number schema: %d", code)
	}

	// Repeated publications under load: each generation's schema decides
	// every request that captured it.
	accepts := map[string]string{first: "text", outcome.Generation: "number"}
	revision := outcome.Revision
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	type decision struct{ body, requestID string }
	var rejected []decision
	for client := range 4 {
		wg.Go(func() {
			bodies := []string{`{"v":"text"}`, `{"v":7}`}
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				body := bodies[(client+i)%2]
				resp, reply := e.exchange(http.MethodPost, "/a/item", "application/json", body)
				if resp.StatusCode == http.StatusBadRequest {
					_, id := requestID(resp, reply)
					mu.Lock()
					rejected = append(rejected, decision{body, id})
					mu.Unlock()
				} else if resp.StatusCode != http.StatusOK {
					t.Errorf("status %d", resp.StatusCode)
				}
			}
		})
	}
	for i := range 40 {
		schema, kind := textSchema, "text"
		if i%2 == 1 {
			schema, kind = numberSchema, "number"
		}
		outcome, code := e.publish(Change{ID: fmt.Sprintf("swap-%d", i), Expected: revision, Policy: mustJSON(withSchema(schema))})
		if code != http.StatusOK {
			t.Fatalf("swap %d: %d %+v", i, code, outcome)
		}
		revision = outcome.Revision
		mu.Lock()
		accepts[outcome.Generation] = kind
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	kindOf := func(body string) string {
		if strings.Contains(body, `"text"`) {
			return "text"
		}
		return "number"
	}
	for _, r := range e.a.receipts()[1:] {
		if accepts[r.Generation] != kindOf(r.Body) {
			t.Fatalf("generation %s (%s schema) forwarded %s", r.Generation, accepts[r.Generation], r.Body)
		}
	}
	events := e.auditEvents()
	for _, d := range rejected {
		ev := findEvent(events, "request.rejected", d.requestID)
		if ev == nil {
			ev = findEvent(e.eventsWhen(func(ev []map[string]any) bool { return findEvent(ev, "request.rejected", d.requestID) != nil }), "request.rejected", d.requestID)
		}
		generation, _ := ev["generation"].(string)
		if ev == nil || ev["stage"] != "VALIDATION" || accepts[generation] == kindOf(d.body) {
			t.Fatalf("rejection of %s: event %v, generation schema %s", d.body, ev, accepts[generation])
		}
	}
	t.Logf("%d forwarded and %d refused across 40 schema publications", len(e.a.receipts())-1, len(rejected))
	if len(rejected) == 0 || len(e.a.receipts()) < 3 {
		t.Fatal("the load did not exercise both outcomes")
	}
}
