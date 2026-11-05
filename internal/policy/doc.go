// Package policy defines the policy document an administrator writes: its
// types, its strict JSON decoding and the limits and identifier formats
// every policy must meet.
//
// It decodes and checks the document's shape only; deciding what a policy
// means is the compiler's job, and the package never reads storage, the
// network or the clock. It imports no other internal package, so any
// gateway package may use it.
package policy
