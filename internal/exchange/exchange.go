// Ledger types and the fixed vocabularies of the exchange file.

package exchange

import "encoding/json"

// Member is the root member that holds the ledger.
const Member = "x-apiward-migration"

// Version is the only ledger version this package reads and writes.
const Version = 1

// Bounds on carried vendor settings.
const (
	MaxCarriedValue = 64 << 10 // bytes of one carried value
	MaxCarriedTotal = 1 << 20  // bytes of all carried values together
	maxText         = 4096     // bytes of an identifier, pointer or note
)

// Formats are the source formats a ledger may name.
var Formats = []string{"openapi"}

// Class is how closely the gateway enforces a source setting.
type Class string

const (
	Exact          Class = "exact"
	Equivalent     Class = "equivalent"
	Approximated   Class = "approximated"
	NotExpressible Class = "not_expressible"
)

// Direction is how an approximation differs: present only when Approximated.
type Direction string

const (
	Stricter Direction = "stricter"
	Looser   Direction = "looser"
	Both     Direction = "both"
)

// Facets name what a setting is about.
var Facets = []string{"route", "access", "auth", "roles", "rate", "validation", "size",
	"upstream", "transform", "failure", "other"}

// Ledger is the root member x-apiward-migration. Every list is written,
// empty rather than null; Blocked only when blocked accounts were asked for.
type Ledger struct {
	Version            int       `json:"version"`
	Source             Source    `json:"source"`
	Items              []Item    `json:"items"`
	Carried            []Carried `json:"carried"`
	CredentialsSkipped []Skipped `json:"credentials_skipped"`
	Trust              Trust     `json:"trust"`
	Blocked            []Blocked `json:"blocked,omitempty"`
}

// Source names the file the ledger was read from.
type Source struct {
	Format        string `json:"format"`
	FormatVersion string `json:"format_version"`
	SHA256        string `json:"sha256"`
}

// Item is one source setting with its class. Source and Target are JSON
// pointers into the source file and the exchange file; Target is empty only
// for a setting that is not expressible.
type Item struct {
	ID        string    `json:"id"`
	Facet     string    `json:"facet"`
	Class     Class     `json:"class"`
	Direction Direction `json:"direction,omitempty"`
	Source    string    `json:"source"`
	Target    string    `json:"target"`
	Note      string    `json:"note"`
	Accepted  bool      `json:"accepted"`
}

// Carried is a vendor setting kept as opaque data for the way back to that
// vendor; nothing on the policy path reads it.
type Carried struct {
	Vendor string          `json:"vendor"`
	Scope  string          `json:"scope"`
	Path   string          `json:"path"`
	Value  json.RawMessage `json:"value"`
}

// Skipped counts the credentials of one type that were never read.
type Skipped struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// Trust lists the token issuers the operator must configure.
type Trust struct {
	Issuers []Issuer `json:"issuers"`
}

// Issuer is one trusted issuer; Audience is null when the source sets none.
type Issuer struct {
	Issuer       string   `json:"issuer"`
	Audience     *string  `json:"audience"`
	PublicKeyIDs []string `json:"public_key_ids"`
}

// Blocked is one blocked account, written only when asked for.
type Blocked struct {
	Provider string `json:"provider"`
	Subject  string `json:"subject"`
}

// New returns an empty ledger for a source.
func New(source Source) *Ledger {
	return &Ledger{Version: Version, Source: source, Items: []Item{}, Carried: []Carried{},
		CredentialsSkipped: []Skipped{}, Trust: Trust{Issuers: []Issuer{}}}
}
