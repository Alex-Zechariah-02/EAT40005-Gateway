// Package exchange is the migration exchange file: an OpenAPI 3.1 document
// whose root member x-apiward-migration is a ledger with one classified item
// per setting of the source configuration, the vendor settings carried for
// the way back, the credential types skipped and the identity facts the
// operator must configure.
//
// The ledger is read strictly: unknown or case-variant members, values
// outside the class rules and oversized carried values refuse the file.
// Split is the only way to obtain a ledger, and the document it returns no
// longer holds one, so carried vendor settings never reach the policy path.
// It imports only policy.
package exchange
