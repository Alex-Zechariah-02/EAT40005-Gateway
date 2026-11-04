// Trusted issuer configuration and verification key loading.

package gateway

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const (
	maxTokenBytes       = 4096
	maxKeyFileBytes     = 64 << 10
	maxTrustValue       = 256
	minRSABits          = 2048
	maxRSABits          = 8192
	maxVerificationKeys = 64
	// maxNumericDate is 9999-12-31T23:59:59Z, so every accepted time claim
	// converts to a time.Time without overflow.
	maxNumericDate = 253402300799
)

// TrustConfig names the trusted issuer, audience and locally configured public
// keys. Token headers can select only a key already in this set.
type TrustConfig struct {
	Issuer   string            `json:"issuer"`
	Audience string            `json:"audience"`
	Keys     []VerificationKey `json:"keys"`
}

type VerificationKey struct {
	Kid           string `json:"kid"`
	PublicKeyFile string `json:"public_key_file"`
}

type keyInfo struct {
	Kid         string    `json:"kid"`
	Fingerprint string    `json:"fingerprint"`
	LoadedAt    time.Time `json:"loaded_at"`
}

// A verifier is an immutable whole key set. Replacing one never mutates keys
// that a request has already captured.
type verifier struct {
	issuer      string
	audience    string
	keys        map[string]*rsa.PublicKey
	info        []keyInfo
	fingerprint string // SHA-256 of the complete named key set
	// uses counts verified tokens per key since the gateway started. The map
	// is fixed when the verifier is built; a reload carries a kept key's
	// counter over (adoptUses), so only the counters' atomics change.
	uses map[string]*keyUse
}

// keyUse counts the tokens one key verified and when it last did. It is read
// by the keys report and written by verify with atomics only, so counting
// adds no lock to a request.
type keyUse struct {
	verified atomic.Uint64
	lastNano atomic.Int64
}

// adoptUses gives v the counters of the keys it shares with previous, before
// v is published, so a reload keeps their counts.
func (v *verifier) adoptUses(previous *verifier) {
	for kid := range v.uses {
		if use := previous.uses[kid]; use != nil {
			v.uses[kid] = use
		}
	}
}

// loadVerifier reads and validates the trust configuration. The key file must
// hold exactly one PEM "PUBLIC KEY" block with an RSA key, so a private key or
// any other material is refused.
func loadVerifier(c TrustConfig) (*verifier, error) {
	if !isTrustValue(c.Issuer) || !isTrustValue(c.Audience) {
		return nil, fmt.Errorf("issuer and audience must each be 1 to %d printable ASCII characters", maxTrustValue)
	}
	if len(c.Keys) == 0 || len(c.Keys) > maxVerificationKeys {
		return nil, fmt.Errorf("keys must contain 1 to %d named public keys", maxVerificationKeys)
	}
	v := &verifier{issuer: c.Issuer, audience: c.Audience, keys: map[string]*rsa.PublicKey{}, info: []keyInfo{}, uses: map[string]*keyUse{}}
	loaded := time.Now().UTC()
	for _, entry := range c.Keys {
		if checkID(entry.Kid, "kid") != nil || v.keys[entry.Kid] != nil {
			return nil, errors.New("key IDs must be valid and distinct")
		}
		key, fingerprint, err := readVerificationKey(entry.PublicKeyFile)
		if err != nil {
			return nil, err
		}
		v.keys[entry.Kid] = key
		v.uses[entry.Kid] = &keyUse{}
		v.info = append(v.info, keyInfo{Kid: entry.Kid, Fingerprint: "sha256:" + fingerprint, LoadedAt: loaded})
	}
	slices.SortFunc(v.info, func(a, b keyInfo) int { return strings.Compare(a.Kid, b.Kid) })
	hash := sha256.New()
	for _, info := range v.info {
		fmt.Fprintf(hash, "%s:%s\n", info.Kid, info.Fingerprint)
	}
	v.fingerprint = hex.EncodeToString(hash.Sum(nil))
	return v, nil
}

func readVerificationKey(path string) (*rsa.PublicKey, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("opening public key: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading public key: %w", err)
	}
	if len(data) > maxKeyFileBytes {
		return nil, "", errors.New("public key file is too large")
	}

	// pem.Decode skips text before the first block, so leading data is
	// checked separately.
	block, rest := pem.Decode(data)
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN ")) ||
		block == nil ||
		block.Type != "PUBLIC KEY" ||
		len(block.Headers) != 0 ||
		len(bytes.TrimSpace(rest)) != 0 {
		return nil, "", errors.New("public key file must hold exactly one PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parsing public key: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, "", errors.New("public key is not an RSA key")
	}
	if bits := key.N.BitLen(); bits < minRSABits || bits > maxRSABits {
		return nil, "", fmt.Errorf("RSA key has %d bits, want %d to %d", bits, minRSABits, maxRSABits)
	}

	sum := sha256.Sum256(block.Bytes)
	return key, hex.EncodeToString(sum[:]), nil
}

func isTrustValue(value string) bool {
	if value == "" || len(value) > maxTrustValue {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
