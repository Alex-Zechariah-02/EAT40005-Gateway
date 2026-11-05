// Package fault holds the fault-injection points that tests use to crash,
// delay or fail the gateway at exact places.
//
// The points act only in binaries built with the faultinject tag and are
// chosen by environment variables; without the tag every point is a no-op,
// so a shipped binary has no way to trigger one. The package imports no other
// internal package. Any gateway package may call it; nothing outside tests
// may depend on a point having an effect.
package fault
