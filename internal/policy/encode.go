// Canonical encoding: one byte form per policy meaning, so two policies compare byte for byte.

package policy

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// methodOrder is the order the OpenAPI Specification lists methods in.
var methodOrder = []string{"GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH"}

// Encode returns the canonical form of p: 2-space JSON with members in
// struct order and a trailing newline; upstreams, APIs and roles sorted by
// id; operations by API, target and method; permits by role and operation.
// Schemas have their members sorted and each "required" list sorted. The
// input is not changed. It fails only on a schema that is not valid JSON.
func Encode(p Policy) ([]byte, error) {
	out := p
	out.Upstreams = sorted(p.Upstreams, func(a, b Upstream) int { return strings.Compare(a.ID, b.ID) })
	out.APIs = sorted(p.APIs, func(a, b API) int { return strings.Compare(a.ID, b.ID) })
	out.Roles = sorted(p.Roles, func(a, b Role) int { return strings.Compare(a.ID, b.ID) })
	out.Permits = sorted(p.Permits, func(a, b Permit) int {
		return cmp.Or(strings.Compare(a.Role, b.Role), strings.Compare(a.Operation, b.Operation))
	})
	out.Operations = make([]Operation, len(p.Operations))
	for i, op := range p.Operations {
		var err error
		if op.Headers, err = canonicalSchema(op.Headers); err != nil {
			return nil, err
		}
		if op.Body != nil {
			body := *op.Body
			if body.Schema, err = canonicalSchema(body.Schema); err != nil {
				return nil, err
			}
			op.Body = &body
		}
		out.Operations[i] = op
	}
	slices.SortFunc(out.Operations, func(a, b Operation) int {
		return cmp.Or(strings.Compare(a.API, b.API), strings.Compare(a.Target, b.Target),
			cmp.Compare(methodRank(a.Method), methodRank(b.Method)), strings.Compare(a.ID, b.ID))
	})
	document, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(document, '\n'), nil
}

// sorted returns a sorted copy that is empty rather than nil, so an empty
// list encodes as [] and never as null.
func sorted[T any](list []T, order func(a, b T) int) []T {
	out := append(make([]T, 0, len(list)), list...)
	slices.SortFunc(out, order)
	return out
}

// methodRank places unknown methods last, so sorting never fails.
func methodRank(method string) int {
	if i := slices.Index(methodOrder, method); i >= 0 {
		return i
	}
	return len(methodOrder)
}

// canonicalSchema re-encodes a schema with sorted members and sorted
// "required" lists; both orders carry no meaning in JSON Schema.
func canonicalSchema(raw json.RawMessage) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	value, reason := DecodeBounded(raw, MaxJSONDepth, MaxSchemaLength)
	if reason != "" {
		return nil, errors.New("schema not encodable: " + reason)
	}
	sortRequired(value)
	return json.Marshal(value)
}

// sortRequired walks schema positions only (properties and items), so a
// "required" member inside an enum value, which is data, keeps its order.
func sortRequired(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	if list, ok := schema["required"].([]any); ok && allStrings(list) {
		slices.SortFunc(list, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, property := range properties {
		sortRequired(property)
	}
	sortRequired(schema["items"])
}

func allStrings(list []any) bool {
	for _, item := range list {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}
