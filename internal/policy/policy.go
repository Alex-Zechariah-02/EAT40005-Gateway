// Policy document types, limits and strict decoding.

package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// SchemaVersion is the only policy document version this compiler accepts.
const SchemaVersion = 4

// ProfileCore is the only enforcement profile that can be activated. Under it
// the gateway enforces routing, the per-operation access flag, bearer JWT
// authentication and role permits on every operation declared auth
// "required", the operation's rate rule, its body and header schemas, and the
// mandatory audit record before forwarding. Any other profile is rejected
// rather than activated with controls it cannot run.
const ProfileCore = "core"

const (
	MaxPolicyBytes   = 64 << 10
	MaxPolicyDepth   = 16
	MaxUpstreams     = 64
	MaxAPIs          = 64
	MaxOperations    = 256
	MaxRoles         = 64
	MaxPermits       = 4096
	MaxIDLength      = 64
	MaxSubjectLength = 128
	MaxTargetLength  = 256
	MaxBodyLimit     = 1 << 20
)

// Policy is the complete permanent policy document. Every publication carries
// a whole document; there is no partial update.
type Policy struct {
	SchemaVersion int         `json:"schema_version"`
	Profile       string      `json:"profile"`
	Upstreams     []Upstream  `json:"upstreams"`
	APIs          []API       `json:"apis"`
	Operations    []Operation `json:"operations"`
	Roles         []Role      `json:"roles"`
	Permits       []Permit    `json:"permits"`
}

// Upstream is one entry of the closed destination inventory.
type Upstream struct {
	ID   string `json:"id"`
	Dial string `json:"dial"`
}

// API registers one managed API and the single upstream that serves it.
type API struct {
	ID       string   `json:"id"`
	Upstream string   `json:"upstream"`
	Forward  *Forward `json:"forward,omitempty"`
}

// Forward rewrites the path an admitted request is forwarded with: the
// inbound path without StripPrefix, after AddPrefix. Every decision is made
// on the inbound path; the rewrite applies only to the outbound request.
//
// In an OpenAPI description or exchange file the rule is the root member
// named ForwardExtension.
type Forward struct {
	StripPrefix string `json:"strip_prefix,omitempty"`
	AddPrefix   string `json:"add_prefix,omitempty"`
}

// ForwardExtension is the OpenAPI root member that carries an API's Forward.
const ForwardExtension = "x-apiward-forward"

// Operation binds a method and target of an API to an access decision and
// the controls that apply to it.
type Operation struct {
	ID      string          `json:"id"`
	API     string          `json:"api"`
	Method  string          `json:"method"`
	Target  string          `json:"target"`
	Access  string          `json:"access"`
	Auth    string          `json:"auth"`
	Body    *BodyRule       `json:"body,omitempty"`
	Headers json.RawMessage `json:"headers,omitempty"` // JSON Schema for declared request headers
	Rate    *RateRule       `json:"rate,omitempty"`
	When    *When           `json:"when,omitempty"`
}

// BodyRule declares a JSON request body, its size ceiling and the JSON Schema
// it must satisfy. An operation without a BodyRule accepts no request body.
type BodyRule struct {
	MaxBytes int64           `json:"max_bytes"`
	Schema   json.RawMessage `json:"schema"`
}

// RateRule limits requests to an operation by verified subject or direct
// client address within a sliding window.
type RateRule struct {
	Algorithm     string `json:"algorithm"`
	Count         int64  `json:"count"`
	WindowSeconds int64  `json:"window_seconds"`
	Key           string `json:"key"`
}

// Role is a flat role identifier.
type Role struct {
	ID string `json:"id"`
}

// Permit grants a role an operation. Roles have no hierarchy: a permit
// applies to the named role only.
type Permit struct {
	Role      string `json:"role"`
	Operation string `json:"operation"`
}

// PolicyError is a typed candidate rejection. Field is the JSON path of the
// offending value when one applies.
type PolicyError struct {
	Code  string
	Field string
}

func (e *PolicyError) Error() string {
	if e.Field == "" {
		return e.Code
	}
	return e.Code + " at " + e.Field
}

func Invalid(code, field string, args ...any) *PolicyError {
	return &PolicyError{Code: code, Field: fmt.Sprintf(field, args...)}
}

var AllowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// DecodePolicy parses a policy document. It rejects oversized or deeply
// nested input, repeated or non-lowercase member names, unknown fields,
// unsupported schema versions and trailing data.
func DecodePolicy(document []byte) (Policy, error) {
	if len(document) > MaxPolicyBytes {
		return Policy{}, Invalid("LIMIT_EXCEEDED", "")
	}
	if err := CheckMembers(document, MaxPolicyDepth); err != nil {
		return Policy{}, err
	}

	// Read the version first, so a document written for another version is
	// reported as such rather than as a set of unknown fields.
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(document, &header); err != nil {
		return Policy{}, Invalid("SCHEMA_INVALID", "")
	}
	if header.SchemaVersion != SchemaVersion {
		return Policy{}, Invalid("SCHEMA_VERSION_UNSUPPORTED", "schema_version")
	}

	var p Policy
	if err := DecodeStrict(document, &p); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return Policy{}, Invalid("UNKNOWN_FIELD", "")
		}
		if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return Policy{}, Invalid("SCHEMA_INVALID", "%s", typeErr.Field)
		}
		return Policy{}, Invalid("SCHEMA_INVALID", "")
	}
	return p, nil
}

// DecodeStrict decodes exactly one JSON value, rejecting unknown fields and
// anything after the value.
func DecodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("data after the JSON value")
	}
	return nil
}

// CheckMembers rejects JSON nested deeper than maxDepth, and objects with a
// repeated member name or a name outside [a-z0-9_]. encoding/json keeps the
// last of repeated names and matches names case-insensitively, so without
// this check two readers of the same document could disagree about it.
//
// Values of members named "schema" or "headers" are JSON Schemas, whose
// keywords and property names are case-sensitive and not limited to
// [a-z0-9_]; the "headers" list of a "when" object is not one. They are decoded as raw JSON, never into Go structs, so only the
// repeated-name and depth rules apply inside them.
func CheckMembers(data []byte, maxDepth int) error {
	// encoding/json replaces malformed Unicode. Policy, operator and Admin
	// inputs must retain the same unambiguous UTF-8 representation as bodies.
	if !utf8.Valid(data) || hasLoneSurrogate(data) {
		return Invalid("SCHEMA_INVALID", "")
	}
	type container struct {
		object   bool
		wantName bool
		names    map[string]bool
		lastName string
		name     string // the member this container is the value of
		schema   bool   // inside a schema value
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var stack []*container
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return Invalid("SCHEMA_INVALID", "")
		}

		var parent *container
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}

		if delim, ok := token.(json.Delim); ok && (delim == '}' || delim == ']') {
			stack = stack[:len(stack)-1]
			continue
		}

		if parent != nil && parent.object && parent.wantName {
			name := token.(string)
			if !parent.schema && !isMemberName(name) {
				return Invalid("UNKNOWN_FIELD", "")
			}
			if parent.names[name] {
				return Invalid("DUPLICATE_MEMBER", "%s", name)
			}
			parent.names[name] = true
			parent.lastName = name
			parent.wantName = false
			continue
		}

		// A value: once it is complete, an enclosing object expects a name.
		if parent != nil && parent.object {
			parent.wantName = true
		}
		if delim, ok := token.(json.Delim); ok {
			if len(stack) == maxDepth {
				return Invalid("LIMIT_EXCEEDED", "")
			}
			inSchema := parent != nil &&
				(parent.schema || (parent.object && (parent.lastName == "schema" || parent.lastName == "headers" && parent.name != "when")))
			name := ""
			if parent != nil && parent.object {
				name = parent.lastName
			}
			stack = append(stack, &container{
				name:     name,
				object:   delim == '{',
				wantName: delim == '{',
				names:    map[string]bool{},
				schema:   inSchema,
			})
		}
	}
}

func isMemberName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		lower := c >= 'a' && c <= 'z'
		digit := c >= '0' && c <= '9'
		if !lower && !digit && c != '_' {
			return false
		}
	}
	return true
}
