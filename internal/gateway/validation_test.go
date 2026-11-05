// Schema compilation, bounded body decoding and media types.

package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// schemaPolicy is threeAPIPolicy with the given body schema on orders.create.
func schemaPolicy(schema string) policy.Policy {
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
		g, err := compile.Compile(schemaPolicy(schema), 1, time.Time{})
		if err != nil {
			t.Errorf("schema %s rejected: %v", schema, err)
			continue
		}
		if _, r := g.Match("/orders", "POST"); r.Body() == nil || r.Body().Validate("not an object") == nil || r.Body().MaxBytes() != 1024 {
			t.Errorf("schema %s not bound to its operation", schema)
		}
		if _, r := g.Match("/orders", "GET"); r.Body() != nil {
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
		{"too many enum values", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"integer","enum":[` + numbers(policy.MaxEnumValues+1) + `]}}}`, "INVALID_VALUE", field + ".properties.a.enum"},
		{"too many properties", `{"type":"object","additionalProperties":true,"properties":{` + properties(policy.MaxSchemaProperties+1) + `}}`, "INVALID_VALUE", field + ".properties"},
	}
	for _, tc := range invalid {
		started := time.Now()
		g, err := compile.Compile(schemaPolicy(tc.schema), 1, time.Time{})
		policyErr, _ := err.(*policy.PolicyError)
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
		_, err := compile.Compile(p, 1, time.Time{})
		policyErr, _ := err.(*policy.PolicyError)
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
		{"nesting at the bound", deep(policy.MaxJSONDepth), ""},
		{"nesting above the bound", deep(policy.MaxJSONDepth + 1), "limit_exceeded"},
		{"values at the bound", "[" + strings.TrimSuffix(strings.Repeat("1,", policy.MaxJSONValues-1), ",") + "]", ""},
		{"values above the bound", "[" + strings.TrimSuffix(strings.Repeat("1,", policy.MaxJSONValues), ",") + "]", "limit_exceeded"},
		{"long number", `{"a":` + strings.Repeat("1", policy.MaxNumberLength+1) + `}`, "limit_exceeded"},
		{"large exponent", `{"a":1e999}`, ""},
		{"exponent above the bound", `{"a":1e1000}`, "limit_exceeded"},
		{"negative exponent above the bound", `{"a":1E-1000}`, "limit_exceeded"},
	}
	for _, tc := range cases {
		started := time.Now()
		_, reason := policy.DecodeJSON([]byte(tc.body))
		if reason != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.name, reason, tc.reason)
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Errorf("%s: took %v", tc.name, elapsed)
		}
	}

	// The decoded value keeps numbers exact and every member.
	value, _ := policy.DecodeJSON([]byte(`{"n":12345678901234567890,"e":[],"o":{}}`))
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
