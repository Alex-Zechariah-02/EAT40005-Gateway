// Sign-in, parameters and request body of one operation.

package openapi

import (
	"encoding/json"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// emptyBodyNote states the gateway's behaviour (internal/gateway/validation.go
// readBody) for a request without a body to an operation with a body rule.
const emptyBodyNote = "The description allows no body; the gateway refuses a request without one (415 without a JSON Content-Type, else 400 malformed_json)."

// security sets the operation's sign-in from the effective requirement.
func (m *mapper) security(object map[string]any, c *candidate) *refusal {
	requirements, stated := object["security"]
	if !stated {
		requirements, stated = m.root["security"]
	}
	list, _ := requirements.([]any)
	switch {
	case !stated:
		c.op.Auth, c.entry.Note = "required", NoteDefaultSignIn
		return nil
	case len(list) == 0:
		c.op.Auth, c.entry.Note = "none", "public: consider a rate limit by client address"
		return nil
	}
	components, _ := m.root["components"].(map[string]any)
	defined, _ := components["securitySchemes"].(map[string]any)
	for _, item := range list {
		requirement, _ := item.(map[string]any)
		if len(requirement) == 0 {
			return refuse(CodeSecurity, "", "Optional sign-in ({}) cannot be expressed; make it required or public.")
		}
		for _, name := range sortedKeys(requirement) {
			if refused := m.scheme(defined, name); refused != nil {
				return refused
			}
			scopes, _ := requirement[name].([]any)
			for _, scope := range scopes {
				if text, ok := scope.(string); ok {
					c.scopes = append(c.scopes, policy.Clip(text))
				}
			}
		}
	}
	c.op.Auth = "required"
	return nil
}

// scheme accepts only schemes that present a bearer token.
func (m *mapper) scheme(defined map[string]any, name string) *refusal {
	pointer := "#/components/securitySchemes/" + escape(name)
	raw, present := defined[name]
	if !present {
		return refuse(CodeSecurity, "", "The security scheme %q is not defined.", policy.Clip(name))
	}
	node, refused := follow(m.root, raw, pointer)
	if refused != nil {
		return refused
	}
	scheme, _ := node.(map[string]any)
	kind, _ := scheme["type"].(string)
	httpScheme, _ := scheme["scheme"].(string)
	bearer := kind == "http" && strings.EqualFold(httpScheme, "bearer")
	if bearer || kind == "oauth2" || kind == "openIdConnect" {
		return nil
	}
	return refuse(CodeSecurity, "", "Sign-in by %q cannot be enforced; only bearer tokens are checked.", policy.Clip(kind+" "+httpScheme))
}

// parameters maps header parameters to the header schema and lists the rest.
// Operation parameters replace path-level ones with the same name and location.
func (m *mapper) parameters(item, object map[string]any, itemPointer, pointer string, c *candidate, s *schemas) *refusal {
	type parameter struct {
		value   map[string]any
		pointer string
	}
	var merged []parameter
	index := map[string]int{}
	for _, level := range []struct {
		list    any
		pointer string
	}{{item["parameters"], itemPointer + "/parameters"}, {object["parameters"], pointer + "/parameters"}} {
		list, _ := level.list.([]any)
		for i, raw := range list {
			at := located(raw, level.pointer+"/"+strconv.Itoa(i))
			node, refused := follow(m.root, raw, at)
			if refused != nil {
				return refused
			}
			value, _ := node.(map[string]any)
			name, _ := value["name"].(string)
			in, _ := value["in"].(string)
			if position, seen := index[in+" "+name]; seen {
				merged[position] = parameter{value, at}
				continue
			}
			index[in+" "+name] = len(merged)
			merged = append(merged, parameter{value, at})
		}
	}

	properties, required := map[string]any{}, []any{}
	for _, p := range merged {
		name, _ := p.value["name"].(string)
		in, _ := p.value["in"].(string)
		canonical := http.CanonicalHeaderKey(name)
		ignored := canonical == "Accept" || canonical == "Content-Type" || canonical == "Authorization"
		if in != "header" || ignored {
			c.notEnforced = append(c.notEnforced, NotEnforced{Operation: c.op.ID, Parameter: policy.Clip(name), In: policy.Clip(in)})
			continue
		}
		if _, present := p.value["content"]; present {
			return refuse(CodeKeyword, p.pointer+"/content", "Header parameters described by content cannot be validated.")
		}
		schema, present := p.value["schema"]
		if !present {
			schema = map[string]any{}
		}
		converted, refused := s.convert(schema, p.pointer+"/schema", "headers.properties."+canonical, 1, nil)
		if refused != nil {
			return refused
		}
		properties[canonical] = converted
		if isRequired, _ := p.value["required"].(bool); isRequired {
			required = append(required, canonical)
		}
	}
	if len(properties) == 0 {
		return nil
	}
	headers := map[string]any{"type": "object", "properties": properties, "additionalProperties": true}
	if len(required) > 0 {
		headers["required"] = required
	}
	c.op.Headers, _ = json.Marshal(headers)
	return nil
}

// body maps a JSON request body to a body rule.
func (m *mapper) body(object map[string]any, pointer string, c *candidate, s *schemas) *refusal {
	raw, present := object["requestBody"]
	if !present {
		if c.maxBytes > 0 {
			return refuse(CodeExtension, "x-apiward-max-bytes", "x-apiward-max-bytes needs a request body.")
		}
		return nil
	}
	at := located(raw, pointer+"/requestBody")
	node, refused := follow(m.root, raw, at)
	if refused != nil {
		return refused
	}
	requestBody, _ := node.(map[string]any)
	content, _ := requestBody["content"].(map[string]any)
	if len(content) == 0 {
		return refuse(CodeMediaType, "", "The request body names no media type.")
	}
	types := sortedKeys(content)
	first, _ := content[types[0]].(map[string]any)
	var others []string
	for _, name := range types {
		mediaType, _, err := mime.ParseMediaType(name)
		if err != nil || mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
			return refuse(CodeMediaType, "", "Only JSON request bodies can be validated; found %q.", policy.Clip(name))
		}
		if mediaType != "application/json" {
			others = append(others, policy.Clip(name))
		}
		this, _ := content[name].(map[string]any)
		if !reflect.DeepEqual(first["schema"], this["schema"]) {
			return refuse(CodeMediaType, "", "The JSON media types have different schemas; one body rule cannot express both.")
		}
	}

	if c.maxBytes == 0 {
		c.maxBytes = m.options.MaxBodyBytes
		c.assumed = append(c.assumed, Assumed{Operation: c.op.ID, Field: "body.max_bytes", Value: c.maxBytes})
	}
	c.op.Body = &policy.BodyRule{MaxBytes: c.maxBytes} // without a schema the compiler refuses it
	if schema, present := first["schema"]; present {
		converted, refused := s.convert(schema, at+"/content/"+escape(types[0])+"/schema", "body.schema", 1, nil)
		if refused != nil {
			return refused
		}
		c.op.Body.Schema, _ = json.Marshal(converted)
	}
	if isRequired, _ := requestBody["required"].(bool); !isRequired {
		c.changed = append(c.changed, Changed{Operation: c.op.ID, Field: "body", Note: emptyBodyNote})
	}
	if len(others) > 0 {
		c.changed = append(c.changed, Changed{Operation: c.op.ID, Field: "body.media_type",
			Note: "The gateway accepts only Content-Type application/json; a request sent as " + strings.Join(others, " or ") + " is refused with 415."})
	}
	return nil
}

// located is where an object is defined: its reference target, or pointer.
func located(raw any, pointer string) string {
	object, _ := raw.(map[string]any)
	if ref, ok := object["$ref"].(string); ok {
		return ref
	}
	return pointer
}
