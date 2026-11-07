// Limits and identifier formats shared by policy decoding, compilation and the store.

package policy

const (
	MaxJSONDepth        = 16
	MaxJSONValues       = 4096
	MaxNumberLength     = 32
	maxExponentDigits   = 3
	MaxSchemaProperties = 64
	MaxEnumValues       = 64
	MaxSchemaLength     = 1 << 20
	MaxHeaderName       = 64
)

// IsSubjectID accepts 1 to 128 printable ASCII characters without spaces. The
// same rule applies to the "sub" claim, so a subject identifier is always
// safe to log and to forward in a header.
func IsSubjectID(id string) bool {
	if id == "" || len(id) > MaxSubjectLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// CheckID accepts short identifiers made of letters, digits, '.', '_' and
// '-', starting with a letter or digit, so identifiers are safe to log and to
// compare byte for byte.
func CheckID(id, field string, args ...any) error {
	if id == "" || len(id) > MaxIDLength {
		return Invalid("INVALID_VALUE", field, args...)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if alphanumeric {
			continue
		}
		if i == 0 || (c != '.' && c != '_' && c != '-') {
			return Invalid("INVALID_VALUE", field, args...)
		}
	}
	return nil
}

// Clip shortens untrusted text to 256 bytes, without splitting a UTF-8
// sequence, before it is echoed in a report or an error.
func Clip(text string) string {
	const limit = 256
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && text[cut]&0xc0 == 0x80 {
		cut--
	}
	return text[:cut] + "…"
}
