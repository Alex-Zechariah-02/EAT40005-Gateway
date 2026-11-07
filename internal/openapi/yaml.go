// YAML to JSON value conversion: refuses what JSON cannot express; YAML 1.2 core-schema scalars.

package openapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	yaml "go.yaml.in/yaml/v4"
)

// YAML 1.2.2 core schema (section 10.3.2), applied to plain scalars only.
var (
	coreNull    = regexp.MustCompile(`^(null|Null|NULL|~|)$`)
	coreTrue    = regexp.MustCompile(`^(true|True|TRUE)$`)
	coreFalse   = regexp.MustCompile(`^(false|False|FALSE)$`)
	coreDecimal = regexp.MustCompile(`^[-+]?[0-9]+$`)
	coreOctal   = regexp.MustCompile(`^0o[0-7]+$`)
	coreHex     = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
	coreFloat   = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
	coreSpecial = regexp.MustCompile(`^([-+]?\.(inf|Inf|INF)|\.(nan|NaN|NAN))$`)
	jsonNumber  = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?$`)
)

const quoted = yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle

// readYAML converts exactly one YAML document into the value a JSON file
// with the same content would give. Errors name the line.
func readYAML(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("the description is not valid UTF-8")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the description is empty")
		}
		return nil, fmt.Errorf("not valid YAML: %s", policy.Clip(err.Error()))
	}
	var next yaml.Node
	err := decoder.Decode(&next)
	if err == nil {
		return nil, fmt.Errorf("line %d: more than one document", firstLine(&next))
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("not valid YAML: %s", policy.Clip(err.Error()))
	}
	if len(document.Content) != 1 {
		return nil, errors.New("the description is empty")
	}
	return convert(document.Content[0], 1)
}

func firstLine(node *yaml.Node) int {
	if len(node.Content) > 0 {
		return node.Content[0].Line
	}
	return node.Line
}

// convert maps one node; depth counts the containers enclosing and including it.
func convert(node *yaml.Node, depth int) (any, error) {
	switch {
	case node.Anchor != "" || node.Kind == yaml.AliasNode:
		return nil, fmt.Errorf("line %d: anchors and aliases are not supported", node.Line)
	case node.Style&yaml.TaggedStyle != 0:
		return nil, fmt.Errorf("line %d: tags are not supported", node.Line)
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return scalar(node)
	case yaml.SequenceNode, yaml.MappingNode:
		if depth > policy.MaxJSONDepth {
			return nil, fmt.Errorf("line %d: nested deeper than %d", node.Line, policy.MaxJSONDepth)
		}
	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node", node.Line)
	}
	if node.Kind == yaml.SequenceNode {
		items := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			item, err := convert(child, depth+1)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	}
	members := make(map[string]any, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name, err := key(node.Content[i])
		if err != nil {
			return nil, err
		}
		if _, repeated := members[name]; repeated {
			return nil, fmt.Errorf("line %d: duplicate key %q", node.Content[i].Line, policy.Clip(name))
		}
		if members[name], err = convert(node.Content[i+1], depth+1); err != nil {
			return nil, err
		}
	}
	return members, nil
}

// key accepts only a scalar that is a string under the core schema.
func key(node *yaml.Node) (string, error) {
	if node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("line %d: key is not a string", node.Line)
	}
	if node.Tag == "!!merge" {
		return "", fmt.Errorf("line %d: merge keys are not supported", node.Line)
	}
	value, err := convert(node, 0)
	if err != nil {
		return "", err
	}
	name, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("line %d: key is not a string", node.Line)
	}
	return name, nil
}

// scalar resolves a scalar: quoted and block scalars are strings; plain ones
// follow the core schema, not the library's resolver (which reads 0777 as
// octal and 1_000 as an integer).
func scalar(node *yaml.Node) (any, error) {
	text := node.Value
	if node.Style&quoted != 0 {
		return text, nil
	}
	switch {
	case coreNull.MatchString(text):
		return nil, nil
	case coreTrue.MatchString(text):
		return true, nil
	case coreFalse.MatchString(text):
		return false, nil
	case coreSpecial.MatchString(text):
		return nil, fmt.Errorf("line %d: %s is not representable in JSON", node.Line, text)
	case len(text) > policy.MaxNumberLength && isNumber(text):
		// The same bound as JSON numbers, before any conversion could round.
		return nil, fmt.Errorf("line %d: number longer than %d characters", node.Line, policy.MaxNumberLength)
	case jsonNumber.MatchString(text):
		return json.Number(text), nil
	case coreDecimal.MatchString(text):
		return integer(strings.TrimPrefix(text, "+"), 10)
	case coreOctal.MatchString(text):
		return integer(text[2:], 8)
	case coreHex.MatchString(text):
		return integer(text[2:], 16)
	case coreFloat.MatchString(text):
		number, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: number out of range", node.Line)
		}
		return json.Number(strconv.FormatFloat(number, 'g', -1, 64)), nil
	}
	return text, nil
}

func isNumber(text string) bool {
	return coreDecimal.MatchString(text) || coreOctal.MatchString(text) || coreHex.MatchString(text) || coreFloat.MatchString(text)
}

// integer writes an integer literal in decimal, at any size; the JSON gate
// then bounds its length.
func integer(digits string, base int) (any, error) {
	number, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return nil, fmt.Errorf("invalid integer %q", policy.Clip(digits))
	}
	return json.Number(number.String()), nil
}

// Encode writes a description as 2-space JSON, or as block YAML
// made from that JSON text, so both formats hold the same values and the
// import reads either back alike.
func Encode(document map[string]any, format Format) ([]byte, error) {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	if format == JSON {
		return append(data, '\n'), nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	plain(&node)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// plain drops the JSON flow and quoting styles; the encoder still quotes
// every string that would otherwise read as another type.
func plain(node *yaml.Node) {
	node.Style = 0
	for _, child := range node.Content {
		plain(child)
	}
}
