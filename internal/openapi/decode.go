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

// Decode returns a file of at most limit bytes as maps, slices,
// json.Number, strings, booleans and nil. YAML is converted and then re-read
// through the same bounded JSON decoder, so both formats pass one gate: valid
// UTF-8, unique member names, bounded numbers and nesting at most
// policy.MaxJSONDepth. limit is a whole number of MiB.
func Decode(data []byte, format Format, limit int) (any, error) {
	if len(data) > limit {
		return nil, fmt.Errorf("the description is larger than %d MiB", limit>>20)
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
	value, reason := policy.DecodeBounded(data, policy.MaxJSONDepth, limit)
	if reason != "" {
		return nil, fmt.Errorf("the description is not accepted: %s", reason)
	}
	return value, nil
}
