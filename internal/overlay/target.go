// Decision targets: RFC 9535 normalized paths into operations or ledger
// items, and two fixed filters, read without a JSONPath library.

package overlay

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

// The two filter forms, each followed by a single-quoted literal and "]".
const (
	operationFilter = "$.paths[*][?@.operationId=="
	itemFilter      = "$['" + exchange.Member + "'].items[?@.id=="
)

// methods are the path item members that hold operations.
var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// selector is what a target selects by.
type selector int

const (
	byPath      selector = iota // a normalized path
	byOperation                 // operations by operationId
	byItem                      // ledger items by id
)

// target is a parsed target: a normalized path (segments of string or int)
// or a filter value.
type target struct {
	by    selector
	path  []any
	value string
}

var errTarget = errors.New("the target must be a normalized path into an operation or a ledger item, " +
	"$.paths[*][?@.operationId=='<id>'] or $['" + exchange.Member + "'].items[?@.id=='<id>']")

func parseTarget(text string) (target, error) {
	for _, form := range []struct {
		prefix string
		by     selector
	}{{operationFilter, byOperation}, {itemFilter, byItem}} {
		rest, found := strings.CutPrefix(text, form.prefix)
		if !found {
			continue
		}
		value, rest, err := literal(rest)
		if err != nil || rest != "]" {
			return target{}, errTarget
		}
		return target{by: form.by, value: value}, nil
	}
	path, err := normalized(text)
	if err != nil || !allowedPath(path) {
		return target{}, errTarget
	}
	return target{by: byPath, path: path}, nil
}

// normalized reads $ followed by ['name'] and [index] segments (RFC 9535 §2.7).
func normalized(text string) ([]any, error) {
	rest, found := strings.CutPrefix(text, "$")
	if !found {
		return nil, errTarget
	}
	path := []any{}
	for rest != "" {
		inner, found := strings.CutPrefix(rest, "[")
		if !found {
			return nil, errTarget
		}
		if strings.HasPrefix(inner, "'") {
			name, after, err := literal(inner)
			if err != nil || !strings.HasPrefix(after, "]") {
				return nil, errTarget
			}
			path, rest = append(path, name), after[1:]
			continue
		}
		digits, after, found := strings.Cut(inner, "]")
		index, err := strconv.Atoi(digits)
		if !found || err != nil || index < 0 || strconv.Itoa(index) != digits {
			return nil, errTarget
		}
		path, rest = append(path, index), after
	}
	return path, nil
}

// allowedPath admits a path into an operation, or exactly one ledger item.
func allowedPath(path []any) bool {
	if len(path) >= 3 && path[0] == "paths" {
		method, isName := path[2].(string)
		_, pathName := path[1].(string)
		return pathName && isName && slices.Contains(methods, method)
	}
	if len(path) == 3 && path[0] == exchange.Member && path[1] == "items" {
		_, isIndex := path[2].(int)
		return isIndex
	}
	return false
}

// literal reads a single-quoted RFC 9535 string at the start of text and
// returns its value and the text after it. The escapes are JSON's, plus \'.
func literal(text string) (string, string, error) {
	if !strings.HasPrefix(text, "'") {
		return "", "", errTarget
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 1; i < len(text); i++ {
		switch c := text[i]; {
		case c == '\'':
			var value string
			if err := json.Unmarshal([]byte(b.String()+`"`), &value); err != nil {
				return "", "", errTarget
			}
			return value, text[i+1:], nil
		case c == '"':
			b.WriteString(`\"`)
		case c != '\\':
			b.WriteByte(c)
		case i+1 < len(text) && text[i+1] == '\'':
			b.WriteByte('\'')
			i++
		case i+1 < len(text) && strings.IndexByte(`bfnrt/\u`, text[i+1]) >= 0:
			b.WriteString(text[i : i+2])
			i++
		default:
			return "", "", errTarget
		}
	}
	return "", "", errTarget
}

// selectNodes returns the location of every node the target selects.
func (t target) selectNodes(document map[string]any) [][]any {
	switch t.by {
	case byPath:
		if _, found := at(document, t.path); found {
			return [][]any{t.path}
		}
		return nil
	case byItem:
		ledger, _ := document[exchange.Member].(map[string]any)
		items, _ := ledger["items"].([]any)
		var found [][]any
		for i, value := range items {
			if object, _ := value.(map[string]any); object != nil && object["id"] == t.value {
				found = append(found, []any{exchange.Member, "items", i})
			}
		}
		return found
	}
	paths, _ := document["paths"].(map[string]any)
	var found [][]any
	for _, name := range sortedKeys(paths) {
		pathItem, _ := paths[name].(map[string]any)
		for _, member := range sortedKeys(pathItem) {
			if object, _ := pathItem[member].(map[string]any); object != nil && object["operationId"] == t.value {
				found = append(found, []any{"paths", name, member})
			}
		}
	}
	return found
}

// at returns the node at a location, and whether it exists.
func at(node any, path []any) (any, bool) {
	for _, segment := range path {
		switch v := node.(type) {
		case map[string]any:
			name, isName := segment.(string)
			child, present := v[name]
			if !isName || !present {
				return nil, false
			}
			node = child
		case []any:
			index, isIndex := segment.(int)
			if !isIndex || index >= len(v) {
				return nil, false
			}
			node = v[index]
		default:
			return nil, false
		}
	}
	return node, true
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
