// Reading a description file into one bounded JSON value, whatever its format.

package openapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// Format is the syntax a description is written in.
type Format string

const (
	JSON Format = "json"
	YAML Format = "yaml"
)

// MaxSpecBytes is the largest description accepted, checked before parsing.
const MaxSpecBytes = 1 << 20

// FormatOf chooses the format from the file extension alone.
func FormatOf(path string) (Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return JSON, nil
	case ".yaml", ".yml":
		return YAML, nil
	}
	return "", errors.New("unsupported file type: use .json, .yaml or .yml")
}

// decodeSpec returns the description as maps, slices, json.Number, strings,
// booleans and nil. YAML is converted and then re-read through the same
// bounded JSON decoder, so both formats pass one gate: valid UTF-8, unique
// member names, bounded numbers and nesting at most policy.MaxJSONDepth.
func decodeSpec(data []byte, format Format) (any, error) {
	if len(data) > MaxSpecBytes {
		return nil, errors.New("the description is larger than 1 MiB")
	}
	if format == YAML {
		value, err := readYAML(data)
		if err != nil {
			return nil, err
		}
		if data, err = json.Marshal(value); err != nil {
			return nil, fmt.Errorf("the description cannot be converted to JSON: %w", err)
		}
	}
	// Every value takes at least one byte, so the size limit bounds the
	// value count; each schema is held to policy.MaxJSONValues later.
	value, reason := policy.DecodeBounded(data, policy.MaxJSONDepth, MaxSpecBytes)
	if reason != "" {
		return nil, fmt.Errorf("the description is not accepted: %s", reason)
	}
	return value, nil
}

// clip shortens text taken from the description before it is echoed.
func clip(text string) string {
	const limit = 256
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && text[cut]&0xc0 == 0x80 {
		cut-- // do not split a UTF-8 sequence
	}
	return text[:cut] + "…"
}
