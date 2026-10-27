package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	maxJSONDepth        = 16
	maxJSONValues       = 4096
	maxNumberLength     = 32
	maxExponentDigits   = 3
	maxSchemaProperties = 64
	maxEnumValues       = 64
	maxSchemaLength     = 1 << 20
	maxHeaderName       = 64
)

// schemaURL names each schema inside its own compiler. Schemas never refer to
// other resources, and the compiler has no loader, so nothing is ever fetched.
const schemaURL = "urn:gateway:schema"

// bodyRule is a compiled body declaration.
type bodyRule struct {
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
	schemaBody schemaKind = iota
	schemaHeaders
)

var schemaTypes = []string{"object", "array", "string", "integer", "number", "boolean", "null"}

// compileSchema accepts a JSON Schema 2020-12 document only if every keyword
// in it belongs to the supported subset, then compiles it with the library.
// Checking the subset first means no keyword is ever silently ignored. It
// also returns the decoded root object.
func compileSchema(raw json.RawMessage, kind schemaKind, field string) (*jsonschema.Schema, map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil, invalid("INVALID_VALUE", "%s", field)
	}
	document, reason := decodeJSON(raw)
	if reason != "" {
		return nil, nil, invalid("SCHEMA_INVALID", "%s", field)
	}
	root, ok := document.(map[string]any)
	if !ok || (root["type"] != "object" && (kind == schemaHeaders || root["type"] != "array")) {
		return nil, nil, invalid("INVALID_VALUE", "%s.type", field)
	}
	if err := checkSchema(root, field); err != nil {
		return nil, nil, err
	}
	if kind == schemaHeaders {
		if err := checkHeaderSchema(root, field); err != nil {
			return nil, nil, err
		}
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, nil, invalid("SCHEMA_INVALID", "%s", field)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, nil, invalid("SCHEMA_INVALID", "%s", field)
	}
	return schema, root, nil
}

func compileHeaders(raw json.RawMessage, field string) (*headerRule, error) {
	schema, root, err := compileSchema(raw, schemaHeaders, field)
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
			return invalid("UNSUPPORTED_FEATURE", "%s.%s", field, keyword)
		}
		if !applies {
			return invalid("INVALID_VALUE", "%s.%s", field, keyword)
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

func checkArrayKeywords(schema map[string]any, field string) error {
	value, present := schema["items"]
	if !present {
		return invalid("INVALID_VALUE", "%s.items", field)
	}
	items, ok := value.(map[string]any)
	if !ok {
		return invalid("UNSUPPORTED_FEATURE", "%s.items", field)
	}
	if err := checkSchema(items, field+".items"); err != nil {
		return err
	}
	lower, upper := int64(0), int64(maxJSONValues)
	for _, keyword := range []string{"minItems", "maxItems"} {
		value, present := schema[keyword]
		if !present {
			continue
		}
		number, _ := value.(json.Number)
		n, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || n < 0 || n > maxJSONValues {
			return invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
		if keyword == "minItems" {
			lower = n
		} else {
			upper = n
		}
	}
	if lower > upper {
		return invalid("INVALID_VALUE", "%s.minItems", field)
	}
	return nil
}

// checkTypes accepts a supported type name or a non-empty array of distinct
// ones.
func checkTypes(value any, field string) (map[string]bool, error) {
	var names []any
	switch v := value.(type) {
	case string:
		names = []any{v}
	case []any:
		names = v
	default:
		return nil, invalid("INVALID_VALUE", "%s", field)
	}
	if len(names) == 0 {
		return nil, invalid("INVALID_VALUE", "%s", field)
	}
	types := map[string]bool{}
	for _, item := range names {
		name, ok := item.(string)
		if !ok || types[name] {
			return nil, invalid("INVALID_VALUE", "%s", field)
		}
		if !slices.Contains(schemaTypes, name) {
			return nil, invalid("UNSUPPORTED_FEATURE", "%s", field)
		}
		types[name] = true
	}
	return types, nil
}

func checkObjectKeywords(schema map[string]any, field string) error {
	if _, ok := schema["additionalProperties"].(bool); !ok {
		return invalid("INVALID_VALUE", "%s.additionalProperties", field)
	}

	properties := map[string]any{}
	if value, present := schema["properties"]; present {
		var ok bool
		properties, ok = value.(map[string]any)
		if !ok || len(properties) > maxSchemaProperties {
			return invalid("INVALID_VALUE", "%s.properties", field)
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		child, ok := properties[name].(map[string]any)
		path := field + ".properties." + name
		if !ok {
			return invalid("UNSUPPORTED_FEATURE", "%s", path)
		}
		if err := checkSchema(child, path); err != nil {
			return err
		}
	}

	if value, present := schema["required"]; present {
		required, ok := value.([]any)
		if !ok {
			return invalid("INVALID_VALUE", "%s.required", field)
		}
		seen := map[string]bool{}
		for _, item := range required {
			name, ok := item.(string)
			if !ok || seen[name] || properties[name] == nil {
				return invalid("INVALID_VALUE", "%s.required", field)
			}
			seen[name] = true
		}
	}
	return nil
}

func checkEnum(schema map[string]any, field string) error {
	value, present := schema["enum"]
	if !present {
		return nil
	}
	values, ok := value.([]any)
	if !ok || len(values) == 0 || len(values) > maxEnumValues {
		return invalid("INVALID_VALUE", "%s.enum", field)
	}
	for _, item := range values {
		switch item.(type) {
		case string, json.Number, bool, nil:
		default:
			return invalid("UNSUPPORTED_FEATURE", "%s.enum", field)
		}
	}
	return nil
}

func checkLengths(schema map[string]any, field string) error {
	lower, upper := int64(0), int64(maxSchemaLength)
	for _, keyword := range []string{"minLength", "maxLength"} {
		value, present := schema[keyword]
		if !present {
			continue
		}
		number, _ := value.(json.Number)
		n, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || n < 0 || n > maxSchemaLength {
			return invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
		if keyword == "minLength" {
			lower = n
		} else {
			upper = n
		}
	}
	if lower > upper {
		return invalid("INVALID_VALUE", "%s.minLength", field)
	}
	return nil
}

func checkRange(schema map[string]any, field string) error {
	bounds := map[string]*big.Rat{}
	for _, keyword := range []string{"minimum", "maximum"} {
		value, present := schema[keyword]
		if !present {
			continue
		}
		// Numbers in a decoded schema are already bounded literals.
		number, ok := value.(json.Number)
		if !ok {
			return invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
		rat, ok := new(big.Rat).SetString(string(number))
		if !ok {
			return invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
		bounds[keyword] = rat
	}
	if bounds["minimum"] != nil && bounds["maximum"] != nil && bounds["minimum"].Cmp(bounds["maximum"]) > 0 {
		return invalid("INVALID_VALUE", "%s.minimum", field)
	}
	return nil
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
		if !isHeaderName(name) || isGatewayHeader(name) || unforwardedHeaders[name] {
			return invalid("INVALID_VALUE", "%s", path)
		}
		if value.(map[string]any)["type"] != "string" {
			return invalid("INVALID_VALUE", "%s.type", path)
		}
	}
	return nil
}

// isHeaderName accepts canonical header names of letters, digits and '-'.
func isHeaderName(name string) bool {
	if name == "" || len(name) > maxHeaderName || http.CanonicalHeaderKey(name) != name {
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

// decodeJSON decodes exactly one JSON value into the form the schema library
// validates (maps, slices, json.Number, strings, booleans and nil). It
// rejects invalid UTF-8, repeated member names, data after the value, nesting
// deeper than maxJSONDepth, more than maxJSONValues values and number
// literals the library could not compare cheaply. encoding/json alone would
// keep the last repeated member and replace invalid UTF-8, so the gateway and
// an upstream could read different documents. It returns the value, or the
// rejection reason.
func decodeJSON(data []byte) (any, string) {
	if !utf8.Valid(data) || hasLoneSurrogate(data) {
		return nil, "invalid_encoding"
	}
	type container struct {
		members map[string]any // nil for an array
		items   []any
		name    string
		named   bool // members: the next token is a value
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var stack []*container
	values := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, "malformed_json"
		}

		var top *container
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.members != nil && !top.named {
			if delim, ok := token.(json.Delim); !ok || delim != '}' {
				name := token.(string)
				if _, exists := top.members[name]; exists {
					return nil, "duplicate_member"
				}
				top.name, top.named = name, true
				continue
			}
		}

		var value any
		switch t := token.(type) {
		case json.Delim:
			if t == '{' || t == '[' {
				values++
				if values > maxJSONValues || len(stack) == maxJSONDepth {
					return nil, "limit_exceeded"
				}
				opened := &container{}
				if t == '{' {
					opened.members = map[string]any{}
				}
				stack = append(stack, opened)
				continue
			}
			// The container is complete.
			stack = stack[:len(stack)-1]
			switch {
			case top.members != nil:
				value = top.members
			case top.items == nil:
				value = []any{}
			default:
				value = top.items
			}
		case json.Number:
			values++
			if values > maxJSONValues || !isBoundedNumber(t) {
				return nil, "limit_exceeded"
			}
			value = t
		default:
			values++
			if values > maxJSONValues {
				return nil, "limit_exceeded"
			}
			value = t
		}

		if len(stack) == 0 {
			if _, err := decoder.Token(); err != io.EOF {
				return nil, "trailing_data"
			}
			return value, ""
		}
		parent := stack[len(stack)-1]
		if parent.members != nil {
			parent.members[parent.name] = value
			parent.named = false
		} else {
			parent.items = append(parent.items, value)
		}
	}
}

// hasLoneSurrogate reports a \u escape of a UTF-16 surrogate that is not part
// of a high-low pair. JSON syntax allows it, but it encodes no character:
// encoding/json replaces it with U+FFFD while other parsers keep or reject
// it. A backslash can only occur inside a string of a document that also
// passes the decoder, so escapes are found by scanning for backslashes.
func hasLoneSurrogate(data []byte) bool {
	surrogate := func(i int) (rune, bool) {
		if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
			return 0, false
		}
		code, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
		return rune(code), err == nil && code >= 0xd800 && code <= 0xdfff
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		code, isSurrogate := surrogate(i)
		switch {
		case !isSurrogate:
			i++ // skip the escaped character, which may be a backslash
		case code >= 0xdc00:
			return true // a low surrogate without a high one
		default:
			low, ok := surrogate(i + 6)
			if !ok || low < 0xdc00 {
				return true
			}
			i += 11
		}
	}
	return false
}

// isBoundedNumber limits a number literal's length and exponent.
func isBoundedNumber(number json.Number) bool {
	literal := string(number)
	if len(literal) > maxNumberLength {
		return false
	}
	_, exponent, found := strings.Cut(strings.ToLower(literal), "e")
	if !found {
		return true
	}
	exponent = strings.TrimLeft(exponent, "+-")
	return len(exponent) <= maxExponentDigits
}

// rejection is a request refused at a body or validation stage.
type rejection struct {
	status int
	code   string
	reason string
}

// readBody enforces the operation's body declaration: no body for a bodyless
// operation; otherwise the size ceiling, a JSON media type, a complete read
// and a bounded, unambiguous JSON document. The request body is replaced with
// the bytes read, so nothing unchecked can stream upstream. It returns the
// decoded document.
//
// The size ceiling is the smaller of the operation's and the global one, and
// it bounds the bytes read as well as the declared length: a chunked body, or
// an HTTP/2 body without a content-length field, declares no length, so a
// check of the declared length alone would let it pass the global ceiling.
func readBody(r *http.Request, rule *bodyRule, globalMaxBytes int64) (any, *rejection) {
	if rule == nil {
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
			return nil, &rejection{http.StatusBadRequest, "bad_request", "unexpected_body"}
		}
		return nil, nil
	}
	maxBytes := min(rule.maxBytes, globalMaxBytes)
	if r.ContentLength > maxBytes {
		return nil, &rejection{http.StatusRequestEntityTooLarge, "payload_too_large", "too_large"}
	}
	if !isJSONMediaType(r.Header.Values("Content-Type")) {
		return nil, &rejection{http.StatusUnsupportedMediaType, "unsupported_media_type", "media_type"}
	}
	// The policy compiler bounds the operation's ceiling, so maxBytes+1
	// cannot overflow.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, &rejection{http.StatusBadRequest, "bad_request", "read_failed"}
	}
	if int64(len(body)) > maxBytes {
		return nil, &rejection{http.StatusRequestEntityTooLarge, "payload_too_large", "too_large"}
	}
	document, reason := decodeJSON(body)
	if reason != "" {
		return nil, &rejection{http.StatusBadRequest, "bad_request", reason}
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return document, nil
}

// isJSONMediaType accepts exactly one Content-Type field naming
// application/json, optionally with the parameter charset=utf-8.
func isJSONMediaType(fields []string) bool {
	if len(fields) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(fields[0])
	if err != nil || mediaType != "application/json" {
		return false
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

// validateRequest checks the declared request headers and the decoded body
// against the operation's schemas. It returns the rejection reason, or "".
// Library error text can quote the input, so it is never returned.
func validateRequest(header http.Header, document any, r *rule) string {
	// The proxy removes every field named in Connection, so a validated field
	// named there would not reach the upstream as it was checked: a declared
	// header, or the media type of a validated body. Such a request is refused
	// rather than forwarded without it.
	for _, name := range connectionNames(header) {
		validatedHeader := r.headers != nil && slices.Contains(r.headers.names, name)
		validatedMediaType := r.body != nil && name == "Content-Type"
		if validatedHeader || validatedMediaType {
			return "connection_nominated"
		}
	}
	if r.headers != nil {
		declared := map[string]any{}
		for _, name := range r.headers.names {
			values := header.Values(name)
			if len(values) > 1 {
				return "repeated_header"
			}
			if len(values) == 1 {
				declared[name] = values[0]
			}
		}
		if r.headers.schema.Validate(declared) != nil {
			return "header_mismatch"
		}
	}
	if r.body != nil && r.body.schema.Validate(document) != nil {
		return "schema_mismatch"
	}
	return ""
}
