// Strict reading, writing and checking of the ledger.

package exchange

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	itemKind  = regexp.MustCompile(`^[a-z_]+:.`)
)

// Split returns the document without its ledger, and the ledger, which has
// passed Check. The input is not changed. It refuses a document without a
// ledger, with an x-apiward-* root member other than the ledger and the
// API's settings, or with a ledger that has unknown members or breaks a
// rule.
func Split(root map[string]any) (map[string]any, *Ledger, error) {
	for name := range root {
		if strings.HasPrefix(name, "x-apiward-") && name != Member && !slices.Contains(Settings, name) {
			return nil, nil, fmt.Errorf("unknown root member %s", policy.Clip(name))
		}
	}
	object, isObject := root[Member].(map[string]any)
	if !isObject {
		return nil, nil, errors.New("not an exchange file: the root member " + Member + " is missing or not an object")
	}
	if err := checkNames(object, false); err != nil {
		return nil, nil, err
	}
	data, err := json.Marshal(object)
	if err != nil {
		return nil, nil, err
	}
	var ledger Ledger
	if err := policy.DecodeStrict(data, &ledger); err != nil {
		return nil, nil, fmt.Errorf("%s: %s", Member, policy.Clip(err.Error()))
	}
	if err := ledger.Check(); err != nil {
		return nil, nil, err
	}
	document := maps.Clone(root)
	delete(document, Member)
	return document, &ledger, nil
}

// Attach returns a copy of the document holding the ledger, which must pass Check.
func Attach(document map[string]any, ledger *Ledger) (map[string]any, error) {
	if err := ledger.Check(); err != nil {
		return nil, err
	}
	out := maps.Clone(document)
	out[Member] = *ledger
	return out, nil
}

// checkNames refuses member names outside [a-z0-9_], which encoding/json
// would match case-insensitively; carried values are opaque and skipped.
func checkNames(value any, carried bool) error {
	switch v := value.(type) {
	case map[string]any:
		for name, child := range v {
			if !lowerName(name) {
				return fmt.Errorf("%s: unknown member %q", Member, policy.Clip(name))
			}
			if carried && name == "value" {
				continue
			}
			if err := checkNames(child, name == "carried"); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := checkNames(item, carried); err != nil {
				return err
			}
		}
	}
	return nil
}

func lowerName(name string) bool {
	return name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

// Check enforces the ledger rules: known format and version, one item per
// identifier prefixed by the format, a direction exactly when approximated,
// a target exactly when expressible, acceptance only of a looser
// approximation, and bounded carried values.
func (l *Ledger) Check() error {
	if l.Version != Version {
		return fmt.Errorf("%s: version must be %d", Member, Version)
	}
	if !slices.Contains(Formats, l.Source.Format) {
		return fmt.Errorf("%s: unknown source format %q", Member, policy.Clip(l.Source.Format))
	}
	if !text(l.Source.FormatVersion) || !sha256Hex.MatchString(l.Source.SHA256) {
		return fmt.Errorf("%s: source needs a format_version and a lowercase hex sha256", Member)
	}
	if l.Items == nil || l.Carried == nil || l.CredentialsSkipped == nil || l.Trust.Issuers == nil {
		return fmt.Errorf("%s: items, carried, credentials_skipped and trust.issuers must be lists", Member)
	}
	seen := map[string]bool{}
	for i, item := range l.Items {
		if err := item.check(l.Source.Format); err != nil {
			return fmt.Errorf("%s: items[%d]: %w", Member, i, err)
		}
		if seen[item.ID] {
			return fmt.Errorf("%s: items[%d]: identifier %q used twice", Member, i, policy.Clip(item.ID))
		}
		seen[item.ID] = true
	}
	if err := l.checkCarried(); err != nil {
		return err
	}
	return l.checkRest()
}

func (item Item) check(format string) error {
	kind, prefixed := strings.CutPrefix(item.ID, format+":")
	switch {
	case !prefixed || !itemKind.MatchString(kind) || len(item.ID) > maxText:
		return fmt.Errorf("identifier %q is not %s:<kind>:<name>", policy.Clip(item.ID), format)
	case !slices.Contains(Facets, item.Facet):
		return fmt.Errorf("unknown facet %q", policy.Clip(item.Facet))
	case !slices.Contains([]Class{Exact, Equivalent, Approximated, NotExpressible}, item.Class):
		return fmt.Errorf("unknown class %q", policy.Clip(string(item.Class)))
	case (item.Class == Approximated) != (item.Direction != ""):
		return errors.New("direction is given exactly when the class is approximated")
	case item.Direction != "" && !slices.Contains([]Direction{Stricter, Looser, Both}, item.Direction):
		return fmt.Errorf("unknown direction %q", policy.Clip(string(item.Direction)))
	case item.Accepted && item.Direction != Looser:
		return errors.New("accepted is true only on a looser approximation")
	case !pointer(item.Source):
		return errors.New("source must be a JSON pointer starting with #")
	case (item.Class == NotExpressible) != (item.Target == ""):
		return errors.New("target is empty exactly when the class is not_expressible")
	case item.Target != "" && !pointer(item.Target):
		return errors.New("target must be a JSON pointer starting with #")
	case len(item.Note) > maxText:
		return errors.New("note is too long")
	}
	return nil
}

func (l *Ledger) checkCarried() error {
	total := 0
	for i, c := range l.Carried {
		if !slices.Contains(Formats, c.Vendor) || !text(c.Scope) || !text(c.Path) || len(c.Value) == 0 {
			return fmt.Errorf("%s: carried[%d] needs a known vendor, a scope, a path and a value", Member, i)
		}
		if len(c.Value) > MaxCarriedValue {
			return fmt.Errorf("%s: carried[%d] is larger than %d bytes", Member, i, MaxCarriedValue)
		}
		if total += len(c.Value); total > MaxCarriedTotal {
			return fmt.Errorf("%s: carried values are larger than %d bytes together", Member, MaxCarriedTotal)
		}
	}
	return nil
}

func (l *Ledger) checkRest() error {
	for i, s := range l.CredentialsSkipped {
		if !text(s.Type) || s.Count < 1 {
			return fmt.Errorf("%s: credentials_skipped[%d] needs a type and a count of at least 1", Member, i)
		}
	}
	for i, issuer := range l.Trust.Issuers {
		if !text(issuer.Issuer) || issuer.Audience != nil && !text(*issuer.Audience) || issuer.PublicKeyIDs == nil {
			return fmt.Errorf("%s: trust.issuers[%d] needs an issuer, a non-empty audience or null, and public_key_ids", Member, i)
		}
		if err := checkKeys(issuer); err != nil {
			return fmt.Errorf("%s: trust.issuers[%d]: %w", Member, i, err)
		}
	}
	for i, b := range l.Blocked {
		if !text(b.Provider) || !text(b.Subject) {
			return fmt.Errorf("%s: blocked[%d] needs a provider and a subject", Member, i)
		}
	}
	return nil
}

// checkKeys accepts public keys only as PEM RSA public keys, one per key
// identifier, so a private key can never be carried as trust.
func checkKeys(issuer Issuer) error {
	if issuer.PublicKeys == nil {
		return nil
	}
	if len(issuer.PublicKeys) != len(issuer.PublicKeyIDs) {
		return errors.New("public_keys needs one key per public_key_ids entry")
	}
	for _, key := range issuer.PublicKeys {
		if _, err := RSAPublicKey(key); err != nil {
			return err
		}
	}
	return nil
}

// RSAPublicKey parses one PEM "PUBLIC KEY" block holding an RSA key and
// nothing else; private keys and other text are refused.
func RSAPublicKey(text string) (*rsa.PublicKey, error) {
	refused := errors.New("public_keys holds only PEM RSA public keys")
	if len(text) > maxText {
		return nil, refused
	}
	block, rest := pem.Decode([]byte(text))
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, refused
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, refused
	}
	public, isRSA := key.(*rsa.PublicKey)
	if !isRSA {
		return nil, refused
	}
	return public, nil
}

func text(value string) bool { return value != "" && len(value) <= maxText }

func pointer(value string) bool {
	return len(value) <= maxText && (value == "#" || strings.HasPrefix(value, "#/"))
}
