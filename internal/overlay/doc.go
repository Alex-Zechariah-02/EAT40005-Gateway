// Package overlay reads and applies migration decisions files: OpenAPI
// Overlay 1.2.0 documents restricted to update actions whose targets are a
// normalized path into an operation or a ledger item, or one of two fixed
// filters (operations by operationId, ledger items by id).
//
// Parse checks the whole file before anything is applied, and only Parse
// makes Decisions. Apply works on a copy, in order; each action must select
// exactly one node, and one that selects none is reported stale. A decision
// may only make an operation's access, rate or body limit stricter, or
// accept a looser ledger item with direction looser in the same action, so
// no decision widens access unnoticed. It imports only exchange.
package overlay
