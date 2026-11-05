// Schema conversion to the gateway's keyword subset: internal references inlined, annotations removed.

package openapi

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// refusal is why one operation cannot be imported; suggestion is a valid
// replacement for a refused operationId.
type refusal struct {
	code, message, field, suggestion string
}

func refuse(code, field, format string, args ...any) *refusal {
	return &refusal{code: code, field: field, message: fmt.Sprintf(format, args...)}
}

// copied are the keywords the gateway enforces whose values need no conversion.
var copied = map[string]bool{
	"type": true, "enum": true, "required": true, "minLength": true, "maxLength": true,
	"minimum": true, "maximum": true, "minItems": true, "maxItems": true,
}

var annotations = map[string]bool{
	"title": true, "description": true, "example": true, "examples": true, "default": true,
	"deprecated": true, "readOnly": true, "writeOnly": true, "externalDocs": true, "xml": true,
}

func isAnnotation(keyword string) bool {
	return annotations[keyword] || strings.HasPrefix(keyword, "x-")
}

// schemas converts the schemas of one operation; it collects what it
// assumed and removed so they are reported only if the operation is kept.
type schemas struct {
	root        map[string]any
	operation   string
	values      int // values produced, across every schema of the operation
	assumed     []Assumed
	annotations []Removed
}

// convert returns the gateway form of the schema at pointer; field is its
// path in the operation (for example "body.schema").
func (s *schemas) convert(node any, pointer, field string, depth int, refs []string) (any, *refusal) {
	s.values++
	if depth > policy.MaxJSONDepth || s.values > policy.MaxJSONValues {
		return nil, refuse(CodeSchemaLimit, pointer, "The schema is nested deeper than %d or expands to more than %d values.",
			policy.MaxJSONDepth, policy.MaxJSONValues)
	}
	object, ok := node.(map[string]any)
	if !ok {
		return node, nil // not a schema object; the compiler refuses it
	}
	if ref, present := object["$ref"]; present {
		return s.reference(object, ref, pointer, field, depth, refs)
	}

	out := map[string]any{}
	for _, keyword := range sortedKeys(object) {
		value, at := object[keyword], pointer+"/"+escape(keyword)
		var refused *refusal
		switch {
		case isAnnotation(keyword):
			s.annotations = append(s.annotations, Removed{Operation: s.operation, Field: at})
			continue
		case copied[keyword]:
			s.values += count(value)
		case keyword == "properties":
			value, refused = s.properties(value, at, field+".properties", depth, refs)
		case keyword == "items":
			value, refused = s.convert(value, at, field+".items", depth+1, refs)
		case keyword == "additionalProperties":
			if _, isBool := value.(bool); !isBool {
				return nil, refuse(CodeKeyword, at, "Only true or false is supported for additionalProperties.")
			}
		default:
			return nil, refuse(CodeKeyword, at, "The keyword %q is not supported; dropping it would accept more than the description allows.", clip(keyword))
		}
		if refused != nil {
			return nil, refused
		}
		out[keyword] = value
	}
	if _, stated := out["additionalProperties"]; !stated && hasType(out["type"], "object") {
		out["additionalProperties"] = true
		s.assumed = append(s.assumed, Assumed{Operation: s.operation, Field: field + ".additionalProperties",
			Value: true, Note: "set false to refuse unknown fields"})
	}
	return out, nil
}

func (s *schemas) properties(value any, pointer, field string, depth int, refs []string) (any, *refusal) {
	members, ok := value.(map[string]any)
	if !ok {
		return value, nil // the compiler refuses it
	}
	out := make(map[string]any, len(members))
	for _, name := range sortedKeys(members) {
		child, refused := s.convert(members[name], pointer+"/"+escape(name), field+"."+name, depth+1, refs)
		if refused != nil {
			return nil, refused
		}
		out[name] = child
	}
	return out, nil
}

// reference inlines an internal reference. Annotation siblings are removed;
// any other sibling is refused, since 3.0 ignores siblings and 3.1 applies them.
func (s *schemas) reference(object map[string]any, ref any, pointer, field string, depth int, refs []string) (any, *refusal) {
	for _, keyword := range sortedKeys(object) {
		switch {
		case keyword == "$ref":
		case isAnnotation(keyword):
			s.annotations = append(s.annotations, Removed{Operation: s.operation, Field: pointer + "/" + escape(keyword)})
		default:
			return nil, refuse(CodeKeyword, pointer+"/"+escape(keyword), "The keyword %q beside $ref is not supported.", clip(keyword))
		}
	}
	target, refused := resolve(s.root, ref, pointer)
	if refused != nil {
		return nil, refused
	}
	name := ref.(string)
	if slices.Contains(refs, name) {
		return nil, refuse(CodeCycle, pointer, "The schema refers to itself through %s.", clip(name))
	}
	return s.convert(target, name, field, depth, append(refs[:len(refs):len(refs)], name))
}

// resolve finds the target of an internal reference ("#/…"). References to
// other files or URLs are refused and never fetched.
func resolve(root map[string]any, ref any, pointer string) (any, *refusal) {
	name, ok := ref.(string)
	if !ok {
		return nil, refuse(CodeRefInvalid, pointer, "$ref is not a string.")
	}
	path, internal := strings.CutPrefix(name, "#/")
	if !internal {
		return nil, refuse(CodeExternalRef, pointer, "The reference %s points outside this file; nothing is fetched.", clip(name))
	}
	var node any = root
	for _, part := range strings.Split(path, "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch container := node.(type) {
		case map[string]any:
			node, ok = container[part]
		case []any:
			index, err := strconv.Atoi(part)
			ok = err == nil && index >= 0 && index < len(container)
			if ok {
				node = container[index]
			}
		default:
			ok = false
		}
		if !ok {
			return nil, refuse(CodeRefInvalid, pointer, "The reference %s does not exist in this file.", clip(name))
		}
	}
	return node, nil
}

func hasType(value any, name string) bool {
	switch t := value.(type) {
	case string:
		return t == name
	case []any:
		return slices.Contains(t, any(name))
	}
	return false
}

// count is the number of JSON values in a decoded value.
func count(value any) int {
	n := 1
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			n += count(item)
		}
	case map[string]any:
		for _, item := range v {
			n += count(item)
		}
	}
	return n
}

// escape encodes one JSON Pointer segment (RFC 6901).
func escape(segment string) string {
	return strings.ReplaceAll(strings.ReplaceAll(segment, "~", "~0"), "/", "~1")
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
