// Package semantics holds the meanings gateway settings share across
// vendors: path matching and transforms, identity, roles, rate algorithms
// and keys, schema dialects and failure modes. Each primitive has a decision
// rule (what the source gateway does with a request, for reference tests),
// its distance to the gateway's own primitive (class and direction; for
// rates the ceiling and guarantee) and the conversion of its parameters.
// Readers of vendor files map their settings onto these primitives, so the
// logic is written once. It imports only exchange.
package semantics
