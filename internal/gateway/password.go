// Administrator password hashing and checks.

package gateway

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password hashing uses argon2id with the OWASP minimum configuration. Only
// hashes with exactly these parameters are accepted, so a stored value can
// never make the gateway compute an attacker-chosen cost.
const (
	argonMemory      = 19 * 1024 // KiB
	argonTime        = 2
	argonThreads     = 1
	argonSaltBytes   = 16
	argonKeyBytes    = 32
	minPasswordBytes = 15
	maxPasswordBytes = 256
)

var argonPrefix = fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$", argon2.Version, argonMemory, argonTime, argonThreads)

func validPassword(password string) bool {
	return len(password) >= minPasswordBytes && len(password) <= maxPasswordBytes && utf8.ValidString(password)
}

func hashPassword(password string) string {
	salt := make([]byte, argonSaltBytes)
	rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyBytes)
	encoding := base64.RawStdEncoding
	return argonPrefix + encoding.EncodeToString(salt) + "$" + encoding.EncodeToString(key)
}

func verifyPassword(encoded, password string) bool {
	rest, ok := strings.CutPrefix(encoded, argonPrefix)
	if !ok {
		return false
	}
	saltText, keyText, ok := strings.Cut(rest, "$")
	if !ok {
		return false
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(saltText)
	key, keyErr := base64.RawStdEncoding.DecodeString(keyText)
	if saltErr != nil || keyErr != nil || len(salt) != argonSaltBytes || len(key) != argonKeyBytes {
		return false
	}
	computed := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyBytes)
	return subtle.ConstantTimeCompare(computed, key) == 1
}

// dummyHash is verified against when the named administrator does not exist,
// so the response time does not reveal which names exist.
var dummyHash = sync.OnceValue(func() string { return hashPassword(newToken()) })
