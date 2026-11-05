// Bounded JSON decoding.

package policy

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DecodeJSON decodes exactly one JSON value into the form the schema library
// validates (maps, slices, json.Number, strings, booleans and nil). It
// rejects invalid UTF-8, repeated member names, data after the value, nesting
// deeper than MaxJSONDepth, more than MaxJSONValues values and number
// literals the library could not compare cheaply. encoding/json alone would
// keep the last repeated member and replace invalid UTF-8, so the gateway and
// an upstream could read different documents. It returns the value, or the
// rejection reason.
func DecodeJSON(data []byte) (any, string) {
	return DecodeBounded(data, MaxJSONDepth, MaxJSONValues)
}

// DecodeBounded is DecodeJSON with the caller's nesting and value limits.
func DecodeBounded(data []byte, maxDepth, maxValues int) (any, string) {
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
				if values > maxValues || len(stack) == maxDepth {
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
			if values > maxValues || !isBoundedNumber(t) {
				return nil, "limit_exceeded"
			}
			value = t
		default:
			values++
			if values > maxValues {
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
	if len(literal) > MaxNumberLength {
		return false
	}
	_, exponent, found := strings.Cut(strings.ToLower(literal), "e")
	if !found {
		return true
	}
	exponent = strings.TrimLeft(exponent, "+-")
	return len(exponent) <= maxExponentDigits
}
