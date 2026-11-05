// Package compile turns a policy document into an immutable generation: the
// route table, permits, rate rules and compiled body and header schemas that
// every request is decided against.
//
// A published generation is shared by concurrent requests, so the package
// exposes it only through read-only methods; nothing outside may change one.
// Compiling has no side effects: it never reads storage or the network, and
// it takes time only from the trusted clock it also owns. It imports only
// policy and fault. Any gateway package may call it.
package compile
