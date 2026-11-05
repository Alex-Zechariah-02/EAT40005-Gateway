// Bounded JSON Schema compilation for bodies and headers.

package compile

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaURL names each schema inside its own compiler. Schemas never refer to
// other resources, and the compiler has no loader, so nothing is ever fetched.
const schemaURL = "urn:gateway:schema"

// BodyRule is a compiled body declaration.
type BodyRule struct {
	maxBytes int64
	schema   *jsonschema.Schema
}

// headerRule is a compiled header schema and the header names it declares.
type headerRule struct {
	names  []string
	schema *jsonschema.Schema
}

type schemaKind int

const (
	SchemaBody schemaKind = iota
	SchemaHeaders
)

var schemaTypes = []string{"object", "array", "string", "integer", "number", "boolean", "null"}

// CompileSchema accepts a JSON Schema 2020-12 document only if every keyword
// in it belongs to the supported subset, then compiles it with the library.
// Checking the subset first means no keyword is ever silently ignored. It
// also returns the decoded root object.
func CompileSchema(raw json.RawMessage, kind schemaKind, field string) (*jsonschema.Schema, map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil, policy.Invalid("INVALID_VALUE", "%s", field)
	}
	document, reason := policy.DecodeJSON(raw)
	if reason != "" {
		return nil, nil, policy.Invalid("SCHEMA_INVALID", "%s", field)
	}
	root, ok := document.(map[string]any)
	if !ok || (root["type"] != "object" && (kind == SchemaHeaders || root["type"] != "array")) {
		return nil, nil, policy.Invalid("INVALID_VALUE", "%s.type", field)
	}
	if err := checkSchema(root, field); err != nil {
		return nil, nil, err
	}
	if kind == SchemaHeaders {
		if err := checkHeaderSchema(root, field); err != nil {
			return nil, nil, err
		}
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, nil, policy.Invalid("SCHEMA_INVALID", "%s", field)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, nil, policy.Invalid("SCHEMA_INVALID", "%s", field)
	}
	return schema, root, nil
}

func compileHeaders(raw json.RawMessage, field string) (*headerRule, error) {
	schema, root, err := CompileSchema(raw, SchemaHeaders, field)
	if err != nil {
		return nil, err
	}
	properties, _ := root["properties"].(map[string]any)
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	slices.Sort(names)
	return &headerRule{names: names, schema: schema}, nil
}

// checkSchema checks one schema node and its property schemas against the
// supported subset: every node is an object with a type; object keywords need
// type object, which must state additionalProperties; string keywords need
// type string; numeric keywords need type integer or number.
func checkSchema(schema map[string]any, field string) error {
	types, err := checkTypes(schema["type"], field+".type")
	if err != nil {
		return err
	}
	keywords := make([]string, 0, len(schema))
	for keyword := range schema {
		keywords = append(keywords, keyword)
	}
	slices.Sort(keywords)
	for _, keyword := range keywords {
		applies := true
		switch keyword {
		case "type", "enum":
		case "properties", "required", "additionalProperties":
			applies = types["object"]
		case "minLength", "maxLength":
			applies = types["string"]
		case "minimum", "maximum":
			applies = types["integer"] || types["number"]
		case "items", "minItems", "maxItems":
			applies = types["array"]
		default:
			return policy.Invalid("UNSUPPORTED_FEATURE", "%s.%s", field, keyword)
		}
		if !applies {
			return policy.Invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
	}

	if types["object"] {
		if err := checkObjectKeywords(schema, field); err != nil {
			return err
		}
	}
	if types["array"] {
		if err := checkArrayKeywords(schema, field); err != nil {
			return err
		}
	}
	if err := checkEnum(schema, field); err != nil {
		return err
	}
	if err := checkLengths(schema, field); err != nil {
		return err
	}
	return checkRange(schema, field)
}

// unforwardedHeaders are the request fields that an upstream would not
// receive as the gateway validated them: the credential, which the gateway
// consumes; the hop-by-hop fields, which the proxy removes (RFC 9110, section
// 7.6.1); Host and Content-Length, which net/http keeps outside the header
// map or writes from the body actually sent; and the forwarding fields, which
// the proxy removes, replaces or extends.
var unforwardedHeaders = map[string]bool{
	"Authorization": true,

	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,

	"Host": true, "Content-Length": true,

	"Forwarded": true, "X-Forwarded-For": true, "X-Forwarded-Host": true, "X-Forwarded-Proto": true, "Via": true,
}

// checkHeaderSchema requires every declared header to be a canonical header
// name, with a string schema, that reaches the upstream as validated: neither
// a header the gateway sets nor an unforwarded one.
func checkHeaderSchema(root map[string]any, field string) error {
	properties, _ := root["properties"].(map[string]any)
	for name, value := range properties {
		path := field + ".properties." + name
		if !isHeaderName(name) || IsGatewayHeader(name) || unforwardedHeaders[name] {
			return policy.Invalid("INVALID_VALUE", "%s", path)
		}
		if value.(map[string]any)["type"] != "string" {
			return policy.Invalid("INVALID_VALUE", "%s.type", path)
		}
	}
	return nil
}

// isHeaderName accepts canonical header names of letters, digits and '-'.
func isHeaderName(name string) bool {
	if name == "" || len(name) > policy.MaxHeaderName || http.CanonicalHeaderKey(name) != name {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alphanumeric && c != '-' {
			return false
		}
	}
	return true
}
