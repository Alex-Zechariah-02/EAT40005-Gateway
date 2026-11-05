// Limits and identifier formats shared by policy decoding, compilation and the store.

package gateway

const (
	maxJSONDepth        = 16
	maxJSONValues       = 4096
	maxNumberLength     = 32
	maxExponentDigits   = 3
	maxSchemaProperties = 64
	maxEnumValues       = 64
	maxSchemaLength     = 1 << 20
	maxHeaderName       = 64
)

// isSubjectID accepts 1 to 128 printable ASCII characters without spaces. The
// same rule applies to the "sub" claim, so a subject identifier is always
// safe to log and to forward in a header.
func isSubjectID(id string) bool {
	if id == "" || len(id) > maxSubjectLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// checkID accepts short identifiers made of letters, digits, '.', '_' and
// '-', starting with a letter or digit, so identifiers are safe to log and to
// compare byte for byte.
func checkID(id, field string, args ...any) error {
	if id == "" || len(id) > maxIDLength {
		return invalid("INVALID_VALUE", field, args...)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if alphanumeric {
			continue
		}
		if i == 0 || (c != '.' && c != '_' && c != '-') {
			return invalid("INVALID_VALUE", field, args...)
		}
	}
	return nil
}
