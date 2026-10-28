// Command testfixture runs one controlled API used by gateway regression
// tests (orders, inventory or reports) on a loopback address. Its receipts,
// state and reset are served on a second loopback address and require the
// secret read from -secret-file. It is not included in the Fetch
// demonstration stack.
package main
