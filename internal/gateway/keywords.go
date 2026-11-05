// JSON Schema keyword checks.

package gateway

import (
	"encoding/json"
	"math/big"
	"slices"
	"strconv"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func checkArrayKeywords(schema map[string]any, field string) error {
	value, present := schema["items"]
	if !present {
		return policy.Invalid("INVALID_VALUE", "%s.items", field)
	}
	items, ok := value.(map[string]any)
	if !ok {
		return policy.Invalid("UNSUPPORTED_FEATURE", "%s.items", field)
	}
	if err := checkSchema(items, field+".items"); err != nil {
		return err
	}
	lower, upper := int64(0), int64(policy.MaxJSONValues)
	for _, keyword := range []string{"minItems", "maxItems"} {
		value, present := schema[keyword]
		if !present {
			continue
		}
		number, _ := value.(json.Number)
		n, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || n < 0 || n > policy.MaxJSONValues {
			return policy.Invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
		if keyword == "minItems" {
			lower = n
		} else {
			upper = n
		}
	}
	if lower > upper {
		return policy.Invalid("INVALID_VALUE", "%s.minItems", field)
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
		return nil, policy.Invalid("INVALID_VALUE", "%s", field)
	}
	if len(names) == 0 {
		return nil, policy.Invalid("INVALID_VALUE", "%s", field)
	}
	types := map[string]bool{}
	for _, item := range names {
		name, ok := item.(string)
		if !ok || types[name] {
			return nil, policy.Invalid("INVALID_VALUE", "%s", field)
		}
		if !slices.Contains(schemaTypes, name) {
			return nil, policy.Invalid("UNSUPPORTED_FEATURE", "%s", field)
		}
		types[name] = true
	}
	return types, nil
}

func checkObjectKeywords(schema map[string]any, field string) error {
	if _, ok := schema["additionalProperties"].(bool); !ok {
		return policy.Invalid("INVALID_VALUE", "%s.additionalProperties", field)
	}

	properties := map[string]any{}
	if value, present := schema["properties"]; present {
		var ok bool
		properties, ok = value.(map[string]any)
		if !ok || len(properties) > policy.MaxSchemaProperties {
			return policy.Invalid("INVALID_VALUE", "%s.properties", field)
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
			return policy.Invalid("UNSUPPORTED_FEATURE", "%s", path)
		}
		if err := checkSchema(child, path); err != nil {
			return err
		}
	}

	if value, present := schema["required"]; present {
		required, ok := value.([]any)
		if !ok {
			return policy.Invalid("INVALID_VALUE", "%s.required", field)
		}
		seen := map[string]bool{}
		for _, item := range required {
			name, ok := item.(string)
			if !ok || seen[name] || properties[name] == nil {
				return policy.Invalid("INVALID_VALUE", "%s.required", field)
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
	if !ok || len(values) == 0 || len(values) > policy.MaxEnumValues {
		return policy.Invalid("INVALID_VALUE", "%s.enum", field)
	}
	for _, item := range values {
		switch item.(type) {
		case string, json.Number, bool, nil:
		default:
			return policy.Invalid("UNSUPPORTED_FEATURE", "%s.enum", field)
		}
	}
	return nil
}

func checkLengths(schema map[string]any, field string) error {
	lower, upper := int64(0), int64(policy.MaxSchemaLength)
	for _, keyword := range []string{"minLength", "maxLength"} {
		value, present := schema[keyword]
		if !present {
			continue
		}
		number, _ := value.(json.Number)
		n, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || n < 0 || n > policy.MaxSchemaLength {
			return policy.Invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
		if keyword == "minLength" {
			lower = n
		} else {
			upper = n
		}
	}
	if lower > upper {
		return policy.Invalid("INVALID_VALUE", "%s.minLength", field)
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
			return policy.Invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
		rat, ok := new(big.Rat).SetString(string(number))
		if !ok {
			return policy.Invalid("INVALID_VALUE", "%s.%s", field, keyword)
		}
		bounds[keyword] = rat
	}
	if bounds["minimum"] != nil && bounds["maximum"] != nil && bounds["minimum"].Cmp(bounds["maximum"]) > 0 {
		return policy.Invalid("INVALID_VALUE", "%s.minimum", field)
	}
	return nil
}
